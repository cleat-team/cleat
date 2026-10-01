package testutil

// cleat#2857: selfHealMSSQLAdminPredicate's old guard (mssqlAdminRefs, a
// per-process in-memory map) could not tell a dead process's leftover
// 'admin' state from a different, still-LIVE process's. Reproducing that --
// and proving the sp_getapplock fix actually closes it -- needs two
// independently-launched OS processes sharing one SQL Server, which a single
// `go test` binary cannot be on its own. This file is the real, in-tree
// version of the scratch probe cleat-review used during cleat#2831's review
// (TestZZProbeHoldAdmin / TestZZProbeSetupOnly, named in cleat#2857 itself):
// a compiled test binary, launched twice as separate processes.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// crossProcessAdminHeldMarker is what the holder process prints to its own
// real stdout -- not t.Logf, which buffers and is discarded on a passing
// test -- the instant it has acquired MSSQLAdminDB, so the driving test
// knows precisely when it is safe to start the second process rather than
// guessing from a sleep.
const crossProcessAdminHeldMarker = "CLEAT_2857_ADMIN_HELD"

// crossProcessHoldSeconds is how long the holder keeps MSSQLAdminDB after
// printing the marker -- long enough for the driving test to launch and wait
// out the second process in the meantime, short enough not to make this
// test slow. 6s against the setup-only process's typical sub-second run is
// ample margin.
const crossProcessHoldSeconds = 6

// TestZZCrossProcessHoldAdminWorker is the holder half. Launched by
// TestMSSQLAdminSelfHealDoesNotClobberALiveCrossProcessCaller as its own OS
// process; also runnable (and harmless) on its own, which is why it is a
// real, selectable test rather than something gated behind an extra env
// var -- `-test.run` picking it out precisely is isolation enough, the same
// posture every other test in this file already takes on CLEAT_TEST_MSSQL.
func TestZZCrossProcessHoldAdminWorker(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := MSSQLTestDB(t)
	SetupMSSQLFullSchema(t, db)
	_ = MSSQLAdminDB(t, db)

	fmt.Println(crossProcessAdminHeldMarker)
	time.Sleep(crossProcessHoldSeconds * time.Second)

	var form string
	if err := db.QueryRow(`SELECT form FROM admin.rls_predicate_form`).Scan(&form); err != nil {
		t.Fatalf("read admin.rls_predicate_form: %v", err)
	}
	if form != "admin" {
		t.Fatalf("predicate form while this process's MSSQLAdminDB call was still live = %q, "+
			"want \"admin\" -- a SECOND process's schema setup (TestZZCrossProcessSetupOnlyWorker) "+
			"restored 'plain' out from under this one, which is exactly cleat#2857", form)
	}
}

// TestZZCrossProcessSetupOnlyWorker is the second process: ordinary schema
// setup, which is what every MSSQL test does first and is exactly the path
// selfHealMSSQLAdminPredicate runs on. It does not itself assert anything --
// the holder (above) is where the defect would show, and this process's own
// exit code only confirms it ran to completion without an unrelated error.
func TestZZCrossProcessSetupOnlyWorker(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := MSSQLTestDB(t)
	SetupMSSQLFullSchema(t, db)
}

// buildTestutilTestBinary compiles this package's test binary once, shared
// by every cross-process driver in this file -- each driver launches it
// twice, under different -test.run selectors, as two real OS processes.
func buildTestutilTestBinary(t *testing.T) string {
	t.Helper()
	root := repoRootForMSSQLTestutil(t)
	binPath := t.TempDir() + "/testutil.test"
	build := exec.Command("go", "test", "-c", "-o", binPath, "./engine/testutil")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go test -c: %v\n%s", err, out)
	}
	return binPath
}

// TestMSSQLAdminSelfHealDoesNotClobberALiveCrossProcessCaller drives the two
// workers above as real, separate OS processes against one SQL Server --
// cleat-review's own method for cleat#2831's review, now in-tree rather than
// scratch code. Skips in -short (it shells out to `go test -c`, which is
// slow) and whenever CLEAT_TEST_MSSQL is unset, same as every worker above.
func TestMSSQLAdminSelfHealDoesNotClobberALiveCrossProcessCaller(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a separate test binary twice; skipping in -short")
	}
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}

	binPath := buildTestutilTestBinary(t)
	env := append(os.Environ(), "CLEAT_TEST_MSSQL="+os.Getenv("CLEAT_TEST_MSSQL"))

	procA := startAndWaitForMarkerProcess(t, binPath, env, "^TestZZCrossProcessHoldAdminWorker$", "A")

	// Process B, while A is still live (it is mid-sleep for
	// crossProcessHoldSeconds): ordinary schema setup -- the self-heal path
	// under test.
	procB := exec.Command(binPath, "-test.run", "^TestZZCrossProcessSetupOnlyWorker$", "-test.v")
	procB.Env = env
	outB, errB := procB.CombinedOutput()
	t.Logf("process B output:\n%s", outB)
	if errB != nil {
		t.Fatalf("process B (setup-only) failed: %v", errB)
	}

	// Now wait for A: its own internal assertion (the predicate was still
	// 'admin' when it checked, after B's setup ran) is what this test is
	// actually about.
	if err := procA.Wait(); err != nil {
		t.Fatalf("process A (the live holder) failed -- cleat#2857's defect, or a regression "+
			"of the sp_getapplock fix: %v", err)
	}
}

