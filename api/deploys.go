package api

// Stagehand: the /api/v1/deploys route group (D-10, D-12). It is registered
// only in mTLS mode (server.tls.client_ca set), so every caller has already
// presented an allow-listed Puppet certificate in the TLS handshake.
//
//	POST /api/v1/deploys       {"environments":[{"name":"production","expected_sha":"<40 hex>"}],"wait":false}
//	                           wait false: 202 with the queued record; poll GET by id (MH-01)
//	                           wait true:  200 with the finished record
//	GET  /api/v1/deploys/{id}  the job record
//	GET  /api/v1/deploys?limit=N  {"deploys":[record, ...]} newest first, N in 1..100 (default 20)
//
// Records are files under deploys.state_dir/jobs (lib/jobs FileStore), so
// they survive a restart.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/voxpupuli/webhook-go/config"
	"github.com/voxpupuli/webhook-go/lib/helpers"
	"github.com/voxpupuli/webhook-go/lib/jobs"
)

const (
	maxDeployBody    = 64 << 10
	defaultListLimit = 20
	maxListLimit     = jobs.MaxListLimit
)

// DeploysController serves the deploy API.
type DeploysController struct {
	runner *jobs.Runner
}

// NewDeploysController builds the controller from the loaded configuration:
// the engine and its argv come from upstream's r10k/g10k command helpers, so
// r10k.command_path, r10k.config_path, r10k.use_g10k_commands and the r10k
// flags apply unchanged (MH-06). Job records live in a FileStore under
// deploys.state_dir, deploys take <state_dir>/deploy.lock, and records a
// previous process left unfinished are failed here, once, before any route
// is registered. ctx is the server's base context: cancelling it stops
// running engines and fails queued jobs; Shutdown then waits for them.
func NewDeploysController(ctx context.Context, conf config.Config) (*DeploysController, error) {
	var h helpers.Helper
	engine := jobs.R10kEngine(func(env string) []string {
		// The helper's second argument is upstream's inverted "no_mods" flag:
		// true deploys the environment's modules when r10k.deploy_modules is on.
		return h.GetR10kDeployEnvironmentCommand(env, true)
	})
	if conf.R10k.UseG10kCommands {
		engine = jobs.G10kEngine(func(env string) []string {
			// branch == env selects -branch=<env>.
			return h.GetG10kDeployEnvironmentCommand(env, env)
		})
	}
	maxJobs := conf.Deploys.MaxJobs
	if maxJobs == 0 {
		maxJobs = jobs.DefaultMaxJobs
	}
	store, err := jobs.NewFileStore(conf.Deploys.StateDir, maxJobs)
	if err != nil {
		return nil, err
	}
	// Before any route accepts a request: a job a previous process left
	// queued or deploying is failed, so no client polls it forever.
	if n, err := store.Reconcile(); err != nil {
		store.Close()
		return nil, fmt.Errorf("reconciling deploy records: %w", err)
	} else if n > 0 {
		log.Warnf("deploy API: %d deploy(s) interrupted by a restart marked failed", n)
	}
	runner, err := jobs.NewRunner(jobs.Options{
		Engine:      engine,
		Basedir:     conf.Deploys.Basedir,
		Timeout:     conf.Deploys.Timeout,
		OutputLimit: conf.Deploys.OutputLimit,
		Store:       store,
		LockPath:    filepath.Join(conf.Deploys.StateDir, "deploy.lock"),
		Context:     ctx,
	})
	if err != nil {
		store.Close()
		return nil, err
	}
	return newDeploysController(runner), nil
}

// Shutdown stops the deploy runner: running engines are stopped, queued jobs
// fail, and it waits up to timeout for their records to be written.
func (d *DeploysController) Shutdown(timeout time.Duration) error {
	return d.runner.Shutdown(timeout)
}

func newDeploysController(runner *jobs.Runner) *DeploysController {
	return &DeploysController{runner: runner}
}

// Register adds the deploys routes under group (normally /api/v1).
func (d *DeploysController) Register(group *gin.RouterGroup) {
	g := group.Group("deploys")
	g.POST("", d.Create)
	g.GET("", d.List)
	g.GET("/:id", d.Get)
}

