package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"
)

// TestSlackWorkspaceStatementsRebindPerDialect is
// TestQuotaStatementsRebindPerDialect's counterpart for this file, and the
// same check: a statement written in the portable $N form that happened to
// work on postgres because nobody rewrote its placeholders would pass every
// other test here, since TestSlackCommandWorksOnEveryDialect drives postgres
// through the same d.rebindArgs call and would not notice a no-op.
//
// This drives rebindArgs, not the bare rebind text-only helper: MySQL's
// $N -> ? rewrite now happens only inside rebindArgs, alongside the arg
// reorder, so a text-only check that called rebind here would see $1
// unchanged and wrongly report every statement as broken (cleat#2259).
//
// slackWorkspaceListSQL is excluded -- it carries no $N, deliberately (it
// lists every row, unfiltered).
func TestSlackWorkspaceStatementsRebindPerDialect(t *testing.T) {
	for _, q := range []string{
		slackWorkspaceGetSQL,
		slackWorkspaceInsertSQL,
		slackWorkspaceUpdateSQL,
		slackWorkspaceDeleteSQL,
	} {
		n := countPlaceholders(q)
		pg, _, err := dialectPostgres.rebindArgs(q, dummyArgs(n)...)
		if err != nil {
			t.Fatalf("postgres rebindArgs: %v", err)
		}
		if !strings.Contains(pg, "$1") {
			t.Errorf("dialectPostgres.rebindArgs(%q) = %q, want it to still contain $1", q, pg)
		}
		my, _, err := dialectMySQL.rebindArgs(q, dummyArgs(n)...)
		if err != nil {
			t.Fatalf("mysql rebindArgs: %v", err)
		}
		if strings.Contains(my, "$1") || !strings.Contains(my, "?") {
			t.Errorf("dialectMySQL.rebindArgs(%q) = %q, want no $1 and a ?", q, my)
		}
		ms, _, err := dialectMSSQL.rebindArgs(q, dummyArgs(n)...)
		if err != nil {
			t.Fatalf("mssql rebindArgs: %v", err)
		}
		if strings.Contains(ms, "$1") || !strings.Contains(ms, "@p1") {
			t.Errorf("dialectMSSQL.rebindArgs(%q) = %q, want no $1 and a @p1", q, ms)
		}
	}
}

// getWorkspaceTenant reads slack_workspace's tenant_id for team on a raw
// *sql.DB, the same handle shape runMapWorkspace/runUnmapWorkspace use --
// cleatctl has no plugin.PluginDB adapter, so this goes through
// d.rebindArgs directly rather than the deleted rebind text-only helper,
// exactly as production code now must (cleat#2259).
func getWorkspaceTenant(t *testing.T, ctx context.Context, db *sql.DB, d dialect, team string) (string, error) {
	t.Helper()
	stmt, args, err := d.rebindArgs(slackWorkspaceGetSQL, team)
	if err != nil {
		t.Fatalf("rebind slackWorkspaceGetSQL: %v", err)
	}
	var tenant string
	err = db.QueryRowContext(ctx, stmt, args...).Scan(&tenant)
	return tenant, err
}

// deleteWorkspaceMapping is getWorkspaceTenant's write-side counterpart,
// used only for this test's own best-effort cleanup.
func deleteWorkspaceMapping(t *testing.T, ctx context.Context, db *sql.DB, d dialect, team string) error {
	t.Helper()
	stmt, args, err := d.rebindArgs(slackWorkspaceDeleteSQL, team)
	if err != nil {
		t.Fatalf("rebind slackWorkspaceDeleteSQL: %v", err)
	}
	_, err = db.ExecContext(ctx, stmt, args...)
	return err
}

func TestValidateSlackTeamID(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		ok   bool
	}{
		{"regular workspace", "T0123ABCD", true},
		{"enterprise grid org unit", "E0123ABCD", true},
		{"minimal valid", "T1", true},
		{"empty", "", false},
		{"lowercase", "t0123abcd", false},
		{"wrong leading letter", "U0123ABCD", false},
		{"contains a hyphen", "T0123-ABCD", false},
		{"contains a space", "T0123 ABCD", false},
		// slackTeamIDShape has no length bound of its own -- validateSlackTeamID's
		// own len() check enforces 32, so a 33-character otherwise-valid id must
		// still be rejected by validateSlackTeamID even though the regex alone
		// would accept it.
		{"32 chars, at the boundary", "T" + strings.Repeat("A", 31), true},
		{"33 chars, one over", "T" + strings.Repeat("A", 32), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSlackTeamID(tc.id)
			if tc.ok && err != nil {
				t.Errorf("validateSlackTeamID(%q) = %v, want nil", tc.id, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("validateSlackTeamID(%q) = nil, want an error", tc.id)
			}
		})
	}
}

