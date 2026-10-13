//go:build unix

package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/voxpupuli/webhook-go/lib/jobs/fakeengine"
)

// TestEngineTimeoutKillsProcessGroup: the fake engine starts a child (as r10k
// starts git) and both sleep for a minute. With deploys.timeout 1s the runner
// must fail the environment "timed out after 1s" well within 3s, and the child
// must be gone too: only a kill of the whole process group does that (MH-04).
func TestEngineTimeoutKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv(fakeengine.ModeEnv, "hang")
	t.Setenv(fakeengine.PIDFileEnv, pidFile)
	r, _ := fakeEngine(t, "r10k", "", shaA, Options{Timeout: time.Second})

	start := time.Now()
	job, res := deployOneEnv(t, r, "production", shaA)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("deploy took %s; the timeout did not stop it", elapsed)
	}
	if job.State != StateFailed || res.State != StateFailed || !strings.Contains(res.Error, "timed out after 1s") {
		t.Fatalf("result = %s %q", res.State, res.Error)
	}

	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the child never started: %v", err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	// Killed, the orphan is reaped by init; wait for it to disappear.
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child %d still exists after the engine timed out (kill(0) = %v)", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
