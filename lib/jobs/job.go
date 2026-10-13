// Package jobs runs Stagehand deploy jobs: one r10k or g10k invocation per
// environment, judged by the environment's deploy marker and the commit SHA
// the caller expected, never by the exit code alone.
package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// State is the state of a job or of one environment in it.
type State string

const (
	StateQueued    State = "queued"
	StateDeploying State = "deploying"
	StateDeployed  State = "deployed"
	StateFailed    State = "failed"
)

// Terminal reports whether s is final: deployed or failed.
func (s State) Terminal() bool { return s == StateDeployed || s == StateFailed }

// newJobID returns 128 random bits in lower-case hex.
func newJobID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("jobs: no randomness for a job id: " + err.Error()) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b[:])
}

var (
	// environmentNameRE is the SDK's deploy-time rule for environment names.
	// It admits no path separators, no leading dash and no shell metacharacters.
	environmentNameRE = regexp.MustCompile(`^[a-z0-9_]+$`)
	// shaRE is a full, lower-case git commit SHA.
	shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ValidEnvironmentName reports whether name is a deployable environment name.
func ValidEnvironmentName(name string) bool { return environmentNameRE.MatchString(name) }

// ValidSHA reports whether sha is a 40-character lower-case hex commit SHA.
func ValidSHA(sha string) bool { return shaRE.MatchString(sha) }

// EnvRequest is one environment a caller asks to deploy, with the commit it
// expects the environment to hold afterwards.
type EnvRequest struct {
	Name        string `json:"name"`
	ExpectedSHA string `json:"expected_sha"`
}

// EnvResult is the normalised outcome for one environment.
type EnvResult struct {
	Environment         string    `json:"environment"`
	State               State     `json:"state"`
	ExitCode            int       `json:"exit_code"`
	ExpectedSHA         string    `json:"expected_sha"`
	DeployedSHA         string    `json:"deployed_sha"`
	MarkerPresent       bool      `json:"marker_present"`
	MarkerDeploySuccess bool      `json:"marker_deploy_success"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at"`
	DurationMS          int64     `json:"duration_ms"`
	Output              string    `json:"output"`
	OutputTruncated     bool      `json:"output_truncated"`
	Error               string    `json:"error"`
}

// Job is one deploy request and its per-environment results. Its JSON form is
// the /api/v1/deploys contract.
type Job struct {
	ID         string      `json:"id"`
	Engine     string      `json:"engine"`
	State      State       `json:"state"`
	CreatedAt  time.Time   `json:"created_at"`
	StartedAt  *time.Time  `json:"started_at"`
	FinishedAt *time.Time  `json:"finished_at"`
	Results    []EnvResult `json:"results"`
}

func (j Job) clone() Job {
	c := j
	c.Results = append([]EnvResult(nil), j.Results...)
	if j.StartedAt != nil {
		t := *j.StartedAt
		c.StartedAt = &t
	}
	if j.FinishedAt != nil {
		t := *j.FinishedAt
		c.FinishedAt = &t
	}
	return c
}

// ErrNotFound is returned by a Store for an unknown job id.
var ErrNotFound = errors.New("deploy not found")

// Store keeps job records. FileStore (store.go) is what the server uses;
// MemoryStore below serves tests.
type Store interface {
	// Save inserts or replaces a job.
	Save(job Job) error
	// Get returns the job with id, or ErrNotFound.
	Get(id string) (Job, error)
	// List returns up to limit jobs, newest first.
	List(limit int) ([]Job, error)
}

// MemoryStore is a bounded in-memory Store for tests. History is lost on
// restart, so the server uses FileStore.
type MemoryStore struct {
	mu    sync.Mutex
	max   int
	jobs  map[string]Job
	order []string // oldest first
}

// NewMemoryStore keeps at most max jobs, evicting the oldest.
func NewMemoryStore(max int) *MemoryStore {
	if max < 1 {
		max = 1
	}
	return &MemoryStore{max: max, jobs: map[string]Job{}}
}

func (s *MemoryStore) Save(job Job) error {
	if job.ID == "" {
		return fmt.Errorf("job has no id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[job.ID]; !ok {
		s.order = append(s.order, job.ID)
		for len(s.order) > s.max {
			delete(s.jobs, s.order[0])
			s.order = s.order[1:]
		}
	}
	s.jobs[job.ID] = job.clone()
	return nil
}

func (s *MemoryStore) Get(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return j.clone(), nil
}

func (s *MemoryStore) List(limit int) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Job{}
	for i := len(s.order) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.jobs[s.order[i]].clone())
	}
	return out, nil
}
