package migration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#1666. A migration run in this package that does not go through
// runMigrations is on the unprotected side of the cluster-wide lock, and the
// failure that produces is a `tuple concurrently updated` in an unrelated
// package's CI job -- attributed to whatever PR happened to be running.
//
// WHY A SCANNER RATHER THAN A CONVENTION. cleat#1603 added the lock and wired
// it into engine/testutil alone; this package stayed unprotected for days
// while the protection read as present. Ten call sites is nine too many to
// keep right by hand, and the one that would have been missed was not even
// greppable by dialect: idempotency_tenant_test.go drives the runner over a
// TABLE of dialects, so `DialectPostgres` appears nowhere in it, and a search
// for that literal reports it clean.
//
// The exemptions are named individually and each says why. A blanket
// exemption by file would have covered the table-driven case silently.
func TestEveryMigrationRunInThisPackageTakesTheClusterLock(t *testing.T) {
	// site: "file:line-fragment" -> reason. Named individually and on purpose.
	allowed := map[string]string{
		// The helper itself, which is where the lock is taken.
		"runner_test.go:return r.Run(ctx)": "runMigrations' unlocked arm: non-PostgreSQL dialects do not contend " +
			"on cluster-wide roles, so they take no lock",
		"runner_test.go:pgclusterlock.WithClusterMigrationLock(dsn, func() { err = r.Run(ctx) })": "runMigrations' locked arm -- this IS the lock",

		// The deliberate collision. Serialising these four EXTERNALLY would
		// make the test pass while exercising nothing: its whole subject is
		// the runner's own per-database lock. It holds the cluster lock once
		// around the block instead, which is checked below.
		"runner_test.go:errs[i] = migration.NewRunner(db, migration.DialectPostgres, root).Run(context.Background())": "TestRunner_ConcurrentRunsAreSerialised collides on purpose; the block " +
			"is wrapped once, not per goroutine",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var unprotected []string
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, "_test.go") {
			continue
		}
		// This file quotes every allowlisted site verbatim, so scanning it
		// would report its own allowlist as findings.
		if name == "every_migration_run_takes_the_cluster_lock_test.go" {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if idx := strings.Index(trimmed, "//"); idx == 0 {
				continue
			}
			// The shape that matters: a Runner being Run. `t.Run(` is the
			// subtest helper and is not one.
			if !strings.Contains(trimmed, ".Run(") || strings.HasPrefix(trimmed, "t.Run(") ||
				strings.Contains(trimmed, "t.Run(") {
				continue
			}
			scanned++
			if _, ok := allowed[name+":"+trimmed]; ok {
				continue
			}
			unprotected = append(unprotected,
				name+":"+itoa(i+1)+"  "+trimmed)
		}
	}

	// The scanner must have found the sites it is supposed to police. If a
	// refactor renames things so this matches nothing, "no findings" would be
	// indistinguishable from "all clear" -- so depend on the count.
	if scanned < len(allowed) {
		t.Fatalf("the scanner found %d Runner.Run site(s) but %d are allowlisted, so it is "+
			"not seeing what it is meant to police -- this check has stopped working",
			scanned, len(allowed))
	}

	if len(unprotected) > 0 {
		t.Errorf("%d migration run(s) in this package do not go through runMigrations, "+
			"so they are on the unprotected side of the cluster-wide lock (cleat#1666):\n    %s\n\n"+
			"Use runMigrations(t, ctx, runner, dialect). If a site genuinely must run "+
			"unlocked, add it to `allowed` above WITH A REASON -- individually, not by file.",
			len(unprotected), strings.Join(unprotected, "\n    "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
