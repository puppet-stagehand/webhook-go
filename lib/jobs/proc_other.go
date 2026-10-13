//go:build !unix

package jobs

import "os/exec"

// inProcessGroup: process groups are a Unix feature. Elsewhere cancellation
// kills only the engine itself (exec.CommandContext's default). The deploy
// API is meant for Linux Puppet primaries.
func inProcessGroup(cmd *exec.Cmd) {}
