package server

// The tracer for the Stagehand deploy API: a YAML config is loaded through
// config.Init, the real router is served by newMTLSServer, a client holding an
// allowed certificate POSTs a deploy, and the fake r10k (lib/jobs/fakeengine)
// replays the SDK harness fixture lib/jobs/testdata/r10k-5.0.3/deploy-success.json
// (a verbatim copy of stagehand-sdk-public harness/fixtures/r10k-5.0.3/deploy-success.json).
// It lives in package server because only this package can build the mTLS server.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxpupuli/webhook-go/config"
	"github.com/voxpupuli/webhook-go/lib/jobs/fakeengine"
)

const tracerSHA = "0123456789abcdef0123456789abcdef01234567"

// deployRecord mirrors the JSON contract of /api/v1/deploys.
type deployRecord struct {
	ID      string `json:"id"`
	Engine  string `json:"engine"`
	State   string `json:"state"`
	Results []struct {
		Environment         string `json:"environment"`
		State               string `json:"state"`
		ExitCode            int    `json:"exit_code"`
		ExpectedSHA         string `json:"expected_sha"`
		DeployedSHA         string `json:"deployed_sha"`
		MarkerPresent       bool   `json:"marker_present"`
		MarkerDeploySuccess bool   `json:"marker_deploy_success"`
		StartedAt           string `json:"started_at"`
		FinishedAt          string `json:"finished_at"`
		DurationMS          *int64 `json:"duration_ms"`
		Output              string `json:"output"`
		OutputTruncated     bool   `json:"output_truncated"`
		Error               string `json:"error"`
	} `json:"results"`
}

type tracer struct {
	f       *mtlsFixture
	url     string
	client  *http.Client
	argvLog string
	basedir string
}

// loadMTLSConfig writes the fixture's mTLS settings plus extra YAML to a file
// and loads it through config.Init, exactly as the server does at start.
func loadMTLSConfig(t *testing.T, f *mtlsFixture, extra string) {
	t.Helper()
	allowed, _ := json.Marshal(f.conf.Server.TLS.AllowedCertnames)
	y := fmt.Sprintf(`server:
  port: 4000
  tls:
    enabled: true
    certificate: %q
    key: %q
    client_ca: %q
    allowed_certnames: %s
%s`, f.conf.Server.TLS.Certificate, f.conf.Server.TLS.Key, f.conf.Server.TLS.ClientCA, allowed, extra)
	p := writeFile(t, f.dir, "webhook.yml", []byte(y))
	config.Init(&p)
	t.Cleanup(func() {
		neutral := "../lib/helpers/yaml/webhook.yaml"
		config.Init(&neutral)
	})
}

func newTracer(t *testing.T) *tracer {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := newMTLSFixture(t, "stagehand-code-manager")
	basedir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	fixture, _ := filepath.Abs("../lib/jobs/testdata/r10k-5.0.3/deploy-success.json")

	loadMTLSConfig(t, f, fmt.Sprintf(`r10k:
  command_path: %q
  config_path: /etc/puppetlabs/r10k/r10k.yaml
  verbose: false
  generate_types: false
deploys:
  basedir: %q
  timeout: 1m
`, exe, basedir))

	t.Setenv(fakeengine.FixtureEnv, fixture)
	t.Setenv(fakeengine.BasedirEnv, basedir)
	t.Setenv(fakeengine.SHAEnv, tracerSHA)
	t.Setenv(fakeengine.ArgvLogEnv, argvLog)

	tr := &tracer{f: f, argvLog: argvLog, basedir: basedir}
	tr.url = f.serve(t, NewRouter())
	tr.client = f.client(t, clientCert(t, f.ca, "stagehand-code-manager"))
	return tr
}

func (tr *tracer) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, tr.url+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := tr.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (tr *tracer) deployProduction(t *testing.T) deployRecord {
	t.Helper()
	code, body := tr.do(t, http.MethodPost, "/api/v1/deploys",
		`{"environments":[{"name":"production","expected_sha":"`+tracerSHA+`"}],"wait":true}`)
	if code != http.StatusOK {
		t.Fatalf("POST status = %d, body %s", code, body)
	}
	var rec deployRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return rec
}

func TestDeployOneEnvironmentR10k(t *testing.T) {
	tr := newTracer(t)
	rec := tr.deployProduction(t)

	if rec.ID == "" || rec.Engine != "r10k" || rec.State != "deployed" || len(rec.Results) != 1 {
		t.Fatalf("record = %+v", rec)
	}
	r := rec.Results[0]
	if r.Environment != "production" || r.State != "deployed" || r.ExitCode != 0 ||
		r.ExpectedSHA != tracerSHA || r.DeployedSHA != tracerSHA ||
		!r.MarkerPresent || !r.MarkerDeploySuccess || r.Error != "" || r.OutputTruncated {
		t.Fatalf("result = %+v", r)
	}
	if r.StartedAt == "" || r.FinishedAt == "" || r.DurationMS == nil {
		t.Fatalf("timings missing: %+v", r)
	}
	if !strings.Contains(r.Output, "Environment production is now at "+tracerSHA) {
		t.Fatalf("output = %q", r.Output)
	}

	argvs, err := fakeengine.ReadArgvLog(tr.argvLog)
	if err != nil {
		t.Fatal(err)
	}
	// One invocation, started directly with an argv: a shell would have been
	// handed "-c" and a command string instead.
	if len(argvs) != 1 {
		t.Fatalf("engine ran %d times", len(argvs))
	}
	want := []string{"deploy", "environment", "production", "--config=/etc/puppetlabs/r10k/r10k.yaml"}
	if strings.Join(argvs[0][:4], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want prefix %q", argvs[0], want)
	}
}

func TestGetDeployByIDAndList(t *testing.T) {
	tr := newTracer(t)
	rec := tr.deployProduction(t)

	code, body := tr.do(t, http.MethodGet, "/api/v1/deploys/"+rec.ID, "")
	if code != http.StatusOK {
		t.Fatalf("GET by id status = %d, body %s", code, body)
	}
	var got deployRecord
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != rec.ID || got.State != rec.State || len(got.Results) != 1 ||
		got.Results[0].DeployedSHA != rec.Results[0].DeployedSHA || got.Results[0].Output != rec.Results[0].Output {
		t.Fatalf("GET by id = %+v, want %+v", got, rec)
	}

	code, body = tr.do(t, http.MethodGet, "/api/v1/deploys?limit=1", "")
	if code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", code, body)
	}
	var list struct {
		Deploys []deployRecord `json:"deploys"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Deploys) != 1 || list.Deploys[0].ID != rec.ID {
		t.Fatalf("list = %s", body)
	}

	code, _ = tr.do(t, http.MethodGet, "/api/v1/deploys/00000000-0000-0000-0000-000000000000", "")
	if code != http.StatusNotFound {
		t.Fatalf("unknown id status = %d, want 404", code)
	}
}
