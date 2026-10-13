package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

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

// G10kEngine drives g10k; argv is normally the upstream g10k command helper
// ([command_path, -config=<cfg>, -branch=<env>, (-verbose)]). g10k is judged
// by its own marker, .g10k-deploy.json, by the same rule as r10k (D-29).
func G10kEngine(argv func(env string) []string) Engine {
	return Engine{Name: "g10k", MarkerFile: ".g10k-deploy.json", Argv: argv}
}

// DefaultMaxJobs is how many job records a FileStore keeps by default
// (deploys.max_jobs).
const DefaultMaxJobs = 500

// ErrInvalidRequest wraps every reason NewJob refuses a request.
var ErrInvalidRequest = errors.New("invalid deploy request")

// MaxPending bounds the jobs one runner holds at once, queued or running.
const MaxPending = 100

// ErrBusy is returned by Start when MaxPending jobs are already pending.
var ErrBusy = errors.New("too many deploys pending")

// ErrShutdown is returned by Start once the runner is shutting down.
var ErrShutdown = errors.New("the deploy service is shutting down")

// Options configure a Runner.
type Options struct {
	Engine      Engine
	Basedir     string        // where environments, and so markers, live
	Timeout     time.Duration // per engine invocation
	OutputLimit int           // bytes of combined output kept per environment
	Store       Store
	// Context is the runner's base context, normally the server's: when it
	// is cancelled, running engines are stopped and queued jobs fail. Nil
	// means context.Background().
	Context context.Context
}

// Runner runs deploy jobs one at a time.
type Runner struct {
	opts Options
	// mu serialises jobs inside this process so two requests never run the
	// engine at once. The cross-process flock arrives with plan 57.6-06.
	mu sync.Mutex

	ctx    context.Context // base context for every job
	cancel context.CancelFunc
	wg     sync.WaitGroup // background jobs

	pendMu  sync.Mutex
	pending int
	closed  bool
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
	base := opts.Context
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	return &Runner{opts: opts, ctx: ctx, cancel: cancel}, nil
}

// reserve counts one more pending job, or refuses it.
func (r *Runner) reserve() error {
	r.pendMu.Lock()
	defer r.pendMu.Unlock()
	switch {
	case r.closed || r.ctx.Err() != nil:
		return ErrShutdown
	case r.pending >= MaxPending:
		return ErrBusy
	}
	r.pending++
	return nil
}

func (r *Runner) release() {
	r.pendMu.Lock()
	r.pending--
	r.pendMu.Unlock()
}

// Start runs job in the background on the runner's base context and returns
// at once (MH-01). Its progress is read back from the store.
func (r *Runner) Start(job Job) error {
	if err := r.reserve(); err != nil {
		return err
	}
	// The goroutine owns its own copy: the caller still encodes job (the
	// 202 answer) while the runner rewrites its results.
	job = job.clone()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.release()
		if _, err := r.Run(r.ctx, job); err != nil {
			log.Errorf("deploy %s: %v", job.ID, err)
		}
	}()
	return nil
}

// RunSync runs job on the runner's base context and waits for it. It is not
// tied to any request: a client that hangs up must not stop an engine half way
// through an environment. deploys.timeout still bounds every invocation.
func (r *Runner) RunSync(job Job) (Job, error) {
	if err := r.reserve(); err != nil {
		return job, err
	}
	defer r.release()
	return r.Run(r.ctx, job)
}

// Shutdown stops accepting jobs, cancels the base context (stopping any
// running engine and failing queued jobs) and waits up to timeout for
// background jobs to record their outcome.
func (r *Runner) Shutdown(timeout time.Duration) error {
	r.pendMu.Lock()
	r.closed = true
	r.pendMu.Unlock()
	r.cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("jobs: background deploys still running after %s", timeout)
	}
}

// Store returns the runner's job store.
func (r *Runner) Store() Store { return r.opts.Store }

// Engine returns the name of the runner's engine.
func (r *Runner) Engine() string { return r.opts.Engine.Name }

