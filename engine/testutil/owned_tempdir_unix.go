//go:build unix

package testutil

import (
	"errors"
	"syscall"
)

// processExists reports whether a process with this id is running. Signal 0 delivers nothing and only checks: ESRCH means there is no such
// process, EPERM means there is one that this user may not signal, which is still a live process and must not have its directory swept.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
