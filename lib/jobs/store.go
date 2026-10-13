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

// FileStore keeps one JSON file per job in <state_dir>/jobs/<id>.json, so
// records survive a restart (D-12). Files are 0600 and the directory 0700:
// they hold repository URLs and engine output. Every write goes to a
// temporary file that is renamed over the record, so a crash never leaves a
// half-written record. Once a job finishes, the store deletes the oldest
// finished records beyond maxJobs.
type FileStore struct {
	mu      sync.Mutex
	dir     string // <state_dir>/jobs
	maxJobs int
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
	return &FileStore{dir: dir, maxJobs: maxJobs}, nil
}

// Close releases the store. Records stay on disk.
func (s *FileStore) Close() error { return nil }

func (s *FileStore) path(id string) string { return filepath.Join(s.dir, id+".json") }

// Save writes job atomically, then prunes old finished records.
func (s *FileStore) Save(job Job) error {
	if !jobIDRE.MatchString(job.ID) {
		return fmt.Errorf("jobs: invalid job id %q", job.ID)
	}
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeFileAtomic(s.dir, job.ID+".json", b); err != nil {
		return fmt.Errorf("jobs: saving %s: %w", job.ID, err)
	}
	if job.State.Terminal() {
		s.pruneLocked()
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
	b, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return Job{}, fmt.Errorf("jobs: record %s is corrupt: %w", id, err)
	}
	return j, nil
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
