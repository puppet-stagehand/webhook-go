package jobs

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// MaxEnvironments bounds one deploy request.
const MaxEnvironments = 20

// Engine is the deploy tool a runner drives.
type Engine struct {
	// Name is "r10k" or "g10k"; it is reported in every job.
	Name string
	// MarkerFile is the marker the engine writes into each environment.
	MarkerFile string
	// Argv returns the full argv (command path first) that deploys one
	// environment. It is executed directly, never through a shell.
	Argv func(env string) []string
}

// R10kEngine drives r10k; argv is normally the upstream r10k command helper.
func R10kEngine(argv func(env string) []string) Engine {
	return Engine{Name: "r10k", MarkerFile: ".r10k-deploy.json", Argv: argv}
}

// G10kEngine drives g10k; argv is normally the upstream g10k command helper.
// Its fixture-driven tests arrive with plan 57.6-06.
func G10kEngine(argv func(env string) []string) Engine {
	return Engine{Name: "g10k", MarkerFile: ".g10k-deploy.json", Argv: argv}
}

// Options configure a Runner.
type Options struct {
	Engine      Engine
	Basedir     string        // where environments, and so markers, live
	Timeout     time.Duration // per engine invocation
	OutputLimit int           // bytes of combined output kept per environment
	Store       Store
}

// Runner runs deploy jobs one at a time.
type Runner struct {
	opts Options
	// mu serialises jobs inside this process so two requests never run the
	// engine at once. The cross-process flock arrives with plan 57.6-06.
	mu sync.Mutex
}

// NewRunner checks opts and returns a Runner.
func NewRunner(opts Options) (*Runner, error) {
	switch {
	case opts.Engine.Argv == nil || opts.Engine.Name == "" || opts.Engine.MarkerFile == "":
		return nil, errors.New("jobs: engine is not configured")
	case !filepath.IsAbs(opts.Basedir):
		return nil, fmt.Errorf("jobs: basedir must be absolute, got %q", opts.Basedir)
	case opts.Timeout <= 0:
		return nil, errors.New("jobs: timeout must be greater than zero")
	case opts.OutputLimit <= 0:
		return nil, errors.New("jobs: output limit must be greater than zero")
	case opts.Store == nil:
		return nil, errors.New("jobs: store is not configured")
	}
	return &Runner{opts: opts}, nil
}

// Store returns the runner's job store.
func (r *Runner) Store() Store { return r.opts.Store }

// Engine returns the name of the runner's engine.
func (r *Runner) Engine() string { return r.opts.Engine.Name }

// NewJob validates envs, records a queued job and returns it.
func (r *Runner) NewJob(envs []EnvRequest) (Job, error) {
	if len(envs) == 0 || len(envs) > MaxEnvironments {
		return Job{}, fmt.Errorf("a deploy names 1 to %d environments, got %d", MaxEnvironments, len(envs))
	}
	seen := map[string]bool{}
	job := Job{
		ID:        uuid.NewString(),
		Engine:    r.opts.Engine.Name,
		State:     StateQueued,
		CreatedAt: time.Now().UTC(),
	}
	for _, e := range envs {
		if !ValidEnvironmentName(e.Name) {
			return Job{}, fmt.Errorf("invalid environment name %q", e.Name)
		}
		if !ValidSHA(e.ExpectedSHA) {
			return Job{}, fmt.Errorf("invalid expected_sha for %s", e.Name)
		}
		if seen[e.Name] {
			return Job{}, fmt.Errorf("environment %s named twice", e.Name)
		}
		seen[e.Name] = true
		job.Results = append(job.Results, EnvResult{Environment: e.Name, State: StateQueued, ExpectedSHA: e.ExpectedSHA})
	}
	if err := r.opts.Store.Save(job); err != nil {
		return Job{}, err
	}
	return job, nil
}

// Run deploys every environment of job in order, one engine invocation each,
// saving progress to the store as it goes, and returns the finished job. The
// job is failed unless every environment was deployed.
func (r *Runner) Run(ctx context.Context, job Job) (Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	started := time.Now().UTC()
	job.StartedAt = &started
	job.State = StateDeploying
	if err := r.opts.Store.Save(job); err != nil {
		return job, err
	}

	all := true
	for i := range job.Results {
		job.Results[i].State = StateDeploying
		_ = r.opts.Store.Save(job)
		job.Results[i] = r.deployOne(ctx, job.Results[i])
		if job.Results[i].State != StateDeployed {
			all = false
		}
		if err := r.opts.Store.Save(job); err != nil {
			log.Errorf("deploy %s: saving progress: %v", job.ID, err)
		}
	}

	finished := time.Now().UTC()
	job.FinishedAt = &finished
	job.State = StateDeployed
	if !all {
		job.State = StateFailed
	}
	return job, r.opts.Store.Save(job)
}

func (r *Runner) deployOne(ctx context.Context, res EnvResult) EnvResult {
	// Defence in depth: the API validated these already.
	if !ValidEnvironmentName(res.Environment) || !ValidSHA(res.ExpectedSHA) {
		res.State, res.ExitCode, res.Error = StateFailed, -1, "invalid environment request"
		return res
	}

	argv := r.opts.Engine.Argv(res.Environment)
	if len(argv) == 0 {
		res.State, res.ExitCode, res.Error = StateFailed, -1, "engine command is empty"
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out := &tailBuffer{limit: r.opts.OutputLimit}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = 10 * time.Second

	res.StartedAt = time.Now().UTC()
	log.Infof("deploy %s with %s: %q", res.Environment, r.opts.Engine.Name, argv)
	runErr := cmd.Run()
	res.FinishedAt = time.Now().UTC()
	res.DurationMS = res.FinishedAt.Sub(res.StartedAt).Milliseconds()
	res.Output, res.OutputTruncated = out.String(), out.truncated

	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		res.ExitCode = 0
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		res.State, res.ExitCode, res.Error = StateFailed, -1, "engine did not start: "+runErr.Error()
		return res
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.State, res.Error = StateFailed, fmt.Sprintf("engine timed out after %s", r.opts.Timeout)
		return res
	}

	marker, err := ReadMarker(filepath.Join(r.opts.Basedir, res.Environment, r.opts.Engine.MarkerFile))
	if err != nil {
		res.State, res.Error = StateFailed, "deploy marker unreadable: "+err.Error()
		return res
	}
	if marker != nil {
		res.MarkerPresent = true
		res.MarkerDeploySuccess = marker.DeploySuccess != nil && *marker.DeploySuccess
	}
	res.State, res.DeployedSHA, res.Error = Normalize(res.ExitCode, marker, res.StartedAt, res.ExpectedSHA)
	return res
}

// tailBuffer keeps the last limit bytes written to it; the end of an engine's
// output is where its error is.
type tailBuffer struct {
	limit     int
	buf       []byte
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.limit; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
		b.truncated = true
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