// NewJob validates envs, records a queued job and returns it.
func (r *Runner) NewJob(envs []EnvRequest) (Job, error) {
	if len(envs) == 0 || len(envs) > MaxEnvironments {
		return Job{}, fmt.Errorf("%w: a deploy names 1 to %d environments, got %d", ErrInvalidRequest, MaxEnvironments, len(envs))
	}
	seen := map[string]bool{}
	job := Job{
		ID:        newJobID(),
		Engine:    r.opts.Engine.Name,
		State:     StateQueued,
		CreatedAt: time.Now().UTC(),
	}
	for _, e := range envs {
		if !ValidEnvironmentName(e.Name) {
			return Job{}, fmt.Errorf("%w: invalid environment name %q", ErrInvalidRequest, e.Name)
		}
		if !ValidSHA(e.ExpectedSHA) {
			return Job{}, fmt.Errorf("%w: invalid expected_sha for %s", ErrInvalidRequest, e.Name)
		}
		if seen[e.Name] {
			return Job{}, fmt.Errorf("%w: environment %s named twice", ErrInvalidRequest, e.Name)
		}
		seen[e.Name] = true
		job.Results = append(job.Results, EnvResult{Environment: e.Name, State: StateQueued, ExpectedSHA: e.ExpectedSHA})
	}
	if err := r.opts.Store.Save(job); err != nil {
		return Job{}, err
	}
	return job, nil
}

// FailUnstarted marks every unfinished environment of job, and the job, failed
// with reason. Nothing ran, so exit codes are -1.
func FailUnstarted(job Job, reason string) Job {
	now := time.Now().UTC()
	for i := range job.Results {
		if !job.Results[i].State.Terminal() {
			job.Results[i].State, job.Results[i].ExitCode, job.Results[i].Error = StateFailed, -1, reason
		}
	}
	job.State, job.FinishedAt = StateFailed, &now
	return job
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
		if ctx.Err() != nil {
			// Shutting down: the remaining environments never start.
			job.Results[i].State, job.Results[i].ExitCode, job.Results[i].Error = StateFailed, -1, "not run: "+ErrShutdown.Error()
			all = false
			continue
		}
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
	// The engine leads its own process group (SysProcAttr Setpgid, see
	// proc_unix.go) and a timeout kills the whole group: git children too.
	inProcessGroup(cmd)
	out := &tailBuffer{limit: r.opts.OutputLimit}
	errTail := &tailBuffer{limit: stderrTailLimit}
	cmd.Stdout = out
	cmd.Stderr = io.MultiWriter(out, errTail)
	// If a grandchild escaped the group and holds the output pipe, stop
	// waiting for it this long after the kill.
	cmd.WaitDelay = 10 * time.Second

	res.StartedAt = time.Now().UTC()
	log.Infof("deploy %s with %s: %q", res.Environment, r.opts.Engine.Name, argv)
	runErr := cmd.Run()
	res.FinishedAt = time.Now().UTC()
	res.DurationMS = res.FinishedAt.Sub(res.StartedAt).Milliseconds()
	res.Output, res.OutputTruncated = out.snapshot()

	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		res.ExitCode = 0
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case cmd.ProcessState != nil:
		// It ran, but Wait gave up on its output pipes (exec.ErrWaitDelay).
		res.ExitCode = cmd.ProcessState.ExitCode()
	default:
		res.State, res.ExitCode, res.Error = StateFailed, -1, "engine did not start: "+runErr.Error()
		return res
	}
	switch ctx.Err() {
	case context.DeadlineExceeded:
		res.State, res.Error = StateFailed, fmt.Sprintf("engine timed out after %s", r.opts.Timeout)
		return res
	case context.Canceled:
		res.State, res.Error = StateFailed, "engine stopped: "+ErrShutdown.Error()
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
	if res.ExitCode != 0 {
		if tail := summariseStderr(errTail.snapshot()); tail != "" {
			res.Error += ": " + tail
		}
	}
	return res
}

// stderrTailLimit bounds the stderr appended to an "engine exited N" error.
const stderrTailLimit = 512

var ansiEscapeRE = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

// summariseStderr turns the end of an engine's stderr into one line: a line
// cut by the bound dropped (or marked "..." when it is the only one), colour
// codes removed, blank lines dropped, lines joined with " | ".
func summariseStderr(s string, truncated bool) string {
	if truncated {
		if i := strings.IndexByte(s, '\n'); i >= 0 && strings.TrimSpace(s[i+1:]) != "" {
			s = s[i+1:]
		} else {
			s = "..." + s
		}
	}
	var lines []string
	for _, l := range strings.Split(ansiEscapeRE.ReplaceAllString(s, ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, " | ")
}

// tailBuffer keeps the last limit bytes written to it; the end of an engine's
// output is where its error is. stdout and stderr are copied by separate
// goroutines, so writes are locked.
type tailBuffer struct {
	mu        sync.Mutex
	limit     int
	buf       []byte
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(p) > b.limit {
		p = p[len(p)-b.limit:]
		b.truncated = true
	}
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.limit; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
		b.truncated = true
	}
	return n, nil
}

func (b *tailBuffer) String() string {
	s, _ := b.snapshot()
	return s
}

// snapshot returns what is kept and whether anything was cut.
func (b *tailBuffer) snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf), b.truncated
}
