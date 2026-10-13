package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxpupuli/webhook-go/lib/jobs/fakeengine"
)

const (
	shaA = "1111111111111111111111111111111111111111"
	shaB = "2222222222222222222222222222222222222222"
)

func boolPtr(b bool) *bool { return &b }

func freshMarker(sig string, at time.Time) *Marker {
	return &Marker{
		Name:          "production",
		Signature:     sig,
		DeploySuccess: boolPtr(true),
		StartedAt:     at.Format(r10kTimeLayout),
		FinishedAt:    at.Format(r10kTimeLayout),
	}
}

func TestNormalizeSignatureMismatch(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	later := start.Add(30 * time.Second)

	cases := []struct {
		name      string
		exit      int
		marker    *Marker
		expected  string
		wantState State
		wantSHA   string
		wantErr   string
	}{
		{"deployed", 0, freshMarker(shaA, later), shaA, StateDeployed, shaA, ""},
		{"signature mismatch", 0, freshMarker(shaB, later), shaA, StateFailed, shaB, "deployed " + shaB + ", expected " + shaA},
		{"stale marker", 0, freshMarker(shaA, start.Add(-time.Hour)), shaA, StateFailed, "", "marker not rewritten by this run"},
		{"missing marker", 0, nil, shaA, StateFailed, "", "no deploy marker"},
		{"non-zero exit with fresh marker", 1, freshMarker(shaA, later), shaA, StateFailed, shaA, "engine exited 1"},
		{"deploy_success false", 0, func() *Marker { m := freshMarker(shaA, later); m.DeploySuccess = boolPtr(false); return m }(), shaA, StateFailed, shaA, "marker reports deploy_success false"},
		{"deploy_success absent", 0, func() *Marker { m := freshMarker(shaA, later); m.DeploySuccess = nil; return m }(), shaA, StateFailed, shaA, "marker reports deploy_success false"},
		{"unreadable finished_at", 0, func() *Marker { m := freshMarker(shaA, later); m.FinishedAt = "yesterday"; return m }(), shaA, StateFailed, "", "marker finished_at unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, sha, errText := Normalize(tc.exit, tc.marker, start, tc.expected)
			if state != tc.wantState || sha != tc.wantSHA || !strings.HasPrefix(errText, tc.wantErr) || (tc.wantErr == "" && errText != "") {
				t.Fatalf("Normalize = (%q, %q, %q), want (%q, %q, %q)", state, sha, errText, tc.wantState, tc.wantSHA, tc.wantErr)
			}
		})
	}
}

// r10k writes marker times with one-second resolution (Ruby Time#to_s), so a
// deploy that finishes in the same second the job started is still fresh.
func TestNormalizeSameSecondIsFresh(t *testing.T) {
	start := time.Date(2026, 10, 13, 12, 0, 0, 900_000_000, time.UTC)
	m := freshMarker(shaA, start) // formatted without the fraction: 12:00:00
	state, _, errText := Normalize(0, m, start, shaA)
	if state != StateDeployed {
		t.Fatalf("state = %q (%s), want deployed", state, errText)
	}
}

func TestParseMarkerTimes(t *testing.T) {
	for _, s := range []string{
		"2026-10-13 12:00:00 +0000",      // r10k
		"2026-10-13T12:00:00.123456789Z", // g10k
		"2026-10-13T12:00:00+02:00",      // RFC 3339
		"2026-10-13 12:00:00.5 -0700",    // fractional r10k-style
	} {
		if _, err := parseMarkerTime(s); err != nil {
			t.Errorf("parseMarkerTime(%q): %v", s, err)
		}
	}
}

