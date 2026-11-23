package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// OwnedTempDir makes a temp directory for a test binary's build output, named after the process that made it, and first removes the
// directories that EARLIER processes made with the same prefix and died without removing (cleat#4226).
//
// Why a sweep and not a TestMain that removes the directory. A TestMain already did: cmd/cleat's removed its directory after m.Run, and a
// normal run left nothing behind (measured 2026-11-20: 1 directory before, 1 after). What leaked was every run that DID NOT END NORMALLY: a
// SIGKILL, a harness timeout, a `go test -timeout` panic. None of those runs any code in the dying process, so nothing inside it can clean
// up after itself. Killed after the build, each left a 34 MB binary, and over twelve days that was 922 directories and 31.5 GB, with the
// chaos harness's own build directory adding 218 more and 16 GB.
//
// So the NEXT process cleans up for the dead one. A directory is removed only when ALL of these hold, so that it can never take another
// run's working files:
//
//   - its name is prefix + "p" + <pid> + "-" + something, which only this function produces (the old unmarked names are digits after the
//     prefix and never begin with "p", so a directory made before this function existed is left alone, as is anything else in the temp dir);
//   - it is a directory;
//   - the process named in it no longer exists. A process that does exist is never swept, which covers this process's own directories and a
//     concurrent run in another worktree alike, so neither loses its binary. A pid that was reused by an unrelated live process makes a
//     directory look alive and it is left, which costs disk and nothing else.
//
// prefix should end in "-", as os.MkdirTemp's pattern does ("cleat-vet-test-").
func OwnedTempDir(prefix string) (string, error) {
	sweepDeadOwners(os.TempDir(), prefix)
	return os.MkdirTemp("", fmt.Sprintf("%sp%d-*", prefix, os.Getpid()))
}

// sweepDeadOwners removes the directories under dir that OwnedTempDir made with prefix for processes that no longer exist. Every failure is
// ignored: a sweep that cannot remove something (another user's directory in a shared /tmp) must not fail the run that asked for a build
// directory.
func sweepDeadOwners(dir, prefix string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, ok := ownerPID(e.Name(), prefix)
		if !ok || processExists(pid) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// ownerPID reads the process id out of a name OwnedTempDir produced: prefix + "p" + digits + "-" + rest. Anything else is not ours.
func ownerPID(name, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(name, prefix+"p")
	if !ok {
		return 0, false
	}
	digits, _, ok := strings.Cut(rest, "-")
	if !ok || digits == "" {
		return 0, false
	}
	pid, err := strconv.Atoi(digits)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}
