// Package fakeengine lets a Go test binary stand in for r10k or g10k.
//
// It uses Go's helper-process pattern. A test points r10k.command_path at its
// own binary (os.Executable) and sets FixtureEnv to a fixture recorded by the
// Stagehand SDK harness (stagehand-sdk-public/harness/fixtures/<tool>/*.json).
// The test binary's TestMain calls MaybeRun first. When FixtureEnv or ModeEnv
// is set, MaybeRun behaves like the recorded engine and exits; otherwise it
// returns and the tests run as normal.
//
// Replaying a fixture, the fake:
//
//   - records the argv it was started with (one JSON array per line in
//     ArgvLogEnv), so a test can prove the runner used an argv and no shell;
//   - sleeps DelayEnv first, when set, so a test can watch a deploy in flight;
//   - prints the fixture's stdout and stderr;
//   - writes the fixture's marker_after into <BasedirEnv>/<env>/ only when the
//     recorded run changed the marker (extra.marker_changed, true when the
//     fixture does not say), and otherwise leaves the marker as it was;
//   - appends "<start> <end>" in Unix nanoseconds to TimesLogEnv, when set;
//   - exits with the fixture's exit code.
//
// The world before the recorded run (marker_before) is laid down by the test
// with SeedBefore, never by the fake: that is what lets a stale marker look
// stale.
//
// ModeEnv selects behaviour no fixture records: "flood" writes FloodBytesEnv
// bytes (default 1 MiB) and exits 0; "hang" starts a child, as r10k starts
// git, which inherits the fake's process group and writes its pid to
// PIDFileEnv, and both sleep for a minute.
//
// No shell scripts and no real engine are needed. This package is test
// infrastructure only; the server never imports it.
package fakeengine

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// FixtureEnv names the fixture JSON the fake replays. Unset means "not a fake".
	FixtureEnv = "WEBHOOK_GO_FAKE_ENGINE_FIXTURE"
	// BasedirEnv is the environments directory the fake writes its marker into.
	BasedirEnv = "WEBHOOK_GO_FAKE_ENGINE_BASEDIR"
	// SHAEnv replaces the fixture's <SHA> placeholder.
	SHAEnv = "WEBHOOK_GO_FAKE_ENGINE_SHA"
	// ArgvLogEnv is a file the fake appends its argv to, one JSON array per line.
	ArgvLogEnv = "WEBHOOK_GO_FAKE_ENGINE_ARGV_LOG"
	// DelayEnv is a Go duration the fake sleeps before replaying the fixture.
	DelayEnv = "WEBHOOK_GO_FAKE_ENGINE_DELAY"
	// TimesLogEnv is a file the fake appends "<start> <end>" (Unix nanoseconds) to.
	TimesLogEnv = "WEBHOOK_GO_FAKE_ENGINE_TIMES_LOG"
	// ModeEnv selects a behaviour no fixture records: "flood" or "hang".
	ModeEnv = "WEBHOOK_GO_FAKE_ENGINE_MODE"
	// FloodBytesEnv is how many bytes "flood" writes (default 1 MiB).
	FloodBytesEnv = "WEBHOOK_GO_FAKE_ENGINE_FLOOD_BYTES"
	// PIDFileEnv is where the child started by "hang" writes its pid.
	PIDFileEnv = "WEBHOOK_GO_FAKE_ENGINE_PIDFILE"
)

// Fixture is the subset of an SDK harness fixture the fake replays.
type Fixture struct {
	Tool         string         `json:"tool"`
	Scenario     string         `json:"scenario"`
	Argv         []string       `json:"argv"`
	ExitCode     int            `json:"exit_code"`
	Stdout       string         `json:"stdout"`
	Stderr       string         `json:"stderr"`
	ExpectLines  []string       `json:"expect_lines"`
	MarkerBefore map[string]any `json:"marker_before"`
	MarkerAfter  map[string]any `json:"marker_after"`
	Extra        map[string]any `json:"extra"`
}

// MarkerChanged reports whether the recorded run rewrote the marker. Fixtures
// that do not say (deploy-success) wrote one.
func (f Fixture) MarkerChanged() bool {
	if v, ok := f.Extra["marker_changed"].(bool); ok {
		return v
	}
	return f.MarkerAfter != nil
}

// MaybeRun turns the current process into the fake engine when FixtureEnv or
// ModeEnv is set, and never returns in that case. Call it first thing in
// TestMain.
func MaybeRun() {
	mode, path := os.Getenv(ModeEnv), os.Getenv(FixtureEnv)
	if mode == "" && path == "" {
		return
	}
	var code int
	var err error
	switch mode {
	case "":
		code, err = run(path, os.Args[1:])
	case "flood":
		code, err = flood()
	case "hang":
		code, err = hang()
	case "child":
		code, err = child()
	default:
		err = fmt.Errorf("unknown %s %q", ModeEnv, mode)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeengine: %v\n", err)
		os.Exit(97)
	}
	os.Exit(code)
}

// LoadFixture reads a fixture file.
func LoadFixture(path string) (Fixture, error) {
	var f Fixture
	b, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("parse %s: %w", path, err)
	}
	return f, nil
}

// SeedBefore lays down the world the fixture was recorded in: its
// marker_before, if any, in <basedir>/<marker name>/, with sha for <SHA> and
// at for <TS>. A fixture without marker_before seeds nothing.
func SeedBefore(fixturePath, basedir, sha string, at time.Time) error {
	f, err := LoadFixture(fixturePath)
	if err != nil {
		return err
	}
	if f.MarkerBefore == nil {
		return nil
	}
	name, _ := f.MarkerBefore["name"].(string)
	if name == "" {
		return fmt.Errorf("%s: marker_before has no name", fixturePath)
	}
	return writeMarker(basedir, name, f.Tool, replaceValue(f.MarkerBefore, sha, f.Tool, at))
}

