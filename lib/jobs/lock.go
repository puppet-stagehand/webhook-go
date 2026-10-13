package jobs

import (
	"context"
	"fmt"
	"os"
	"time"
)

// lockPollInterval is how often a waiting Acquire retries the file lock.
const lockPollInterval = 50 * time.Millisecond

// DeployLock lets one deploy run at a time (MH-03). Inside the process a
// one-slot semaphore (a mutex that a cancelled caller can stop waiting for)
// orders jobs; across processes an exclusive Flock on the lock file, normally
// <state_dir>/deploy.lock, keeps a second webhook-go on the same host out.
// r10k has no deploy lock of its own (its deploy.write_lock is an admin
// switch), and two engines writing one code directory corrupt it.
//
// A flock belongs to the open file, so it is released when the process dies,
// however it dies; and Go opens files close-on-exec, so an engine never
// inherits it.
type DeployLock struct {
	path string
	slot chan struct{}
}

// NewDeployLock returns a lock on path. An empty path locks within this
// process only, for tests that need no file.
func NewDeployLock(path string) *DeployLock {
	return &DeployLock{path: path, slot: make(chan struct{}, 1)}
}

// Acquire waits until it holds the lock, or ctx is done. release gives it up.
func (l *DeployLock) Acquire(ctx context.Context) (release func(), err error) {
	select {
	case l.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if l.path == "" {
		return func() { <-l.slot }, nil
	}
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		<-l.slot
		return nil, fmt.Errorf("deploy lock: %w", err)
	}
	for {
		ok, err := tryFlockExclusive(f)
		if err != nil {
			f.Close()
			<-l.slot
			return nil, fmt.Errorf("deploy lock %s: %w", l.path, err)
		}
		if ok {
			break
		}
		select {
		case <-time.After(lockPollInterval):
		case <-ctx.Done():
			f.Close()
			<-l.slot
			return nil, ctx.Err()
		}
	}
	return func() {
		unflock(f)
		f.Close()
		<-l.slot
	}, nil
}
