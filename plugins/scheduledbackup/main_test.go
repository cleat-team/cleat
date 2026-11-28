package scheduledbackup

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain puts a pg_dump that fails at once first on PATH for the whole test
// binary (cleat#3622).
//
// A dispatched backup runs "pg_dump" from PATH against the host in
// testBackupDSN ("postgres://test"). Left alone, how long the tests that wait
// for a dispatched backup take is how long THE MACHINE'S pg_dump takes to fail
// -- nothing, on a machine with none, and whatever name resolution and a
// connect attempt cost on one that has it. With a slow pg_dump on PATH, four
// tests that run backups failed with "timed out waiting for dispatched backup
// to finish", the message the CI stall in cleat#3622 produced once.
//
// FAILS rather than succeeds, because TestSB_ExecuteScheduledBackup_Error asserts
// status 'failed' and says "no real pg_dump on PATH", which is the condition
// this recreates on every machine. A test that needs a dump to succeed, or to
// run long enough to cancel, still installs its own with installFakePgDump,
// which goes in front of this one.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "scheduledbackup-fake-pg_dump-")
	if err != nil {
		panic(err)
	}
	script := "#!/bin/sh\necho 'pg_dump: fake pg_dump from TestMain (cleat#3622)' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "pg_dump"), []byte(script), 0o755); err != nil {
		panic(err)
	}
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
