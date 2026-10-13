package api

import (
	"bytes"
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
		{"wait false", `{"environments":[{"name":"production","expected_sha":"` + goodSHA + `"}],"wait":false}`, "wait"},
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
