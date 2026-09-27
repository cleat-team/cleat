package testutil

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestASiblingPackageBinaryIsNotForeign pins the exemption that makes
// `go test ./plugins/...` survive cleat#1469's gate.
//
// `go test` runs each package's test binary as a separate OS process, up to -p
// of them at once. Every sibling looks like a foreign client, so whichever
// starts second was refused and the invocation became flaky -- 2 failures in 3
// runs on a clean develop. Siblings share a parent (the `go` driver) and have
// distinct pids; a second `go test` gets its own driver and so a different
// parent, which is why the case the gate exists for still fires.
//
// BOTH DIRECTIONS ARE ASSERTED. An exemption that swallowed everything would
// pass the first half on its own, and would silently disable the gate.
func TestASiblingPackageBinaryIsNotForeign(t *testing.T) {
	if db := TestDB(t, DialectPostgres); db == nil {
		t.Fatal("no PostgreSQL test database")
	}

	dsn := os.Getenv("CLEAT_TEST_POSTGRES")
	if dsn == "" {
		dsn = os.Getenv("CLEAT_TEST_DB")
	}

	// A connection tagged as a DIFFERENT pid that shares our parent, which is
	// exactly what a sibling package binary looks like. Staged by tagging a
	// real connection, because the probe reads the tag off the server rather
	// than trusting anything in this process.
	sibling := openTagged(t, dsn, "cleat-test-999001")
	defer sibling.Close()

	// A REAL live process whose parent is this one. With selfPPID claiming our
	// parent IS this process, that child is a sibling by exactly the relation
	// `go test` creates between package binaries -- same parent, different pid
	// -- and the ps lookup inside isSiblingTestProcess runs for real.
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatalf("staging a sibling process: %v", err)
	}
	defer func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() }()
	childPID := child.Process.Pid

	realPPID := selfPPID
	selfPPID = func() int { return os.Getpid() }
	defer func() { selfPPID = realPPID }()

	if !isSiblingTestProcess(childPID) {
		t.Fatalf("a live process whose parent is ours (pid %d) was not recognised as a "+
			"sibling. `go test ./plugins/...` runs each package as a separate process "+
			"under one driver; without this they refuse each other. cleat#1469.", childPID)
	}

	sib := openTagged(t, dsn, fmt.Sprintf("cleat-test-%d", childPID))
	defer sib.Close()

	foreign, basis, ok := ForeignSessions(DialectPostgres)
	if !ok {
		t.Fatalf("the probe could not answer at all: %s", basis)
	}
	for _, f := range foreign {
		if strings.Contains(f, fmt.Sprintf("cleat-test-%d", childPID)) {
			t.Errorf("a sibling package binary was reported as foreign:\n  %s\n\n"+
				"reported set: %v", f, foreign)
		}
	}

	// THE OTHER DIRECTION, on the same run: the staged stranger must STILL be
	// reported. An exemption that swallowed everything would pass the check
	// above and silently disable the gate.
	sawStaged := false
	for _, f := range foreign {
		if strings.Contains(f, "cleat-test-999001") {
			sawStaged = true
		}
	}
	if !sawStaged {
		t.Errorf("a connection tagged with an unresolvable pid was NOT reported.\n\n"+
			"isSiblingTestProcess must fail CLOSED: a pid it cannot resolve is the "+
			"session we know least about. reported: %v", foreign)
	}

	if isSiblingTestProcess(1) {
		t.Error("pid 1 was treated as a sibling; init is every orphan's parent, so " +
			"matching on it would exempt any process whose real parent has died")
	}
	if isSiblingTestProcess(0) || isSiblingTestProcess(-1) {
		t.Error("a non-positive pid was treated as a sibling")
	}
}

func openTagged(t *testing.T, dsn, tag string) *sql.DB {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing the test DSN: %v", err)
	}
	q := u.Query()
	q.Set("application_name", tag)
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatalf("opening a tagged connection: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("pinging the tagged connection: %v", err)
	}
	db.SetMaxIdleConns(1)
	return db
}
