//go:build unix

package jobs

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// inProcessGroup starts cmd as the leader of a new process group
// (SysProcAttr.Setpgid) and makes cancellation kill the whole group, so the
// git processes r10k and g10k start die with them (MH-04). Without it a
// timed-out engine's children keep running, keep the output pipe open and
// keep changing the environment after the job reported failure.
func inProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// The group id is the leader's pid; a negative pid signals the group.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