// fakeR10k points a runner at this test binary acting as r10k.
func fakeR10k(t *testing.T, fixture, sha string, limit int) (*Runner, string, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	basedir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv(fakeengine.FixtureEnv, fixture)
	t.Setenv(fakeengine.BasedirEnv, basedir)
	t.Setenv(fakeengine.SHAEnv, sha)
	t.Setenv(fakeengine.ArgvLogEnv, argvLog)

	r, err := NewRunner(Options{
		Engine: R10kEngine(func(env string) []string {
			return []string{exe, "deploy", "environment", env, "--config=/etc/puppetlabs/r10k/r10k.yaml", "--modules"}
		}),
		Basedir:     basedir,
		Timeout:     time.Minute,
		OutputLimit: limit,
		Store:       NewMemoryStore(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, basedir, argvLog
}

func TestRunnerDeploysOneEnvironmentR10k(t *testing.T) {
	r, basedir, argvLog := fakeR10k(t, "testdata/r10k-5.0.3/deploy-success.json", shaA, 262144)

	job, err := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != StateQueued || job.Engine != "r10k" || job.ID == "" {
		t.Fatalf("new job = %+v", job)
	}
	job, err = r.Run(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != StateDeployed || len(job.Results) != 1 {
		t.Fatalf("job = %+v", job)
	}
	res := job.Results[0]
	if res.State != StateDeployed || res.ExitCode != 0 || res.DeployedSHA != shaA || res.ExpectedSHA != shaA ||
		!res.MarkerPresent || !res.MarkerDeploySuccess || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Output, "Environment production is now at "+shaA) {
		t.Fatalf("output = %q", res.Output)
	}
	if res.StartedAt.IsZero() || res.FinishedAt.Before(res.StartedAt) || res.DurationMS < 0 {
		t.Fatalf("timings = %v %v %d", res.StartedAt, res.FinishedAt, res.DurationMS)
	}
	if _, err := os.Stat(filepath.Join(basedir, "production", ".r10k-deploy.json")); err != nil {
		t.Fatal(err)
	}
	argvs, err := fakeengine.ReadArgvLog(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(argvs) != 1 || strings.Join(argvs[0][:4], " ") != "deploy environment production --config=/etc/puppetlabs/r10k/r10k.yaml" {
		t.Fatalf("argv = %q", argvs)
	}
	stored, err := r.Store().Get(job.ID)
	if err != nil || stored.State != StateDeployed {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
}

func TestRunnerReportsSignatureMismatch(t *testing.T) {
	r, _, _ := fakeR10k(t, "testdata/r10k-5.0.3/deploy-success.json", shaB, 262144)
	job, _ := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	job, err := r.Run(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	res := job.Results[0]
	if job.State != StateFailed || res.State != StateFailed || res.DeployedSHA != shaB ||
		res.Error != "deployed "+shaB+", expected "+shaA {
		t.Fatalf("job = %+v", job)
	}
}

func TestRunnerBoundsOutput(t *testing.T) {
	r, _, _ := fakeR10k(t, "testdata/r10k-5.0.3/deploy-success.json", shaA, 32)
	job, _ := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	job, _ = r.Run(context.Background(), job)
	res := job.Results[0]
	if !res.OutputTruncated || len(res.Output) != 32 {
		t.Fatalf("output %d bytes, truncated=%v", len(res.Output), res.OutputTruncated)
	}
	// The tail is kept: the last line r10k printed is the most useful one.
	if !strings.HasSuffix(res.Output, "modules/m\n") {
		t.Fatalf("output tail = %q", res.Output)
	}
}

func TestRunnerEngineMissing(t *testing.T) {
	r, err := NewRunner(Options{
		Engine:      R10kEngine(func(env string) []string { return []string{"/nonexistent/r10k", "deploy", "environment", env} }),
		Basedir:     t.TempDir(),
		Timeout:     time.Minute,
		OutputLimit: 1024,
		Store:       NewMemoryStore(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	job, _ := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	job, _ = r.Run(context.Background(), job)
	res := job.Results[0]
	if job.State != StateFailed || res.ExitCode != -1 || !strings.HasPrefix(res.Error, "engine did not start") {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunnerRefusesInvalidNames(t *testing.T) {
	r, _, _ := fakeR10k(t, "testdata/r10k-5.0.3/deploy-success.json", shaA, 1024)
	for _, env := range []EnvRequest{{Name: "../x", ExpectedSHA: shaA}, {Name: "prod;rm", ExpectedSHA: shaA}, {Name: "production", ExpectedSHA: "abc"}} {
		if _, err := r.NewJob([]EnvRequest{env}); err == nil {
			t.Errorf("NewJob(%+v) accepted", env)
		}
	}
}

func TestMemoryStoreListNewestFirstAndBounded(t *testing.T) {
	s := NewMemoryStore(2)
	for _, id := range []string{"a", "b", "c"} {
		if err := s.Save(Job{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := s.List(10)
	if len(list) != 2 || list[0].ID != "c" || list[1].ID != "b" {
		t.Fatalf("list = %+v", list)
	}
	if _, err := s.Get("a"); err != ErrNotFound {
		t.Fatalf("evicted job: err = %v", err)
	}
}