// ReadArgvLog returns every argv the fake recorded in log, oldest first.
func ReadArgvLog(log string) ([][]string, error) {
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			return nil, err
		}
		out = append(out, argv)
	}
	return out, nil
}

// Span is one fake run's start and end.
type Span struct{ Start, End time.Time }

// ReadTimesLog returns every span the fake recorded in log, in write order.
func ReadTimesLog(log string) ([]Span, error) {
	b, err := os.ReadFile(log)
	if err != nil {
		return nil, err
	}
	var out []Span
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var s, e int64
		if _, err := fmt.Sscanf(line, "%d %d", &s, &e); err != nil {
			return nil, fmt.Errorf("times log line %q: %w", line, err)
		}
		out = append(out, Span{time.Unix(0, s), time.Unix(0, e)})
	}
	return out, nil
}

func appendLine(path string, line []byte) error {
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := fh.Write(append(line, '\n'))
	cerr := fh.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

func run(fixturePath string, args []string) (int, error) {
	start := time.Now()
	f, err := LoadFixture(fixturePath)
	if err != nil {
		return 0, err
	}

	if log := os.Getenv(ArgvLogEnv); log != "" {
		line, _ := json.Marshal(args)
		if err := appendLine(log, line); err != nil {
			return 0, err
		}
	}
	if d := os.Getenv(DelayEnv); d != "" {
		delay, err := time.ParseDuration(d)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", DelayEnv, err)
		}
		time.Sleep(delay)
	}

	sha := os.Getenv(SHAEnv)
	now := time.Now()
	fmt.Fprint(os.Stdout, replace(f.Stdout, sha, f.Tool, now))
	fmt.Fprint(os.Stderr, replace(f.Stderr, sha, f.Tool, now))

	env := environmentFromArgs(f.Tool, args)
	if f.MarkerAfter != nil && f.MarkerChanged() && env != "" {
		basedir := os.Getenv(BasedirEnv)
		if basedir == "" {
			return 0, fmt.Errorf("%s is not set", BasedirEnv)
		}
		if err := writeMarker(basedir, env, f.Tool, replaceValue(f.MarkerAfter, sha, f.Tool, now)); err != nil {
			return 0, err
		}
	}

	if log := os.Getenv(TimesLogEnv); log != "" {
		if err := appendLine(log, []byte(fmt.Sprintf("%d %d", start.UnixNano(), time.Now().UnixNano()))); err != nil {
			return 0, err
		}
	}
	return f.ExitCode, nil
}

func writeMarker(basedir, env, tool string, marker any) error {
	b, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(basedir, env)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, MarkerFile(tool)), b, 0o644)
}

// flood writes FloodBytesEnv bytes of output, ending with a recognisable line.
func flood() (int, error) {
	n := 1 << 20
	if s := os.Getenv(FloodBytesEnv); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", FloodBytesEnv, err)
		}
		n = v
	}
	line := []byte(strings.Repeat("x", 1023) + "\n")
	for written := 0; written < n; written += len(line) {
		if _, err := os.Stdout.Write(line); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

// hang starts a child like git under r10k and then sleeps; only a kill of the
// whole process group stops both in time. The child shares this process's
// stdout, so the runner's output pipe stays open until the child dies too.
func hang() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), ModeEnv+"=child")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	fmt.Println("fake engine: started a child that sleeps")
	time.Sleep(time.Minute)
	return 0, nil
}

func child() (int, error) {
	if p := os.Getenv(PIDFileEnv); p != "" {
		if err := os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			return 0, err
		}
	}
	time.Sleep(time.Minute)
	return 0, nil
}

// MarkerFile is the deploy marker each engine writes into an environment.
func MarkerFile(tool string) string {
	if tool == "g10k" {
		return ".g10k-deploy.json"
	}
	return ".r10k-deploy.json"
}

// environmentFromArgs finds the environment an engine was asked to deploy.
// r10k: deploy environment <env> ...; g10k: -branch=<env> or -environment=<env>
// (upstream's helper), or -branch <env> as the SDK harness recorded it.
func environmentFromArgs(tool string, args []string) string {
	if tool == "g10k" {
		for i, a := range args {
			for _, p := range []string{"-branch", "-environment"} {
				if strings.HasPrefix(a, p+"=") {
					return strings.TrimPrefix(a, p+"=")
				}
				if a == p && i+1 < len(args) {
					return args[i+1]
				}
			}
		}
		return ""
	}
	if len(args) >= 3 && args[0] == "deploy" && args[1] == "environment" {
		return args[2]
	}
	return ""
}

// timestamp renders now the way each engine writes marker times: r10k (Ruby
// Time#to_s) "2006-01-02 15:04:05 -0700", g10k (Go time.Time) RFC 3339.
func timestamp(tool string, now time.Time) string {
	if tool == "g10k" {
		return now.Format(time.RFC3339Nano)
	}
	return now.Format("2006-01-02 15:04:05 -0700")
}

func replace(s, sha, tool string, now time.Time) string {
	s = strings.ReplaceAll(s, "<SHA>", sha)
	return strings.ReplaceAll(s, "<TS>", timestamp(tool, now))
}

func replaceValue(v any, sha, tool string, now time.Time) any {
	switch x := v.(type) {
	case string:
		return replace(x, sha, tool, now)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = replaceValue(x[i], sha, tool, now)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = replaceValue(val, sha, tool, now)
		}
		return out
	default:
		return v
	}
}