// startAndWaitForMarkerProcess launches binPath under -test.run=runPattern,
// waits for it to print crossProcessAdminHeldMarker on its own real stdout,
// and returns the still-running *exec.Cmd for the caller to Wait() on once
// the rest of the scenario has played out. label distinguishes this
// process's log lines from any other concurrent one (e.g. "A" vs "B").
//
// Draining continues past the marker line rather than stopping there, so a
// t.Fatalf this process prints AFTER the marker (its own assertion, usually
// the whole point of the scenario) still reaches the log instead of being
// lost on the pipe cleat-review found silent (cleat#2899).
func startAndWaitForMarkerProcess(t *testing.T, binPath string, env []string, runPattern, label string) *exec.Cmd {
	t.Helper()
	proc := exec.Command(binPath, "-test.run", runPattern, "-test.v")
	proc.Env = env
	stdout, err := proc.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe (process %s): %v", label, err)
	}
	proc.Stderr = os.Stderr
	if err := proc.Start(); err != nil {
		t.Fatalf("start process %s: %v", label, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	markerSeen := make(chan error, 1)
	go func() {
		seenMarker := false
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			t.Logf("process %s: %s", label, line)
			if !seenMarker && strings.Contains(line, crossProcessAdminHeldMarker) {
				seenMarker = true
				markerSeen <- nil
			}
		}
		if !seenMarker {
			markerSeen <- fmt.Errorf("process %s's stdout closed before the marker appeared: %w", label, scanner.Err())
		}
	}()

	select {
	case err := <-markerSeen:
		if err != nil {
			proc.Process.Kill()
			t.Fatalf("waiting for process %s to acquire MSSQLAdminDB: %v", label, err)
		}
	case <-ctx.Done():
		proc.Process.Kill()
		t.Fatalf("process %s never printed the admin-held marker within 30s", label)
	}
	return proc
}

// TestZZCrossProcessReleaseWorker is G2's actor: acquires MSSQLAdminDB and
// returns immediately, letting its own t.Cleanup run the FULL release path
// for real. A fresh process always starts with mssqlAdminRefs at zero, so
// unlike two overlapping MSSQLAdminDB callers in ONE process -- see
// TestMSSQLAdminDBRefcountsOverlappingCallers, where the refcount makes the
// second caller's release a no-op decrement -- this process's single
// Cleanup call is never short-circuited by a refcount above zero. That is
// exactly what makes this process's release reach mssqlReleaseAdminDB's
// restore-or-not decision, which is what cleat#2899's G2 is about.
func TestZZCrossProcessReleaseWorker(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := MSSQLTestDB(t)
	SetupMSSQLFullSchema(t, db)
	_ = MSSQLAdminDB(t, db)
	// Returning here runs this process's t.Cleanup stack, including
	// mssqlReleaseAdminDB -- the real release path under test, not a
	// simulation of it.
}

// TestMSSQLAdminDBReleaseDoesNotClobberALiveCrossProcessCaller is cleat#2899's
// G2 regression test: releasing one process's own hold on 'admin' must not
// restore 'plain' while a DIFFERENT, still-live process's MSSQLAdminDB call
// needs it.
//
// Reuses TestZZCrossProcessHoldAdminWorker as the long-lived holder (process
// A below) exactly as the self-heal driver above does -- its own internal
// assertion, that admin.rls_predicate_form still reads 'admin' once its
// crossProcessHoldSeconds sleep ends, is what actually catches a clobber
// here, the same way it catches one from a bad heal in the sibling test.
// Process B is the new, short-lived TestZZCrossProcessReleaseWorker: it
// acquires and releases while A is still mid-sleep, and a pre-fix
// mssqlReleaseAdminDB restores 'plain' unconditionally there, which A's
// post-sleep read then catches.
func TestMSSQLAdminDBReleaseDoesNotClobberALiveCrossProcessCaller(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a separate test binary twice; skipping in -short")
	}
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}

	binPath := buildTestutilTestBinary(t)
	env := append(os.Environ(), "CLEAT_TEST_MSSQL="+os.Getenv("CLEAT_TEST_MSSQL"))

	procA := startAndWaitForMarkerProcess(t, binPath, env, "^TestZZCrossProcessHoldAdminWorker$", "A")

	// Process B, while A is still live (mid-sleep for
	// crossProcessHoldSeconds): acquire-then-release, the release path under
	// test.
	procB := exec.Command(binPath, "-test.run", "^TestZZCrossProcessReleaseWorker$", "-test.v")
	procB.Env = env
	outB, errB := procB.CombinedOutput()
	t.Logf("process B output:\n%s", outB)
	if errB != nil {
		t.Fatalf("process B (acquire-then-release) failed: %v", errB)
	}

	// Now wait for A: its own internal assertion (the predicate was still
	// 'admin' after B released) is what this test is actually about.
	if err := procA.Wait(); err != nil {
		t.Fatalf("process A (the live holder) failed -- cleat#2899's G2 defect, or a "+
			"regression of the release-side sp_getapplock probe: %v", err)
	}
}
