//go:build !unix

package jobs

import (
	"errors"
	"os"
)

// errNoFlock: the deploy API is meant for Linux Puppet primaries. Elsewhere a
// deploy fails closed rather than run without the cross-process lock.
var errNoFlock = errors.New("flock is not available on this platform")

func tryFlockExclusive(f *os.File) (bool, error) { return false, errNoFlock }

func unflock(f *os.File) error { return nil }
