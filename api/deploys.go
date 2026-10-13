package api

// Stagehand: the /api/v1/deploys route group (D-10, D-12). It is registered
// only in mTLS mode (server.tls.client_ca set), so every caller has already
// presented an allow-listed Puppet certificate in the TLS handshake.
//
//	POST /api/v1/deploys       {"environments":[{"name":"production","expected_sha":"<40 hex>"}],"wait":true}
//	GET  /api/v1/deploys/{id}  the job record
//	GET  /api/v1/deploys?limit=N  {"deploys":[record, ...]} newest first, N in 1..100 (default 20)

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/voxpupuli/webhook-go/config"
	"github.com/voxpupuli/webhook-go/lib/helpers"
	"github.com/voxpupuli/webhook-go/lib/jobs"
)

const (
	maxDeployBody     = 64 << 10
	defaultListLimit  = 20
	maxListLimit      = 100
	memoryStoreLength = 200
)

// DeploysController serves the deploy API.
type DeploysController struct {
	runner *jobs.Runner
}

// NewDeploysController builds the controller from the loaded configuration:
// the engine and its argv come from upstream's r10k/g10k command helpers, so
// r10k.command_path, r10k.config_path and the r10k flags apply unchanged.
func NewDeploysController(conf config.Config) (*DeploysController, error) {
	var h helpers.Helper
	engine := jobs.R10kEngine(func(env string) []string {
		// The helper's second argument is upstream's inverted "no_mods" flag:
		// true deploys the environment's modules when r10k.deploy_modules is on.
		return h.GetR10kDeployEnvironmentCommand(env, true)
	})
	if conf.R10k.UseG10kCommands {
		engine = jobs.G10kEngine(func(env string) []string {
			return h.GetG10kDeployEnvironmentCommand(env, env)
		})
	}
	runner, err := jobs.NewRunner(jobs.Options{
		Engine:      engine,
		Basedir:     conf.Deploys.Basedir,
		Timeout:     conf.Deploys.Timeout,
		OutputLimit: conf.Deploys.OutputLimit,
		Store:       jobs.NewMemoryStore(memoryStoreLength),
	})
	if err != nil {
		return nil, err
	}
	return newDeploysController(runner), nil
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
	Wait         *bool             `json:"wait"`
}

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
	if req.Wait == nil || !*req.Wait {
		return "wait", "must be true; asynchronous deploys are not available yet"
	}
	return "", ""
}

// Create validates the request, deploys every environment and answers the
// normalised job record. A deploy that ran but did not deploy answers 200 with
// state "failed": the request worked, the deploy did not.
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
		unprocessable(c, "environments", err.Error())
		return
	}
	// The deploy is not tied to the request context: a client that hangs up
	// must not kill r10k half way through an environment. deploys.timeout
	// still bounds every invocation, and the record stays readable by id.
	job, err = d.runner.Run(context.Background(), job)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "recording deploy: " + err.Error(), "id": job.ID})
		return
	}
	c.JSON(http.StatusOK, job)
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
