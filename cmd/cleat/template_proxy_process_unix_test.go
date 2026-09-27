//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// inOwnProcessGroup puts cmd in a process group of its own, so that killing the group takes `make`, the `go run`
// it started and the proxy binary that `go run` started, and nothing else.
func inOwnProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}
}
