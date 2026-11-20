//go:build unix

package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A process id that is certainly dead: a child that has been started and waited for. (A made-up number could be alive, which would make the
// sweep tests below pass or fail on the machine's luck.)
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("could not start and reap a child process to get a dead pid: %v", err)
	}
	return cmd.Process.Pid
}

// mark makes a directory with a file in it, so that "removed" means the whole tree went and not an empty shell.
func mark(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "binary"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// TestOwnedTempDirSweepsOnlyWhatADeadProcessLeft pins the three rules the sweep has to keep (cleat#4226). Each case is a directory that the sweep
// has to treat a particular way, and each is named for the rule it decides: removing a rule makes its own case fail and no other.
func TestOwnedTempDirSweepsOnlyWhatADeadProcessLeft(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	const prefix = "owned-test-"
	dead := deadPID(t)

	leftByDead := mark(t, root, fmt.Sprintf("%sp%d-aaaa", prefix, dead))
	liveOwner := mark(t, root, fmt.Sprintf("%sp%d-bbbb", prefix, os.Getppid()))
	ownDir := mark(t, root, fmt.Sprintf("%sp%d-cccc", prefix, os.Getpid()))
	oldFormat := mark(t, root, prefix+"4268830989") // what os.MkdirTemp(prefix+"*") made before this function existed: digits, never "p"
	otherPrefix := mark(t, root, fmt.Sprintf("someone-elses-p%d-dddd", dead))
	noDash := mark(t, root, fmt.Sprintf("%sp%d", prefix, dead))
	noMarker := mark(t, root, fmt.Sprintf("%s%d-abc", prefix, dead)) // digits and a dash, but no "p": not a name this function makes
	notAPid := mark(t, root, prefix+"pnotanumber-eeee")
	// A symlink named like one, pointing at a directory that is NOT in the temp dir and must survive whatever the sweep does to the link.
	elsewhere := mark(t, t.TempDir(), "precious")
	aLink := filepath.Join(root, fmt.Sprintf("%sp%d-gggg", prefix, dead))
	if err := os.Symlink(elsewhere, aLink); err != nil {
		t.Fatal(err)
	}
	aFile := filepath.Join(root, fmt.Sprintf("%sp%d-ffff", prefix, dead))
	if err := os.WriteFile(aFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := OwnedTempDir(prefix)
	if err != nil {
		t.Fatal(err)
	}

	if exists(leftByDead) {
		t.Errorf("the directory a DEAD process left was not removed: %s", leftByDead)
	}
	for name, p := range map[string]string{
		"a LIVE process's directory (its parent here: a concurrent run keeps its binary)": liveOwner,
		"this process's own directory":                                      ownDir,
		"an old unmarked directory (digits after the prefix)":               oldFormat,
		"a directory with another prefix":                                   otherPrefix,
		"a name with no dash after the pid":                                 noDash,
		"a name with a number and a dash but no \"p\" marker":               noMarker,
		"a name whose pid is not a number":                                  notAPid,
		"a FILE named like one":                                             aFile,
		"a SYMLINK named like one (the sweep must not follow or remove it)": aLink,
		"the directory that symlink points at":                              elsewhere,
	} {
		if !exists(p) {
			t.Errorf("the sweep removed %s: %s", name, p)
		}
	}

	// The directory it hands back is new, empty, under TMPDIR, and carries this process's id so that the next run can tell it is alive.
	if filepath.Dir(got) != root {
		t.Errorf("OwnedTempDir returned %s, not a child of TMPDIR %s", got, root)
	}
	if want := fmt.Sprintf("%sp%d-", prefix, os.Getpid()); !strings.HasPrefix(filepath.Base(got), want) {
		t.Errorf("OwnedTempDir returned %s, whose name does not begin %s", filepath.Base(got), want)
	}
	if entries, _ := os.ReadDir(got); len(entries) != 0 {
		t.Errorf("OwnedTempDir returned a directory that is not empty: %d entries", len(entries))
	}
}

// TestOwnedTempDirSweepRunsOnEveryCall is the "a directory abandoned between two runs is gone after the second" half, stated the way the leak
// happened: one process dies after making its directory, a later process asks for its own.
func TestOwnedTempDirSweepRunsOnEveryCall(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	const prefix = "owned-twice-"
	first := mark(t, root, fmt.Sprintf("%sp%d-1111", prefix, deadPID(t)))
	second := mark(t, root, fmt.Sprintf("%sp%d-2222", prefix, deadPID(t)))
	if _, err := OwnedTempDir(prefix); err != nil {
		t.Fatal(err)
	}
	if exists(first) || exists(second) {
		t.Errorf("both abandoned directories should be gone: first exists=%v second exists=%v", exists(first), exists(second))
	}
}

func TestOwnerPID(t *testing.T) {
	for name, c := range map[string]struct {
		in     string
		pid    int
		wantOK bool
	}{
		"a name this package makes":  {"v-p1234-abc", 1234, true},
		"the unmarked old form":      {"v-4268830989", 0, false},
		"a different prefix":         {"w-p1234-abc", 0, false},
		"no dash after the digits":   {"v-p1234", 0, false},
		"empty digits":               {"v-p-abc", 0, false},
		"a pid that is not a number": {"v-p12x4-abc", 0, false},
		"zero is not a process":      {"v-p0-abc", 0, false},
		"negative is not a process":  {"v-p-5-abc", 0, false},
		"the prefix alone":           {"v-", 0, false},
	} {
		pid, ok := ownerPID(c.in, "v-")
		if ok != c.wantOK || pid != c.pid {
			t.Errorf("%s: ownerPID(%q) = %d, %v; want %d, %v", name, c.in, pid, ok, c.pid, c.wantOK)
		}
	}
}

// TestOwnedTempDirKeepsADirectoryWhoseOwnerWeMayNotSignal pins the EPERM branch of processExists (cleat#4234). kill(pid, 0) answers EPERM for a
// live process this user may not signal, and a sweep that read that as "no such process" would remove a directory belonging to a running
// binary. Every other test here uses a pid this user CAN signal (its parent, itself), so reading EPERM as dead left all of them green.
//
// Process 1 is the live process that is not ours. The test checks that it really answers EPERM before relying on it, and says so when it does
// not: as root, signal 0 to it succeeds, and the case then measures nothing about EPERM.
func TestOwnedTempDirKeepsADirectoryWhoseOwnerWeMayNotSignal(t *testing.T) {
	if err := syscall.Kill(1, 0); err != syscall.EPERM {
		t.Skipf("kill(1, 0) = %v here, not EPERM (running as root, or no pid 1 we can see): this case measures nothing about the EPERM branch", err)
	}
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	const prefix = "owned-eperm-"
	kept := mark(t, root, prefix+"p1-live")
	if _, err := OwnedTempDir(prefix); err != nil {
		t.Fatal(err)
	}
	if !exists(kept) {
		t.Errorf("the directory of a live process this user may not signal was swept: EPERM was read as \"no such process\"")
	}
}