func TestSlackFlags_AcceptsAnyOrder(t *testing.T) {
	tenant := uuid.New().String()
	for _, args := range [][]string{
		{"--team", "T0123ABCD", "--tenant", tenant},
		{"--tenant", tenant, "--team", "T0123ABCD"},
		{"--team=T0123ABCD", "--tenant=" + tenant},
		{"--team", "T0123ABCD", "--tenant", tenant, "--reassign"},
		{"--reassign", "--team", "T0123ABCD", "--tenant", tenant},
	} {
		team, gotTenant, reassign, err := parseSlackFlags(args)
		if err != nil {
			t.Errorf("parseSlackFlags(%v): %v", args, err)
			continue
		}
		if team != "T0123ABCD" || gotTenant != tenant {
			t.Errorf("parseSlackFlags(%v) = team=%q tenant=%q, want T0123ABCD %q", args, team, gotTenant, tenant)
		}
		wantReassign := len(args) == 5
		if reassign != wantReassign {
			t.Errorf("parseSlackFlags(%v) reassign=%v, want %v", args, reassign, wantReassign)
		}
	}
}

func TestSlackFlags_RejectsUnknownFlag(t *testing.T) {
	_, _, _, err := parseSlackFlags([]string{"--team", "T0123ABCD", "--nonesuch", "x"})
	if err == nil || !strings.Contains(err.Error(), "nonesuch") {
		t.Fatalf("parseSlackFlags(--nonesuch) = %v, want an error naming the flag", err)
	}
}

func TestSlackFlags_RejectsMissingValue(t *testing.T) {
	if _, _, _, err := parseSlackFlags([]string{"--team"}); err == nil {
		t.Error("parseSlackFlags(--team with no value) = nil, want an error")
	}
	if _, _, _, err := parseSlackFlags([]string{"--tenant"}); err == nil {
		t.Error("parseSlackFlags(--tenant with no value) = nil, want an error")
	}
}

func TestSlackMapWorkspace_RejectsMissingFlags(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runMapWorkspace(context.Background(), nil, dialect{}, []string{"--team", "T0123ABCD"})
	})
	if !strings.Contains(stderr, "requires --team and --tenant") {
		t.Errorf("slack map-workspace with no --tenant produced:\n%s", stderr)
	}
}

func TestSlackMapWorkspace_RejectsBadTeamID(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runMapWorkspace(context.Background(), nil, dialect{}, []string{
			"--team", "not-a-team-id", "--tenant", uuid.New().String(),
		})
	})
	if !strings.Contains(stderr, "does not look like a Slack team/org id") {
		t.Errorf("slack map-workspace with a malformed --team produced:\n%s", stderr)
	}
}

func TestSlackMapWorkspace_RejectsNonUUIDTenant(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runMapWorkspace(context.Background(), nil, dialect{}, []string{
			"--team", "T0123ABCD", "--tenant", "not-a-uuid",
		})
	})
	if !strings.Contains(stderr, "not a tenant UUID") {
		t.Errorf("slack map-workspace with a malformed --tenant produced:\n%s", stderr)
	}
}

func TestSlackUnmapWorkspace_RejectsMissingTeam(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runUnmapWorkspace(context.Background(), nil, dialect{}, nil)
	})
	if !strings.Contains(stderr, "requires --team") {
		t.Errorf("slack unmap-workspace with no --team produced:\n%s", stderr)
	}
}

