package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// finishedJob is a terminal job created at the given time.
func finishedJob(t *testing.T, created time.Time) Job {
	t.Helper()
	started, finished := created.Add(time.Second), created.Add(5*time.Second)
	return Job{
		ID:         newJobID(),
		Engine:     "g10k",
		State:      StateDeployed,
		CreatedAt:  created.UTC(),
		StartedAt:  &started,
		FinishedAt: &finished,
		Results: []EnvResult{{
			Environment: "production", State: StateDeployed, ExpectedSHA: shaA, DeployedSHA: shaA,
			MarkerPresent: true, MarkerDeploySuccess: true, StartedAt: started, FinishedAt: finished,
			DurationMS: 4000, Output: "Synced\n",
		}},
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestJobRecordSurvivesRestart(t *testing.T) {
	stateDir := t.TempDir()
	first, err := NewFileStore(stateDir, 500)
	if err != nil {
		t.Fatal(err)
	}
	job := finishedJob(t, time.Now().Add(-time.Minute))
	if err := first.Save(job); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// A new process: a new store on the same state directory.
	second, err := NewFileStore(stateDir, 500)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	got, err := second.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mustJSON(t, got) != mustJSON(t, job) {
		t.Fatalf("read back\n%s\nwant\n%s", mustJSON(t, got), mustJSON(t, job))
	}
	list, err := second.List(10)
	if err != nil || len(list) != 1 || list[0].ID != job.ID {
		t.Fatalf("List = %+v, %v", list, err)
	}

	jobsDir := filepath.Join(stateDir, "jobs")
	st, err := os.Stat(jobsDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("jobs dir mode = %o, want 700", st.Mode().Perm())
	}
	st, err = os.Stat(filepath.Join(jobsDir, job.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("job file mode = %o, want 600", st.Mode().Perm())
	}
}

func TestFileStoreRejectsUnsafeIDs(t *testing.T) {
	s, err := NewFileStore(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, id := range []string{"", "..", "../x", "00000000-0000-0000-0000-000000000000", "ABCDEF0123456789ABCDEF0123456789"} {
		if _, err := s.Get(id); err != ErrNotFound {
			t.Errorf("Get(%q) err = %v, want ErrNotFound", id, err)
		}
		if err := s.Save(Job{ID: id}); err == nil {
			t.Errorf("Save(%q) accepted", id)
		}
	}
}

func TestRetentionKeepsNewest(t *testing.T) {
	stateDir := t.TempDir()
	s, err := NewFileStore(stateDir, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	base := time.Now().Add(-time.Hour)
	var ids []string
	for i := 0; i < 5; i++ {
		j := finishedJob(t, base.Add(time.Duration(i)*time.Minute))
		ids = append(ids, j.ID)
		if err := s.Save(j); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(stateDir, "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		files = append(files, e.Name())
	}
	sort.Strings(files)
	want := []string{ids[2] + ".json", ids[3] + ".json", ids[4] + ".json"}
	sort.Strings(want)
	if mustJSON(t, files) != mustJSON(t, want) {
		t.Fatalf("files = %v, want the newest three %v", files, want)
	}

	list, err := s.List(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].ID != ids[4] || list[1].ID != ids[3] || list[2].ID != ids[2] {
		t.Fatalf("List not newest first: %v", list)
	}

	// An unfinished job is never pruned, even when it is the oldest.
	old := finishedJob(t, base.Add(-time.Hour))
	old.State, old.FinishedAt = StateQueued, nil
	if err := s.Save(old); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(finishedJob(t, base.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(old.ID); err != nil {
		t.Fatalf("queued job pruned: %v", err)
	}
}

func TestFileStoreListCapsAt100(t *testing.T) {
	s, err := NewFileStore(t.TempDir(), 500)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 105; i++ {
		if err := s.Save(finishedJob(t, base.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List(1000)
	if err != nil || len(list) != 100 {
		t.Fatalf("List(1000) = %d jobs, %v; want 100", len(list), err)
	}
}

func TestReconcileFailsInterruptedJobs(t *testing.T) {
	stateDir := t.TempDir()
	base := time.Now().Add(-time.Hour)

	// A previous process left work in flight, then died.
	dead, err := NewFileStore(stateDir, 500)
	if err != nil {
		t.Fatal(err)
	}
	queued := finishedJob(t, base)
	queued.State, queued.StartedAt, queued.FinishedAt = StateQueued, nil, nil
	queued.Results[0] = EnvResult{Environment: "production", State: StateQueued, ExpectedSHA: shaA}

	deploying := finishedJob(t, base.Add(time.Minute))
	deploying.State, deploying.FinishedAt = StateDeploying, nil
	deploying.Results = append(deploying.Results,
		EnvResult{Environment: "staging", State: StateDeploying, ExpectedSHA: shaA},
		EnvResult{Environment: "dev", State: StateQueued, ExpectedSHA: shaA})

	deployed := finishedJob(t, base.Add(2*time.Minute))
	failed := finishedJob(t, base.Add(3*time.Minute))
	failed.State, failed.Results[0].State, failed.Results[0].Error = StateFailed, StateFailed, "engine exited 1"
	for _, j := range []Job{queued, deploying, deployed, failed} {
		if err := dead.Save(j); err != nil {
			t.Fatal(err)
		}
	}
	if err := dead.Close(); err != nil {
		t.Fatal(err)
	}
	terminalBefore := map[string][]byte{}
	for _, id := range []string{deployed.ID, failed.ID} {
		b, err := os.ReadFile(filepath.Join(stateDir, "jobs", id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		terminalBefore[id] = b
	}

	s, err := NewFileStore(stateDir, 500)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	n, err := s.Reconcile()
	if err != nil || n != 2 {
		t.Fatalf("Reconcile = %d, %v; want 2", n, err)
	}

	got, _ := s.Get(queued.ID)
	if got.State != StateFailed || got.FinishedAt == nil || got.Results[0].State != StateFailed ||
		got.Results[0].Error != "interrupted by a restart" {
		t.Fatalf("queued job after Reconcile = %+v", got)
	}
	got, _ = s.Get(deploying.ID)
	if got.State != StateFailed || got.FinishedAt == nil {
		t.Fatalf("deploying job after Reconcile = %+v", got)
	}
	if r := got.Results[0]; r.State != StateDeployed || r.Error != "" {
		t.Fatalf("an environment that finished was rewritten: %+v", r)
	}
	for _, r := range got.Results[1:] {
		if r.State != StateFailed || r.Error != "interrupted by a restart" {
			t.Fatalf("unfinished environment after Reconcile = %+v", r)
		}
	}
	for id, before := range terminalBefore {
		after, err := os.ReadFile(filepath.Join(stateDir, "jobs", id+".json"))
		if err != nil || string(after) != string(before) {
			t.Fatalf("terminal job %s was touched", id)
		}
	}

	// A job owned by a process that is still running is left alone, until
	// that process is gone.
	live, err := NewFileStore(stateDir, 500)
	if err != nil {
		t.Fatal(err)
	}
	running := finishedJob(t, time.Now())
	running.State, running.FinishedAt = StateDeploying, nil
	running.Results[0].State = StateDeploying
	if err := live.Save(running); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Reconcile(); err != nil || n != 0 {
		t.Fatalf("Reconcile with a live owner = %d, %v; want 0", n, err)
	}
	if got, _ := s.Get(running.ID); got.State != StateDeploying {
		t.Fatalf("a live process's job was failed: %+v", got)
	}
	live.Close()
	if n, err := s.Reconcile(); err != nil || n != 1 {
		t.Fatalf("Reconcile after the owner exited = %d, %v; want 1", n, err)
	}
}
