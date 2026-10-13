//go:build unix

package jobs

import (
	"errors"
	"os"
	"syscall"
)

// tryFlockExclusive takes an exclusive flock on f without blocking. It
// reports false when another open file holds it.
func tryFlockExclusive(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EINTR):
		return false, nil
	}
	return false, err
}

func unflock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
