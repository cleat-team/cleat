package main

// cleat#2306, phase 1. --uninstall-plugin refuses up front on MySQL and SQL
// Server for any plugin not proven by an end-to-end Down test
// (plugin.UninstallProvenOnDialect, plugin/uninstall_proven.go). This drives
// the real binary against a real, empty database of each dialect, because
// the property under test is what the PROCESS does before it ever reaches
// plugin.RunDownMigrations -- a unit test of the gate function alone (already
// in plugin/uninstall_proven_test.go) cannot see whether main.go actually
// wired it in, or wired it in AFTER the dry-run report already printed.
//
// deployDialect, deployScratch and runWorker are shared with
// a_migration_is_a_deploy_step_test.go, same package.

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallPluginRefusesAnUnprovenPluginOnEveryDialect(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "cleat-worker")
	build := exec.Command("go", "build", "-o", bin, "./cmd/cleat-worker")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/cleat-worker: %v\n%s", err, out)
	}

	for _, c := range []deployDialect{
		{"postgres", "CLEAT_TEST_POSTGRES", "postgres"},
		{"mysql", "CLEAT_TEST_MYSQL", "mysql"},
		{"mssql", "CLEAT_TEST_MSSQL", "sqlserver"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.admin() == "" {
				t.Skipf("%s not set, skipping %s", c.env, c.name)
			}
			dsn, _ := deployScratch(t, c)
			base := []string{"--driver=" + c.name, "--db=" + dsn}
			gated := c.name != "postgres"

			// 1. blobstore has no end-to-end uninstall test (cleat-review's
			// 2026-09-25 measurement lists it among the plugins whose Down
			// breaks on both MySQL and SQL Server). It must refuse on those
			// two dialects and must NOT refuse on PostgreSQL, which this
			// gate does not touch.
			code, out := runWorker(t, bin, nil, append(base, "--uninstall-plugin=blobstore")...)
			if gated {
				if code == 0 {
					t.Fatalf("--uninstall-plugin=blobstore exited 0 on %s; an unproven plugin "+
						"must refuse.\n%s", c.name, out)
				}
				for _, want := range []string{"uninstall is supported on PostgreSQL", "cleat#2306", "scheduled-backup"} {
					if !strings.Contains(out, want) {
						t.Errorf("%s: refusal missing %q in:\n%s", c.name, want, out)
					}
				}
			} else {
				if code != 0 {
					t.Fatalf("--uninstall-plugin=blobstore refused on PostgreSQL, which this "+
						"gate does not cover:\n%s", out)
				}
				if strings.Contains(out, "uninstall is supported on PostgreSQL") {
					t.Errorf("PostgreSQL printed the dialect-gate refusal message:\n%s", out)
				}
			}

			// 2. The gate runs BEFORE --uninstall-dry-run's report, not after:
			// a dry run for an unproven plugin must refuse too, and must not
			// print "DRY RUN" first -- that would read as reassurance about a
			// dialect the plugin was never tested on.
			code, out = runWorker(t, bin, nil, append(base, "--uninstall-plugin=blobstore", "--uninstall-dry-run")...)
			if gated {
				if code == 0 {
					t.Fatalf("--uninstall-dry-run for an unproven plugin exited 0 on %s.\n%s", c.name, out)
				}
				if strings.Contains(out, "DRY RUN") {
					t.Errorf("%s: the dry-run report printed despite the plugin not being proven "+
						"on this dialect:\n%s", c.name, out)
				}
			} else if code != 0 || !strings.Contains(out, "DRY RUN") {
				t.Fatalf("--uninstall-dry-run for blobstore on PostgreSQL should report cleanly:\n%s", out)
			}

			// 3. scheduled-backup IS proven on every dialect
			// (plugins/scheduledbackup/a_v4_down_keeps_uninstall_working_test.go)
			// and must never be gated here, on any of the three. Dry run only:
			// applying its migrations first is out of scope for this test,
			// which is about the gate, not about scheduled-backup's Down SQL.
			code, out = runWorker(t, bin, nil, append(base, "--uninstall-plugin=scheduled-backup", "--uninstall-dry-run")...)
			if code != 0 {
				t.Fatalf("scheduled-backup's dry run was refused on %s, but it is the one plugin "+
					"this gate should always allow:\n%s", c.name, out)
			}
			if !strings.Contains(out, "DRY RUN") {
				t.Errorf("%s: scheduled-backup's dry run did not print its report:\n%s", c.name, out)
			}
		})
	}
}