type deployRequest struct {
	Environments []jobs.EnvRequest `json:"environments"`
	// Wait is kept raw so that a missing or non-boolean value answers 422 on
	// the field rather than 400.
	Wait json.RawMessage `json:"wait"`
}

// wait reports the request's wait flag; validate has checked it.
func (req deployRequest) wait() bool { return string(req.Wait) == "true" }

func unprocessable(c *gin.Context, field, msg string) {
	c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": field + ": " + msg, "field": field})
}

// validate returns the first invalid field and why, or "" when req is valid.
func (req deployRequest) validate() (field, msg string) {
	if n := len(req.Environments); n == 0 || n > jobs.MaxEnvironments {
		return "environments", fmt.Sprintf("must name 1 to %d environments, got %d", jobs.MaxEnvironments, n)
	}
	seen := map[string]bool{}
	for i, e := range req.Environments {
		f := fmt.Sprintf("environments[%d]", i)
		if !jobs.ValidEnvironmentName(e.Name) {
			return f + ".name", "must match ^[a-z0-9_]+$"
		}
		if seen[e.Name] {
			return f + ".name", "environment " + e.Name + " is named twice"
		}
		seen[e.Name] = true
		if !jobs.ValidSHA(e.ExpectedSHA) {
			return f + ".expected_sha", "must be a 40-character lower-case hex commit SHA"
		}
	}
	if w := string(req.Wait); w != "true" && w != "false" {
		return "wait", "must be true (answer when finished) or false (answer 202 at once and poll)"
	}
	return "", ""
}

// Create validates the request and records a queued job. With wait false it
// answers 202 with that record at once and deploys in the background; the
// caller polls GET /api/v1/deploys/{id}. With wait true it deploys, then
// answers 200 with the finished record. A deploy that ran but did not deploy
// still answers 200 with state "failed": the request worked, the deploy did
// not.
func (d *DeploysController) Create(c *gin.Context) {
	var req deployRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxDeployBody)
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "request body is not valid JSON: " + err.Error()})
		return
	}
	if field, msg := req.validate(); field != "" {
		unprocessable(c, field, msg)
		return
	}

	job, err := d.runner.NewJob(req.Environments)
	if err != nil {
		if errors.Is(err, jobs.ErrInvalidRequest) {
			unprocessable(c, "environments", err.Error())
			return
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "recording deploy: " + err.Error()})
		return
	}

	if !req.wait() {
		if err := d.runner.Start(job); err != nil {
			d.refuse(c, job, err)
			return
		}
		c.Header("Location", "/api/v1/deploys/"+job.ID)
		c.JSON(http.StatusAccepted, job)
		return
	}

	job, err = d.runner.RunSync(job)
	if errors.Is(err, jobs.ErrBusy) || errors.Is(err, jobs.ErrShutdown) {
		d.refuse(c, job, err)
		return
	}
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "recording deploy: " + err.Error(), "id": job.ID})
		return
	}
	c.JSON(http.StatusOK, job)
}

// refuse answers 503 for a job the runner would not take, and records it as
// failed so its id never reads as queued.
func (d *DeploysController) refuse(c *gin.Context, job jobs.Job, err error) {
	job = jobs.FailUnstarted(job, err.Error())
	if serr := d.runner.Store().Save(job); serr != nil {
		log.Errorf("deploy %s: %v", job.ID, serr)
	}
	c.Header("Retry-After", "30")
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": err.Error(), "id": job.ID})
}

// Get answers one job record by id.
func (d *DeploysController) Get(c *gin.Context) {
	job, err := d.runner.Store().Get(c.Param("id"))
	if errors.Is(err, jobs.ErrNotFound) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "deploy not found"})
		return
	}
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, job)
}

// List answers the newest job records.
func (d *DeploysController) List(c *gin.Context) {
	limit := defaultListLimit
	if s, ok := c.GetQuery("limit"); ok {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxListLimit {
			unprocessable(c, "limit", fmt.Sprintf("must be an integer from 1 to %d", maxListLimit))
			return
		}
		limit = n
	}
	list, err := d.runner.Store().List(limit)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deploys": list})
}
