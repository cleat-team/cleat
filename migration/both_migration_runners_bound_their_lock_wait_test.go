package migration_test

// Both migration runners bound how long they wait for a lock, and by the same
// amount. cleat#1775.
//
// # Why this is a source-level check and not a behavioural one
//
// There are two runners: migration.Runner.session for the core schema, and
// plugin.pluginMigrationSession for plugin schemas. Both pin a connection, both
// take an advisory lock, both must bound the wait, and both must put the
// setting back before returning the connection to the pool.
//
// The plugin side cannot import migration.DefaultLockTimeout. `go list -deps`
// shows neither package importing the other and `go build ./...` accepts the
// import, but `go vet` rejects it -- migration's own internal_test.go imports
// engine, which imports plugin, so the cycle exists in the TEST build only.
// (That cost two wrong commits' worth of reasoning before vet said so.) The
// duration is therefore written twice, and two literals drift.
//
// A behavioural test would need a database, which means it skips where none is
// configured, and this repo has paid four times for a skip being read as a
// pass. This is a property of the source, so it runs in every job.
//
// Same move, and the same neighbours, as session_scoped_lock_test.go: that one
// asserts a property across these two exact files, for the same reason.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// lockBoundSites are the two files that pin a migration connection.
var lockBoundSites = []string{
	"migration/runner.go",
	"plugin/migration.go",
}

var (
	setLockTimeout   = regexp.MustCompile(`SET lock_timeout = (?:%d|(\d+))`)
	resetLockTimeout = regexp.MustCompile(`RESET lock_timeout`)
	defaultConst     = regexp.MustCompile(`DefaultLockTimeout = (\d+) \* time\.Second`)
)

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("UNMEASURED: locating the repository root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestBothMigrationRunnersBoundTheirLockWait(t *testing.T) {
	root := repoRoot(t)

	for _, rel := range lockBoundSites {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("UNMEASURED: reading %s: %v. This is a failure of the check, not a finding.", rel, err)
		}
		body := string(src)

		if !setLockTimeout.MatchString(body) {
			t.Errorf("%s pins a migration connection but never sets lock_timeout. "+
				"A migration that cannot get its lock waits forever, and a worker waiting in "+
				"migrations is not heartbeating -- the reaper then collects its runs mid-DDL. "+
				"cleat#1775.", rel)
		}
		if !resetLockTimeout.MatchString(body) {
			t.Errorf("%s sets lock_timeout but never resets it. *sql.Conn.Close() RETURNS the "+
				"connection to the pool, so the bound would ride back in and apply to ordinary "+
				"application traffic on whichever caller is handed it next.", rel)
		}
	}

	// And the two durations agree. The plugin side carries a literal because it
	// cannot import the constant; nothing but this makes them the same number.
	runnerSrc, err := os.ReadFile(filepath.Join(root, "migration/runner.go"))
	if err != nil {
		t.Fatalf("UNMEASURED: reading migration/runner.go: %v", err)
	}
	m := defaultConst.FindStringSubmatch(string(runnerSrc))
	if m == nil {
		t.Fatalf("UNMEASURED: could not read DefaultLockTimeout out of migration/runner.go. " +
			"The pattern stopped matching, so the comparison below would be vacuous.")
	}
	coreSeconds, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("UNMEASURED: DefaultLockTimeout is %q, which is not a number", m[1])
	}

	pluginSrc, err := os.ReadFile(filepath.Join(root, "plugin/migration.go"))
	if err != nil {
		t.Fatalf("UNMEASURED: reading plugin/migration.go: %v", err)
	}
	pm := setLockTimeout.FindStringSubmatch(string(pluginSrc))
	if pm == nil || pm[1] == "" {
		t.Fatalf("UNMEASURED: could not read the literal millisecond value out of " +
			"plugin/migration.go's SET lock_timeout.")
	}
	pluginMillis, err := strconv.Atoi(pm[1])
	if err != nil {
		t.Fatalf("UNMEASURED: plugin lock_timeout is %q, which is not a number", pm[1])
	}

	if pluginMillis != coreSeconds*1000 {
		t.Errorf("the two migration runners bound their lock wait differently: "+
			"migration.DefaultLockTimeout is %ds (%dms), plugin/migration.go sets %dms. "+
			"They are two literals because the import would cycle in migration's test build; "+
			"keeping them equal is this test's whole job.",
			coreSeconds, coreSeconds*1000, pluginMillis)
	}
}
