package jobs

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/voxpupuli/webhook-go/lib/jobs/fakeengine"
)

// holdLockEnv turns the test binary into a second process that holds the
// deploy lock: it prints "locked", holds the lock until its stdin closes, then
// releases it and exits.
const holdLockEnv = "WEBHOOK_GO_TEST_HOLD_DEPLOY_LOCK"

func maybeHoldLock() {
	path := os.Getenv(holdLockEnv)
	if path == "" {
		return
	}
	release, err := NewDeployLock(path).Acquire(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(98)
	}
	fmt.Println("locked")
	io.Copy(io.Discard, os.Stdin)
	release()
	os.Exit(0)
}

// assertNoOverlap fails if any two recorded engine runs overlapped.
func assertNoOverlap(t *testing.T, spans []fakeengine.Span, want int) {
	t.Helper()
	if len(spans) != want {
		t.Fatalf("%d engine runs recorded, want %d", len(spans), want)
	}
	sort.Slice(spans, func(a, b int) bool { return spans[a].Start.Before(spans[b].Start) })
	for i := 1; i < len(spans); i++ {
		if spans[i].Start.Before(spans[i-1].End) {
			t.Fatalf("engine runs overlap: %v-%v and %v-%v", spans[i-1].Start, spans[i-1].End, spans[i].Start, spans[i].End)
		}
	}
}

func TestDeploysAreSerialised(t *testing.T) {
	fixture := "testdata/r10k-5.0.3/deploy-success.json"

	t.Run("two jobs on one runner", func(t *testing.T) {
		timesLog := filepath.Join(t.TempDir(), "times.log")
		t.Setenv(fakeengine.TimesLogEnv, timesLog)
		t.Setenv(fakeengine.DelayEnv, "300ms")
		r, _ := fakeEngine(t, "r10k", fixture, shaA, Options{LockPath: filepath.Join(t.TempDir(), "deploy.lock")})
		for _, env := range []string{"production", "staging"} {
			job, err := r.NewJob([]EnvRequest{{Name: env, ExpectedSHA: shaA}})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Start(job); err != nil {
				t.Fatal(err)
			}
		}
		r.wg.Wait()
		spans, err := fakeengine.ReadTimesLog(timesLog)
		if err != nil {
			t.Fatal(err)
		}
		assertNoOverlap(t, spans, 2)
	})

	// Two runners with their own in-process locks stand for two webhook-go
	// processes: only the flock on the shared lock file keeps them apart.
	t.Run("two runners sharing deploy.lock", func(t *testing.T) {
		timesLog := filepath.Join(t.TempDir(), "times.log")
		lockPath := filepath.Join(t.TempDir(), "deploy.lock")
		t.Setenv(fakeengine.TimesLogEnv, timesLog)
		t.Setenv(fakeengine.DelayEnv, "300ms")
		r1, _ := fakeEngine(t, "r10k", fixture, shaA, Options{LockPath: lockPath})
		r2, _ := fakeEngine(t, "r10k", fixture, shaA, Options{LockPath: lockPath})
		for _, r := range []*Runner{r1, r2, r1, r2} {
			job, err := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Start(job); err != nil {
				t.Fatal(err)
			}
		}
		r1.wg.Wait()
		r2.wg.Wait()
		spans, err := fakeengine.ReadTimesLog(timesLog)
		if err != nil {
			t.Fatal(err)
		}
		assertNoOverlap(t, spans, 4)
	})

	t.Run("a second process blocks until the first releases", func(t *testing.T) {
		lockPath := filepath.Join(t.TempDir(), "deploy.lock")
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		holder := exec.Command(exe)
		holder.Env = append(os.Environ(), holdLockEnv+"="+lockPath)
		stdin, err := holder.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := holder.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		holder.Stderr = os.Stderr
		if err := holder.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil || line != "locked\n" {
			t.Fatalf("holder said %q, %v", line, err)
		}

		acquired := make(chan func(), 1)
		go func() {
			release, err := NewDeployLock(lockPath).Acquire(context.Background())
			if err != nil {
				t.Error(err)
				close(acquired)
				return
			}
			acquired <- release
		}()
		select {
		case <-acquired:
			t.Fatal("acquired the deploy lock while another process held it")
		case <-time.After(400 * time.Millisecond):
		}

		stdin.Close() // the holder releases and exits
		select {
		case release, ok := <-acquired:
			if !ok {
				t.Fatal("acquire failed")
			}
			release()
		case <-time.After(5 * time.Second):
			t.Fatal("the lock was not acquired after the other process released it")
		}
	})
}

func TestDeployLockAcquireHonoursContext(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "deploy.lock")
	release, err := NewDeployLock(lockPath).Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := NewDeployLock(lockPath).Acquire(ctx); err == nil {
		t.Fatal("acquired a held lock")
	}
	st, err := os.Stat(lockPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}
}

// A job that waits for the lock while the service shuts down fails at once
// instead of hanging, and records why.
func TestQueuedJobFailsOnShutdown(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "deploy.lock")
	release, err := NewDeployLock(lockPath).Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	r, _ := fakeEngine(t, "r10k", "testdata/r10k-5.0.3/deploy-success.json", shaA, Options{LockPath: lockPath})
	job, _ := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	if err := r.Start(job); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := r.Shutdown(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	got, err := r.Store().Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateFailed || got.Results[0].State != StateFailed || got.Results[0].Error != "not run: "+ErrShutdown.Error() {
		t.Fatalf("job = %+v", got)
	}
	if err := r.Start(job); err != ErrShutdown {
		t.Fatalf("Start after Shutdown = %v, want ErrShutdown", err)
	}
}
