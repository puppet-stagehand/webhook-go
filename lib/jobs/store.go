package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

// MaxListLimit caps one List call, whatever limit the caller asks for.
const MaxListLimit = 100

// jobIDRE is the form of every job id: 128 random bits in lower-case hex. A
// file store never builds a path from anything else.
var jobIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// InterruptedReason is the error Reconcile gives a job a dead process left
// unfinished.
const InterruptedReason = "interrupted by a restart"

// FileStore keeps one JSON file per job in <state_dir>/jobs/<id>.json, so
// records survive a restart (D-12). Files are 0600 and the directory 0700:
// they hold repository URLs and engine output. Every write goes to a
// temporary file that is renamed over the record, so a crash never leaves a
// half-written record. Once a job finishes, the store deletes the oldest
// finished records beyond maxJobs.
//
// Each store is an instance with a random id, and holds an flock on
// <state_dir>/instances/<id>.lock while it is open. Records name the instance
// that wrote them ("owner", never shown by the API), so Reconcile can tell a
// job a dead process left behind from one another live process is running.
type FileStore struct {
	mu       sync.Mutex
	stateDir string
	dir      string // <state_dir>/jobs
	maxJobs  int
	instance string
	instLock *os.File // nil when flock is unavailable
}

// storedJob is a record on disk: the job and the instance that wrote it.
type storedJob struct {
	Job
	Owner string `json:"owner,omitempty"`
}

// NewFileStore opens (creating as needed) the job store under stateDir.
func NewFileStore(stateDir string, maxJobs int) (*FileStore, error) {
	if !filepath.IsAbs(stateDir) {
		return nil, fmt.Errorf("jobs: state_dir must be absolute, got %q", stateDir)
	}
	if maxJobs < 1 {
		return nil, fmt.Errorf("jobs: max_jobs must be at least 1, got %d", maxJobs)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("jobs: state_dir: %w", err)
	}
	dir := filepath.Join(stateDir, "jobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	// An existing directory may have been created wider; job records are
	// for this service only.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	s := &FileStore{stateDir: stateDir, dir: dir, maxJobs: maxJobs, instance: newJobID()}
	if err := s.lockInstance(); err != nil {
		// Without it, other instances treat this one's unfinished jobs as
		// abandoned; deploys still work.
		log.Errorf("jobs: instance lock: %v", err)
	}
	return s, nil
}

func (s *FileStore) instancesDir() string { return filepath.Join(s.stateDir, "instances") }

func (s *FileStore) lockInstance() error {
	if err := os.MkdirAll(s.instancesDir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.instancesDir(), s.instance+".lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok, err := tryFlockExclusive(f)
	if err == nil && !ok {
		err = errors.New("lock held by another process")
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	s.instLock = f
	return nil
}

// Close releases the store's instance lock. Records stay on disk.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.instLock == nil {
		return nil
	}
	name := s.instLock.Name()
	unflock(s.instLock)
	err := s.instLock.Close()
	s.instLock = nil
	os.Remove(name)
	return err
}

// ownerAlive reports whether the instance that wrote a record is still
// running: its instance lock file exists and is flocked by someone else.
// A stale lock file of a dead instance is removed.
func (s *FileStore) ownerAlive(owner string) bool {
	if owner == s.instance && s.instLock != nil {
		return true
	}
	if !jobIDRE.MatchString(owner) {
		return false
	}
	path := filepath.Join(s.instancesDir(), owner+".lock")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	ok, err := tryFlockExclusive(f)
	if err != nil {
		return false
	}
	if !ok {
		return true
	}
	unflock(f)
	os.Remove(path)
	return false
}

// Reconcile fails every job a dead process left queued or deploying: its
// unfinished environments become failed "interrupted by a restart", so a
// client never polls a job forever (T-57.6-35). Finished jobs, and jobs an
// instance that is still running owns, are untouched. The server calls it once
// at start, before the deploy routes accept requests. It returns how many
// jobs it failed.
func (s *FileStore) Reconcile() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() || !jobIDRE.MatchString(id) {
			continue
		}
		rec, err := s.readStoredLocked(id)
		if err != nil {
			log.Errorf("jobs: reconcile: skipping %s: %v", e.Name(), err)
			continue
		}
		if rec.State.Terminal() || s.ownerAlive(rec.Owner) {
			continue
		}
		job := FailUnstarted(rec.Job, InterruptedReason)
		if err := s.writeLocked(job); err != nil {
			return n, err
		}
		log.Warnf("jobs: deploy %s was %s when its process stopped; marked failed", id, rec.State)
		n++
	}
	return n, nil
}

func (s *FileStore) path(id string) string { return filepath.Join(s.dir, id+".json") }

// Save writes job atomically, then prunes old finished records.
func (s *FileStore) Save(job Job) error {
	if !jobIDRE.MatchString(job.ID) {
		return fmt.Errorf("jobs: invalid job id %q", job.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeLocked(job); err != nil {
		return err
	}
	if job.State.Terminal() {
		s.pruneLocked()
	}
	return nil
}

func (s *FileStore) writeLocked(job Job) error {
	b, err := json.Marshal(storedJob{Job: job, Owner: s.instance})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.dir, job.ID+".json", b); err != nil {
		return fmt.Errorf("jobs: saving %s: %w", job.ID, err)
	}
	return nil
}

// writeFileAtomic writes b to dir/name with mode 0600 through a temporary
// file in the same directory and a rename.
func writeFileAtomic(dir, name string, b []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*") // CreateTemp uses mode 0600
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return err
	}
	ok = true
	return nil
}

// Get returns the job with id, or ErrNotFound (also for an id that is not a
// job id at all).
func (s *FileStore) Get(id string) (Job, error) {
	if !jobIDRE.MatchString(id) {
		return Job{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(id)
}

func (s *FileStore) readLocked(id string) (Job, error) {
	rec, err := s.readStoredLocked(id)
	return rec.Job, err
}

func (s *FileStore) readStoredLocked(id string) (storedJob, error) {
	b, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return storedJob{}, ErrNotFound
	}
	if err != nil {
		return storedJob{}, err
	}
	var rec storedJob
	if err := json.Unmarshal(b, &rec); err != nil {
		return storedJob{}, fmt.Errorf("jobs: record %s is corrupt: %w", id, err)
	}
	return rec, nil
}

// List returns up to limit jobs (at most MaxListLimit), newest first.
func (s *FileStore) List(limit int) ([]Job, error) {
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.allLocked()
	if err != nil {
		return nil, err
	}
	if limit < 0 {
		limit = 0
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// allLocked reads every record, newest first. A corrupt record is logged and
// skipped so one bad file cannot hide the rest of the history.
func (s *FileStore) allLocked() ([]Job, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := []Job{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() || !jobIDRE.MatchString(id) {
			continue
		}
		j, err := s.readLocked(id)
		if err != nil {
			log.Errorf("jobs: skipping %s: %v", e.Name(), err)
			continue
		}
		out = append(out, j)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if !out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].CreatedAt.After(out[b].CreatedAt)
		}
		return out[a].ID > out[b].ID
	})
	return out, nil
}

// pruneLocked deletes the oldest finished records beyond maxJobs. Unfinished
// jobs are never deleted.
func (s *FileStore) pruneLocked() {
	all, err := s.allLocked()
	if err != nil {
		log.Errorf("jobs: retention: %v", err)
		return
	}
	for i := s.maxJobs; i < len(all); i++ {
		if !all[i].State.Terminal() {
			continue
		}
		if err := os.Remove(s.path(all[i].ID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Errorf("jobs: retention: %v", err)
		}
	}
}
