package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/voxpupuli/webhook-go/config"
	"github.com/voxpupuli/webhook-go/lib/jobs"
	"github.com/voxpupuli/webhook-go/lib/jobs/fakeengine"
)

// The fake r10k replays lib/jobs/testdata/r10k-5.0.3/deploy-success.json, a
// verbatim copy of stagehand-sdk-public harness/fixtures/r10k-5.0.3/deploy-success.json.
func TestMain(m *testing.M) {
	fakeengine.MaybeRun()
	os.Exit(m.Run())
}

const goodSHA = "0123456789abcdef0123456789abcdef01234567"

// newTestDeploys wires a deploys controller to the fake r10k and returns a
// gin engine serving it plus the fake's argv log.
func newTestDeploys(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	basedir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	fixture, _ := filepath.Abs("../lib/jobs/testdata/r10k-5.0.3/deploy-success.json")
	t.Setenv(fakeengine.FixtureEnv, fixture)
	t.Setenv(fakeengine.BasedirEnv, basedir)
	t.Setenv(fakeengine.SHAEnv, goodSHA)
	t.Setenv(fakeengine.ArgvLogEnv, argvLog)

	runner, err := jobs.NewRunner(jobs.Options{
		Engine: jobs.R10kEngine(func(env string) []string {
			return []string{exe, "deploy", "environment", env, "--config=/etc/puppetlabs/r10k/r10k.yaml"}
		}),
		Basedir:     basedir,
		Timeout:     time.Minute,
		OutputLimit: 4096,
		Store:       jobs.NewMemoryStore(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	newDeploysController(runner).Register(r.Group("/api/v1"))
	return r, argvLog
}

func post(r http.Handler, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deploys", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestDeployRefusesBadInput(t *testing.T) {
	r, argvLog := newTestDeploys(t)

	many := make([]string, 21)
	for i := range many {
		many[i] = fmt.Sprintf(`{"name":"env%d","expected_sha":"%s"}`, i, goodSHA)
	}

	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"shell metacharacters", `{"environments":[{"name":"prod;rm","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[0].name"},
		{"path traversal", `{"environments":[{"name":"../x","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[0].name"},
		{"empty name", `{"environments":[{"name":"","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[0].name"},
		{"uppercase name", `{"environments":[{"name":"Production","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[0].name"},
		{"option-looking name", `{"environments":[{"name":"--config=/x","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[0].name"},
		{"short sha", `{"environments":[{"name":"production","expected_sha":"abc"}],"wait":true}`, "environments[0].expected_sha"},
		{"missing sha", `{"environments":[{"name":"production"}],"wait":true}`, "environments[0].expected_sha"},
		{"uppercase sha", `{"environments":[{"name":"production","expected_sha":"` + strings.ToUpper(goodSHA) + `"}],"wait":true}`, "environments[0].expected_sha"},
		{"second entry bad", `{"environments":[{"name":"production","expected_sha":"` + goodSHA + `"},{"name":"a b","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[1].name"},
		{"duplicate", `{"environments":[{"name":"production","expected_sha":"` + goodSHA + `"},{"name":"production","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[1].name"},
		{"no environments", `{"environments":[],"wait":true}`, "environments"},
		{"too many environments", `{"environments":[` + strings.Join(many, ",") + `],"wait":true}`, "environments"},
		{"wait missing", `{"environments":[{"name":"production","expected_sha":"` + goodSHA + `"}]}`, "wait"},
		{"wait not a boolean", `{"environments":[{"name":"production","expected_sha":"` + goodSHA + `"}],"wait":"no"}`, "wait"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := post(r, tc.body)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body %s", w.Code, w.Body)
			}
			var e struct {
				Error string `json:"error"`
				Field string `json:"field"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
				t.Fatal(err)
			}
			if e.Field != tc.field || !strings.HasPrefix(e.Error, tc.field+": ") {
				t.Fatalf("error = %+v, want field %q", e, tc.field)
			}
		})
	}

	if w := post(r, `{"environments":`); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON status = %d", w.Code)
	}

	argvs, err := fakeengine.ReadArgvLog(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(argvs) != 0 {
		t.Fatalf("engine started %d times for bad input: %q", len(argvs), argvs)
	}
}

func TestDeployHandlerAnswersNormalisedRecord(t *testing.T) {
	r, argvLog := newTestDeploys(t)
	w := post(r, `{"environments":[{"name":"production","expected_sha":"`+goodSHA+`"}],"wait":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var job jobs.Job
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.State != jobs.StateDeployed || job.Results[0].DeployedSHA != goodSHA {
		t.Fatalf("job = %+v", job)
	}
	argvs, _ := fakeengine.ReadArgvLog(argvLog)
	if len(argvs) != 1 {
		t.Fatalf("engine ran %d times", len(argvs))
	}
}

func TestDeployListLimitValidation(t *testing.T) {
	r, _ := newTestDeploys(t)
	for _, q := range []string{"limit=0", "limit=101", "limit=x"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploys?"+q, nil))
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d", q, w.Code)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploys", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deploys":[]`) {
		t.Fatalf("empty list = %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploys/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d", w.Code)
	}
}

// TestAsyncDeployG10k is plan 57.6-06's tracer. The configuration is loaded
// through config.Init with r10k.use_g10k_commands on, so the engine and its
// argv come from upstream's g10k helper exactly as in production. The fake g10k
// replays lib/jobs/testdata/g10k-0.10.0/deploy-success.json, a verbatim copy of
// stagehand-sdk-public harness/fixtures/g10k-0.10.0/deploy-success.json (a real
// g10k 0.10.0 run recorded by the SDK harness), after sleeping, so the test can
// watch the job move from queued through deploying to deployed.
func TestAsyncDeployG10k(t *testing.T) {
	gin.SetMode(gin.TestMode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	basedir, stateDir := t.TempDir(), t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	fixture, _ := filepath.Abs("../lib/jobs/testdata/g10k-0.10.0/deploy-success.json")

	cfg := filepath.Join(t.TempDir(), "webhook.yml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`r10k:
  command_path: %q
  config_path: /etc/puppetlabs/r10k/g10k.yaml
  verbose: false
  use_g10k_commands: true
deploys:
  basedir: %q
  state_dir: %q
  timeout: 1m
  output_limit: 4096
`, exe, basedir, stateDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Init(&cfg)
	t.Cleanup(func() {
		neutral := "../lib/helpers/yaml/webhook.yaml"
		config.Init(&neutral)
	})

	t.Setenv(fakeengine.FixtureEnv, fixture)
	t.Setenv(fakeengine.BasedirEnv, basedir)
	t.Setenv(fakeengine.SHAEnv, goodSHA)
	t.Setenv(fakeengine.ArgvLogEnv, argvLog)
	t.Setenv(fakeengine.DelayEnv, "600ms")

	d, err := NewDeploysController(context.Background(), config.GetConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Shutdown(10 * time.Second) })
	r := gin.New()
	d.Register(r.Group("/api/v1"))

	start := time.Now()
	w := post(r, `{"environments":[{"name":"production","expected_sha":"`+goodSHA+`"}],"wait":false}`)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("POST wait:false took %s; it must answer at once", elapsed)
	}
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var accepted struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.ID == "" || accepted.State != "queued" {
		t.Fatalf("202 body = %s", w.Body)
	}
	if loc := w.Header().Get("Location"); loc != "/api/v1/deploys/"+accepted.ID {
		t.Fatalf("Location = %q", loc)
	}

	var seen []jobs.State
	var job jobs.Job
	deadline := time.Now().Add(15 * time.Second)
	for {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploys/"+accepted.ID, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET status = %d, body %s", w.Code, w.Body)
		}
		job = jobs.Job{}
		if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if len(seen) == 0 || seen[len(seen)-1] != job.State {
			seen = append(seen, job.State)
		}
		if job.State == jobs.StateDeployed || job.State == jobs.StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job still %s after 15s (states %v)", job.State, seen)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if seen[len(seen)-1] != jobs.StateDeployed {
		t.Fatalf("states %v, job %+v", seen, job)
	}
	sawDeploying := false
	for _, s := range seen {
		sawDeploying = sawDeploying || s == jobs.StateDeploying
	}
	if !sawDeploying {
		t.Fatalf("never observed deploying: %v", seen)
	}
	res := job.Results[0]
	if job.Engine != "g10k" || res.State != jobs.StateDeployed || res.DeployedSHA != goodSHA ||
		!res.MarkerPresent || !res.MarkerDeploySuccess || res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("job = %+v", job)
	}
	if !strings.Contains(res.Output, "Synced /srv/work/g10k/g10k.yaml with branch production") {
		t.Fatalf("output = %q", res.Output)
	}
	if _, err := os.Stat(filepath.Join(basedir, "production", ".g10k-deploy.json")); err != nil {
		t.Fatal(err)
	}

	argvs, err := fakeengine.ReadArgvLog(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-config=/etc/puppetlabs/r10k/g10k.yaml", "-branch=production"}
	if len(argvs) != 1 || strings.Join(argvs[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want exactly %q (an argv, no shell)", argvs, want)
	}

	// The record is a file under deploys.state_dir/jobs, so it outlives the process.
	if _, err := os.Stat(filepath.Join(stateDir, "jobs", accepted.ID+".json")); err != nil {
		t.Fatal(err)
	}
}