// TestSlackCommandWorksOnEveryDialect is TestQuotaCommandWorksOnEveryDialect's
// counterpart: map, list, reassign (with and without --reassign), unmap, and
// tenant-scoping (a cross-tenant read only matters for RLS-backed tables;
// this one has none, by migrations/postgres/104's own design, so what is
// checked instead is that two DIFFERENT team_ids for the same tenant are
// both listed rather than colliding).
func TestSlackCommandWorksOnEveryDialect(t *testing.T) {
	for _, tc := range []struct {
		name string
		td   testutil.Dialect
		d    dialect
	}{
		{"postgres", testutil.DialectPostgres, dialectPostgres},
		{"mysql", testutil.DialectMySQL, dialectMySQL},
		{"mssql", testutil.DialectMSSQL, dialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.td)
			ctx := context.Background()

			// No plugin.RunMigrations here: slack_workspace is a CORE
			// migration (migrations/{postgres,mysql,mssql}/10{3,4}...), and
			// testutil.TestDB already applies every core migration via
			// SetupMinimalSchema/applyMigrations. slacknotify's own
			// plugin.RunMigrations was here in an earlier version of this
			// test and did nothing for this table -- it only pulled in
			// slacknotify's OWN migrations (slack_config, plus an MSSQL
			// policy), which this test does not touch and which then leaked
			// into the shared test database for every other test in this
			// package to trip over, cleat#2239's class of defect.

			// t.Cleanup, not reliance on the test's own trailing unmap calls:
			// MySQL's test database is the SHARED, PERSISTENT one behind the
			// singleton tenant seeded above (seedSlackTestTenant's own
			// comment), not a fresh schema per run -- so a row this test
			// leaves behind (notably team2, which nothing below unmaps) is
			// still there on the NEXT run and inflates
			// "tenant maps to N workspaces" counts across invocations.
			// Registered before either team exists so a t.Fatalf partway
			// through still deletes whichever rows made it in.
			cleanupTeam := func(team string) {
				t.Cleanup(func() {
					deleteWorkspaceMapping(t, context.Background(), db, tc.d, team) //nolint:errcheck // best-effort cleanup
				})
			}

			team := "T" + strings.ToUpper(uuid.New().String()[:8])
			cleanupTeam(team)
			tenant := seedSlackTestTenant(t, ctx, db, tc.td)

			// A team with no row: runMapWorkspace's SELECT must read
			// sql.ErrNoRows, not silently succeed some other way.
			existing, err := getWorkspaceTenant(t, ctx, db, tc.d, team)
			if err == nil {
				t.Fatalf("a fresh team already maps to %q", existing)
			}

			runMapWorkspace(ctx, db, tc.d, []string{"--team", team, "--tenant", tenant})

			var gotTenant string
			if gotTenant, err = getWorkspaceTenant(t, ctx, db, tc.d, team); err != nil {
				t.Fatalf("reading after map: %v", err)
			}
			if !strings.EqualFold(gotTenant, tenant) {
				t.Fatalf("after map, team_id=%s maps to %s, want %s", team, gotTenant, tenant)
			}

			// Re-mapping to the SAME tenant is a no-op, not an error -- and
			// must not require --reassign.
			runMapWorkspace(ctx, db, tc.d, []string{"--team", team, "--tenant", tenant})

			// Re-mapping to a DIFFERENT tenant, with and without --reassign,
			// needs a SECOND real tenant row -- and MySQL's `tenants` table
			// carries a hard singleton constraint (038_single_tenant_guard.sql's
			// `uq_tenants_mysql_is_single_tenant_only_see_tiers_yaml_d1`), not
			// just the convention reseal_secrets_test.go's own seed helper
			// notes ("MySQL is single-tenant by constraint"). A second INSERT
			// there fails on that unique index, before this test's subject is
			// even reached. So this section only runs where a second tenant
			// can exist; on MySQL it is skipped outright rather than faked
			// with a same-tenant reassign, which would silently stop testing
			// the refusal and the actual move.
			other := tenant
			if tc.td != testutil.DialectMySQL {
				other = seedSlackTestTenant(t, ctx, db, tc.td)

				stderr := withExitPanic(t, func() {
					runMapWorkspace(ctx, db, tc.d, []string{"--team", team, "--tenant", other})
				})
				if !strings.Contains(stderr, "pass --reassign") {
					t.Errorf("re-mapping without --reassign produced:\n%s", stderr)
				}
				if gotTenant, err = getWorkspaceTenant(t, ctx, db, tc.d, team); err != nil {
					t.Fatalf("reading after refused reassign: %v", err)
				}
				if !strings.EqualFold(gotTenant, tenant) {
					t.Fatalf("a refused reassign still changed the mapping to %s, want unchanged %s", gotTenant, tenant)
				}

				// With --reassign, it must actually move.
				runMapWorkspace(ctx, db, tc.d, []string{"--team", team, "--tenant", other, "--reassign"})
				if gotTenant, err = getWorkspaceTenant(t, ctx, db, tc.d, team); err != nil {
					t.Fatalf("reading after reassign: %v", err)
				}
				if !strings.EqualFold(gotTenant, other) {
					t.Fatalf("after --reassign, team_id=%s maps to %s, want %s", team, gotTenant, other)
				}
			}

			// A second, distinct team_id for the same tenant must coexist --
			// the table is keyed on team_id, not (team_id, tenant_id).
			//
			// Checked by looking up team2 itself, not by counting every row
			// that maps to `other`: on MySQL, `other` IS the shared, process-
			// wide default tenant seedSlackTestTenant returns (the singleton
			// constraint leaves no other choice), so a global count there
			// reflects every OTHER test and session that has ever mapped a
			// workspace to it, not just this run's two.
			team2 := "T" + strings.ToUpper(uuid.New().String()[:8])
			cleanupTeam(team2)
			runMapWorkspace(ctx, db, tc.d, []string{"--team", team2, "--tenant", other})
			var team2Tenant string
			if team2Tenant, err = getWorkspaceTenant(t, ctx, db, tc.d, team2); err != nil {
				t.Fatalf("reading %s after mapping it alongside %s: %v", team2, team, err)
			}
			if !strings.EqualFold(team2Tenant, other) {
				t.Fatalf("%s maps to %s, want %s", team2, team2Tenant, other)
			}
			if gotTenant, err = getWorkspaceTenant(t, ctx, db, tc.d, team); err != nil {
				t.Fatalf("reading %s after mapping %s alongside it: %v", team, team2, err)
			}
			if !strings.EqualFold(gotTenant, other) {
				t.Fatalf("mapping %s disturbed %s's mapping: now %s, want %s", team2, team, gotTenant, other)
			}

			// list-workspaces round-trips tenant_id through the same CAST
			// slackWorkspaceGetSQL uses. Nothing above exercises this
			// separately -- slackWorkspaceListSQL is its own statement,
			// and dropping ITS CAST stays green everywhere but MSSQL,
			// where UNIQUEIDENTIFIER then scans as 16 raw storage bytes
			// rather than the canonical hyphenated string (cleat-review,
			// cleat#2230). uuid.Parse is the assertion that actually
			// catches that: raw bytes reinterpreted as text fail to parse
			// as a UUID at all, where a merely-wrong-case string would
			// still parse and only strings.EqualFold would catch it.
			listed := parseListWorkspacesOutput(t, captureStdout(t, func() {
				runListWorkspaces(ctx, db, tc.d, nil)
			}))
			for _, want := range []struct{ team, tenant string }{
				{team, other}, {team2, other},
			} {
				got, ok := listed[want.team]
				if !ok {
					t.Fatalf("list-workspaces did not list %s (rows: %v)", want.team, listed)
				}
				if _, err := uuid.Parse(got); err != nil {
					t.Fatalf("list-workspaces printed %q for %s's tenant_id, which is not even a UUID: %v "+
						"(the CAST(tenant_id AS CHAR(36)) in slackWorkspaceListSQL is likely missing)",
						got, want.team, err)
				}
				if !strings.EqualFold(got, want.tenant) {
					t.Fatalf("list-workspaces says %s -> %s, want %s", want.team, got, want.tenant)
				}
			}

			// unmap removes exactly the row named, and only it.
			runUnmapWorkspace(ctx, db, tc.d, []string{"--team", team})
			if gotTenant, err = getWorkspaceTenant(t, ctx, db, tc.d, team); err == nil {
				t.Fatalf("team %s still maps to %s after unmap", team, gotTenant)
			}
			if gotTenant, err = getWorkspaceTenant(t, ctx, db, tc.d, team2); err != nil || !strings.EqualFold(gotTenant, other) {
				t.Fatalf("unmapping %s disturbed %s's mapping: gotTenant=%q err=%v", team, team2, gotTenant, err)
			}

			// Unmapping an already-unmapped team is a no-op, not an error.
			runUnmapWorkspace(ctx, db, tc.d, []string{"--team", team})
		})
	}
}

