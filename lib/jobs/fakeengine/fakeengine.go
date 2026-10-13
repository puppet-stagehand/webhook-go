// Package fakeengine lets a Go test binary stand in for r10k or g10k.
//
// It uses Go's helper-process pattern. A test points r10k.command_path at its
// own binary (os.Executable) and sets FixtureEnv to a fixture recorded by the
// Stagehand SDK harness (stagehand-sdk-public/harness/fixtures/<tool>/*.json).
// The test binary's TestMain calls MaybeRun first. When FixtureEnv is set,
// MaybeRun behaves like the recorded engine and exits; otherwise it returns
// and the tests run as normal:
//
//   - it records the argv it was started with (one JSON array per line in
//     ArgvLogEnv), so a test can prove the runner used an argv and no shell;
//   - it prints the fixture's stdout and stderr;
//   - it writes the fixture's marker_after into <BasedirEnv>/<env>/ (or leaves
//     the marker untouched when the fixture recorded none);
//   - it exits with the fixture's exit code.
//
// No shell scripts and no real engine are needed. This package is test
// infrastructure only; the server never imports it.
package fakeengine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
)

// Fixture is the subset of an SDK harness fixture the fake replays.
type Fixture struct {
	Tool        string         `json:"tool"`
	Scenario    string         `json:"scenario"`
	Argv        []string       `json:"argv"`
	ExitCode    int            `json:"exit_code"`
	Stdout      string         `json:"stdout"`
	Stderr      string         `json:"stderr"`
	MarkerAfter map[string]any `json:"marker_after"`
}

// MaybeRun turns the current process into the fake engine when FixtureEnv is
// set, and never returns in that case. Call it first thing in TestMain.
func MaybeRun() {
	path := os.Getenv(FixtureEnv)
	if path == "" {
		return
	}
	code, err := run(path, os.Args[1:])
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

func run(fixturePath string, args []string) (int, error) {
	f, err := LoadFixture(fixturePath)
	if err != nil {
		return 0, err
	}

	if log := os.Getenv(ArgvLogEnv); log != "" {
		line, _ := json.Marshal(args)
		fh, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, err
		}
		_, werr := fh.Write(append(line, '\n'))
		cerr := fh.Close()
		if werr != nil {
			return 0, werr
		}
		if cerr != nil {
			return 0, cerr
		}
	}

	sha := os.Getenv(SHAEnv)
	now := time.Now()
	fmt.Fprint(os.Stdout, replace(f.Stdout, sha, f.Tool, now))
	fmt.Fprint(os.Stderr, replace(f.Stderr, sha, f.Tool, now))

	env := environmentFromArgs(f.Tool, args)
	if f.MarkerAfter != nil && env != "" {
		basedir := os.Getenv(BasedirEnv)
		if basedir == "" {
			return 0, fmt.Errorf("%s is not set", BasedirEnv)
		}
		marker := replaceValue(f.MarkerAfter, sha, f.Tool, now)
		b, err := json.MarshalIndent(marker, "", "  ")
		if err != nil {
			return 0, err
		}
		dir := filepath.Join(basedir, env)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
		if err := os.WriteFile(filepath.Join(dir, MarkerFile(f.Tool)), b, 0o644); err != nil {
			return 0, err
		}
	}
	return f.ExitCode, nil
}

// MarkerFile is the deploy marker each engine writes into an environment.
func MarkerFile(tool string) string {
	if tool == "g10k" {
		return ".g10k-deploy.json"
	}
	return ".r10k-deploy.json"
}

// environmentFromArgs finds the environment an engine was asked to deploy.
// r10k: deploy environment <env> ...; g10k: -branch=<env> or -environment=<env>.
func environmentFromArgs(tool string, args []string) string {
	if tool == "g10k" {
		for _, a := range args {
			for _, p := range []string{"-branch=", "-environment="} {
				if strings.HasPrefix(a, p) {
					return strings.TrimPrefix(a, p)
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
