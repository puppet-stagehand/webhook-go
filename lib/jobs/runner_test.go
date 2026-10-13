package jobs

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
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

// shaPrev is the commit an environment held before the recorded run: what a
// seeded marker_before carries.
const shaPrev = "3333333333333333333333333333333333333333"

// fakeEngine points a runner at this test binary acting as r10k or g10k,
// replaying fixture (an SDK harness recording copied verbatim into testdata)
// after laying down the fixture's marker_before, an hour old.
func fakeEngine(t *testing.T, tool, fixture, sha string, opts Options) (*Runner, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	basedir := t.TempDir()
	if fixture != "" {
		t.Setenv(fakeengine.FixtureEnv, fixture)
		if err := fakeengine.SeedBefore(fixture, basedir, shaPrev, time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(fakeengine.BasedirEnv, basedir)
	t.Setenv(fakeengine.SHAEnv, sha)

	opts.Engine = R10kEngine(func(env string) []string {
		return []string{exe, "deploy", "environment", env, "--config=/etc/puppetlabs/r10k/r10k.yaml", "--modules"}
	})
	if tool == "g10k" {
		opts.Engine = G10kEngine(func(env string) []string {
			return []string{exe, "-config=/etc/puppetlabs/r10k/g10k.yaml", "-branch=" + env}
		})
	}
	opts.Basedir = basedir
	if opts.Timeout == 0 {
		opts.Timeout = time.Minute
	}
	if opts.OutputLimit == 0 {
		opts.OutputLimit = 262144
	}
	if opts.Store == nil {
		opts.Store = NewMemoryStore(10)
	}
	r, err := NewRunner(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r, basedir
}

func deployOneEnv(t *testing.T, r *Runner, env, expected string) (Job, EnvResult) {
	t.Helper()
	job, err := r.NewJob([]EnvRequest{{Name: env, ExpectedSHA: expected}})
	if err != nil {
		t.Fatal(err)
	}
	job, err = r.Run(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	return job, job.Results[0]
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

// lastStderrLine is the last non-empty line the fixture wrote to stderr.
func lastStderrLine(f fakeengine.Fixture, sha string) string {
	lines := strings.Split(strings.ReplaceAll(ansiRE.ReplaceAllString(f.Stderr, ""), "<SHA>", sha), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// TestNormalizeFixtureMatrix replays every copied SDK harness fixture through
// the real runner and Normalize (research Pitfall 3). Each misleading outcome
// must end failed, its error naming the first unmet condition; each success
// must end deployed. Fixture sources (verbatim copies, never edited):
// stagehand-sdk-public harness/fixtures/r10k-5.0.3/*.json and
// harness/fixtures/g10k-0.10.0/*.json.
type matrixCase struct {
	name, tool, fixture, env string
	wroteSHA, expected       string // what the engine deploys; what the caller expects
	exitOverride             string
	wantState                State
	wantErr                  string // prefix
	wantMarker, wantSuccess  bool
	wantDeployedSHA          string
}

// fixtureMatrix is one case per copied fixture, both engines, plus the
// misleading variants (a wrong commit, an exit code masked to 0).
var fixtureMatrix = []matrixCase{
	// r10k 5.0.3
	{"r10k deploy-success", "r10k", "deploy-success", "production", shaA, shaA, "", StateDeployed, "", true, true, shaA},
	{"r10k fresh marker, wrong commit", "r10k", "deploy-success", "production", shaB, shaA, "", StateFailed, "deployed " + shaB + ", expected " + shaA, true, true, shaB},
	{"r10k unknown-environment", "r10k", "unknown-environment", "nosuchenv", shaA, shaA, "", StateFailed, "engine exited 1: ", false, false, ""},
	{"r10k unreachable-remote", "r10k", "unreachable-remote", "production", shaA, shaA, "", StateFailed, "engine exited 1: ", true, true, ""},
	{"r10k bad-module-ref (checkout moved, marker unchanged)", "r10k", "bad-module-ref", "production", shaA, shaA, "", StateFailed, "engine exited 1: ", true, true, ""},
	{"r10k bad-module-ref, exit code masked", "r10k", "bad-module-ref", "production", shaA, shaA, "0", StateFailed, "marker not rewritten by this run", true, true, ""},
	{"r10k missing-config", "r10k", "missing-config", "production", shaA, shaA, "", StateFailed, "engine exited 1: ", false, false, ""},
	{"r10k readonly-cachedir", "r10k", "readonly-cachedir", "production", shaA, shaA, "", StateFailed, "engine exited 1: ", true, true, ""},
	// g10k 0.10.0
	{"g10k deploy-success", "g10k", "deploy-success", "production", shaA, shaA, "", StateDeployed, "", true, true, shaA},
	{"g10k fresh marker, wrong commit", "g10k", "deploy-success", "production", shaB, shaA, "", StateFailed, "deployed " + shaB + ", expected " + shaA, true, true, shaB},
	{"g10k unknown-environment exits 0", "g10k", "unknown-environment", "nosuchenv", shaA, shaA, "", StateFailed, "no deploy marker", false, false, ""},
	{"g10k unreachable-remote exits 0", "g10k", "unreachable-remote", "production", shaA, shaA, "", StateFailed, "marker not rewritten by this run", true, true, ""},
	{"g10k bad-module-ref", "g10k", "bad-module-ref", "production", shaA, shaA, "", StateFailed, "engine exited 1: ", true, false, shaA},
	{"g10k bad-module-ref, exit code masked", "g10k", "bad-module-ref", "production", shaA, shaA, "0", StateFailed, "marker reports deploy_success false", true, false, shaA},
	{"g10k missing-config", "g10k", "missing-config", "production", shaA, shaA, "", StateFailed, "engine exited 1: ", false, false, ""},
	{"g10k readonly-cachedir", "g10k", "readonly-cachedir", "production", shaA, shaA, "", StateFailed, "engine exited 1: ", true, true, ""},
}

var fixtureDirs = map[string]string{"r10k": "testdata/r10k-5.0.3", "g10k": "testdata/g10k-0.10.0"}

func TestNormalizeFixtureMatrix(t *testing.T) {
	for _, tc := range fixtureMatrix {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(fixtureDirs[tc.tool], tc.fixture+".json")
			fx, err := fakeengine.LoadFixture(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.exitOverride != "" {
				t.Setenv(fakeengine.ExitCodeEnv, tc.exitOverride)
			}
			r, _ := fakeEngine(t, tc.tool, path, tc.wroteSHA, Options{})
			job, res := deployOneEnv(t, r, tc.env, tc.expected)

			if job.Engine != tc.tool || res.State != tc.wantState || job.State != tc.wantState {
				t.Fatalf("engine %s, job %s, env %s (%s); want %s %s", job.Engine, job.State, res.State, res.Error, tc.tool, tc.wantState)
			}
			if !strings.HasPrefix(res.Error, tc.wantErr) || (tc.wantErr == "" && res.Error != "") {
				t.Fatalf("error = %q, want prefix %q", res.Error, tc.wantErr)
			}
			if res.MarkerPresent != tc.wantMarker || res.MarkerDeploySuccess != tc.wantSuccess || res.DeployedSHA != tc.wantDeployedSHA {
				t.Fatalf("marker present %v, deploy_success %v, deployed_sha %q; want %v %v %q",
					res.MarkerPresent, res.MarkerDeploySuccess, res.DeployedSHA, tc.wantMarker, tc.wantSuccess, tc.wantDeployedSHA)
			}
			// A non-zero exit carries the end of the engine's stderr.
			if strings.HasPrefix(tc.wantErr, "engine exited") {
				if want := lastStderrLine(fx, tc.wroteSHA); !strings.Contains(res.Error, want) {
					t.Fatalf("error %q lacks the stderr tail %q", res.Error, want)
				}
			}
			// The replay is faithful: the lines the harness expects are in the output.
			for _, line := range fx.ExpectLines {
				if line = strings.ReplaceAll(line, "<SHA>", tc.wroteSHA); !strings.Contains(res.Output, line) {
					t.Fatalf("output lacks expected line %q:\n%s", line, res.Output)
				}
			}
		})
	}

}

// Every fixture copied into testdata is in the matrix.
func TestFixtureMatrixCoversEveryFixture(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range fixtureMatrix {
		covered[filepath.Join(fixtureDirs[tc.tool], tc.fixture+".json")] = true
	}
	for _, dir := range fixtureDirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		if len(files) < 3 {
			t.Errorf("%s holds %d fixtures, want at least 3", dir, len(files))
		}
		for _, f := range files {
			if !covered[f] {
				t.Errorf("fixture %s is not in the matrix", f)
			}
		}
	}
}

func TestOutputIsBounded(t *testing.T) {
	t.Setenv(fakeengine.ModeEnv, "flood") // 1 MiB of output, then exit 0
	r, _ := fakeEngine(t, "r10k", "", shaA, Options{OutputLimit: 4096})
	_, res := deployOneEnv(t, r, "production", shaA)
	if len(res.Output) > 4096 || !res.OutputTruncated {
		t.Fatalf("output %d bytes, truncated=%v; want at most 4096 and truncated", len(res.Output), res.OutputTruncated)
	}
	if res.State != StateFailed || res.Error != "no deploy marker" {
		t.Fatalf("result = %s %q", res.State, res.Error)
	}
}