// parseListWorkspacesOutput maps team_id -> tenant_id from runListWorkspaces'
// fixed-width stdout, skipping its header row. strings.Fields is safe against
// the %-16s/%-36s padding because it splits on any run of whitespace, not a
// fixed column width.
func parseListWorkspacesOutput(t *testing.T, out string) map[string]string {
	t.Helper()
	rows := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "TEAM_ID" {
			continue
		}
		rows[fields[0]] = fields[1]
	}
	return rows
}

// seedSlackTestTenant inserts one row into the tenants table
// slack_workspace's FK targets on this dialect (admin.tenants on postgres
// and mssql, unqualified tenants on mysql -- migrations/mysql/103's own
// header, "unqualified tenants(tenant_id) FK: the same shape
// tenant_domains' own mysql migration (068) uses") and registers its
// cleanup, then returns the new tenant's id.
//
// Unlike tenant_quota (TestQuotaCommandWorksOnEveryDialect's own comment:
// "carries no FK to tenants... an invented UUID is fine"), slack_workspace
// has one, by this table's own design (the header comment on
// migrations/postgres/104: the FK and CASCADE need a real tenant_id column,
// which is why this table diverges from deployment_secrets' no-tenant-
// column precedent). So an invented UUID here is NOT fine, and using one
// (the first version of this test did) fails on the INSERT with a foreign
// key violation rather than anywhere close to the behavior under test.
//
// reseal_secrets_test.go's own seed helper skips this on MySQL ("MySQL is
// single-tenant by constraint") because ITS subject is whether a
// suspended tenant is skipped -- a product-level multi-tenancy question
// that genuinely has no MySQL story. Nothing here asks that question:
// slack_workspace's FK is enforced identically on all three dialects, and
// migrations/mysql/001_schema.sql's tenants table carries no constraint
// limiting it to one row, only a UNIQUE on name. So this seeds two rows on
// MySQL too, exercising the FK and the reassign-to-a-different-tenant path
// the same way on every dialect.
func seedSlackTestTenant(t *testing.T, ctx context.Context, db *sql.DB, td testutil.Dialect) string {
	t.Helper()

	// MySQL's `tenants` table cannot take a second INSERT at all --
	// 038_single_tenant_guard.sql's uq_tenants_mysql_is_single_tenant_only_...
	// unique index enforces at most one row, full stop, and this test's
	// database already has one (the process-wide default tenant, present
	// before this test ever runs). So on this dialect ONLY, reuse whatever
	// is there rather than inserting -- and never delete it in cleanup: it
	// is not this test's row to own, and other tests in this shared
	// container depend on it existing.
	if td == testutil.DialectMySQL {
		var existing string
		err := db.QueryRowContext(ctx, `SELECT tenant_id FROM tenants LIMIT 1`).Scan(&existing)
		if err == nil {
			return existing
		}
		if err != sql.ErrNoRows {
			t.Fatalf("reading the existing mysql tenant: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO tenants (tenant_id, name) VALUES (?, ?)`,
			engine.DefaultTenantUUID, "default"); err != nil {
			t.Fatalf("seeding the default mysql tenant: %v", err)
		}
		return engine.DefaultTenantUUID
	}

	id := uuid.New().String()
	name := "slack-workspace-test-" + id[:8]

	// Only postgres and mssql reach here -- MySQL returned above.
	ins := map[testutil.Dialect]string{
		testutil.DialectPostgres: `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
		testutil.DialectMSSQL:    `INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`,
	}[td]
	if _, err := db.ExecContext(ctx, ins, id, name); err != nil {
		t.Fatalf("seeding a tenant on %v: %v", td, err)
	}

	del := map[testutil.Dialect]string{
		testutil.DialectPostgres: `DELETE FROM admin.tenants WHERE tenant_id = $1`,
		testutil.DialectMSSQL:    `DELETE FROM admin.tenants WHERE tenant_id = @p1`,
	}[td]
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), del, id); err != nil {
			t.Logf("cleanup: deleting seeded tenant %s: %v", id, err)
		}
	})
	return id
}
