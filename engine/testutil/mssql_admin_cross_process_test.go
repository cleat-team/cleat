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

	root := repoRootForMSSQLTestutil(t)
	binPath := t.TempDir() + "/testutil.test"
	build := exec.Command("go", "test", "-c", "-o", binPath, "./engine/testutil")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go test -c: %v\n%s", err, out)
	}

	env := append(os.Environ(), "CLEAT_TEST_MSSQL="+os.Getenv("CLEAT_TEST_MSSQL"))

	// Process A: the holder. Its stdout is read live, not CombinedOutput'd
	// after the fact, so this test can tell the moment it has acquired
	// MSSQLAdminDB rather than guessing from a sleep -- the same "observe a
	// marker, don't race a timer" discipline this repo's acceptance tests
	// use for a real crash rather than a synthetic one.
	procA := exec.Command(binPath, "-test.run", "^TestZZCrossProcessHoldAdminWorker$", "-test.v")
	procA.Env = env
	stdoutA, err := procA.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	procA.Stderr = os.Stderr
	if err := procA.Start(); err != nil {
		t.Fatalf("start process A: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	markerSeen := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdoutA)
		for scanner.Scan() {
			line := scanner.Text()
			t.Logf("process A: %s", line)
			if strings.Contains(line, crossProcessAdminHeldMarker) {
				markerSeen <- nil
				return
			}
		}
		markerSeen <- fmt.Errorf("process A's stdout closed before the marker appeared: %w", scanner.Err())
	}()

	select {
	case err := <-markerSeen:
		if err != nil {
			procA.Process.Kill()
			t.Fatalf("waiting for process A to acquire MSSQLAdminDB: %v", err)
		}
	case <-ctx.Done():
		procA.Process.Kill()
		t.Fatal("process A never printed the admin-held marker within 30s")
	}

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
