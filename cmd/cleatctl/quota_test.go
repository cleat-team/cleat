package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/tenantquota"
	"github.com/google/uuid"
)

// TestQuotaStatementsRebindPerDialect asserts quota.go's statements are
// written in the portable $N form and that d.rebind actually reaches all
// three placeholder forms -- the same check
// TestRebindRewritesEveryPlaceholderInThisPackage makes for the rest of the
// package, applied to this file's own statements rather than assumed of
// them. A statement that happened to work on postgres because nobody
// rewrote its placeholders would pass every other test in this file, since
// TestQuotaCommandWorksOnEveryDialect drives postgres through the same
// d.rebind call and would not notice a no-op.
func TestQuotaStatementsRebindPerDialect(t *testing.T) {
	for _, q := range []string{quotaReadSQL, quotaInsertSQL, quotaUpdateSQL, quotaListTenantSQL, quotaListAllSQL} {
		// quotaListAllSQL takes no parameters at all -- it lists every tenant --
		// so it is the one statement here with no $N to preserve or rewrite.
		if !strings.Contains(q, "$1") {
			continue
		}
		pg := dialectPostgres.rebind(q)
		if !strings.Contains(pg, "$1") {
			t.Errorf("postgres arm lost its own $N form: %q", pg)
		}
		my := dialectMySQL.rebind(q)
		if strings.Contains(my, "$1") {
			t.Errorf("mysql arm still carries $1: %q", my)
		}
		if strings.Contains(q, "$1") && !strings.Contains(my, "?") {
			t.Errorf("mysql arm has no positional ? placeholder: %q", my)
		}
		ms := dialectMSSQL.rebind(q)
		if strings.Contains(ms, "$1") {
			t.Errorf("mssql arm still carries $1: %q", ms)
		}
		if strings.Contains(q, "$1") && !strings.Contains(ms, "@p1") {
			t.Errorf("mssql arm has no @pN placeholder: %q", ms)
		}
	}

	// created_at/updated_at must be BOUND parameters, never SQL-side now().
	// MySQL's bare now() truncates to whole-second precision against a
	// TIMESTAMP(6) column -- TestQuotaCommandWorksOnEveryDialect/mysql caught
	// this directly, as two writes inside one wall-clock second producing an
	// identical stored timestamp defeated the stale-write refusal entirely.
	// Regression guard: this statement must carry no now()/NOW(6)/
	// SYSUTCDATETIME() call, on any dialect, ever again.
	for _, d := range []dialect{dialectPostgres, dialectMySQL, dialectMSSQL} {
		for _, q := range []string{quotaInsertSQL, quotaUpdateSQL} {
			got := strings.ToLower(d.rebind(q))
			if strings.Contains(got, "now(") || strings.Contains(got, "sysutcdatetime") {
				t.Errorf("%s: statement still calls a SQL-side now expression: %q", d.name, got)
			}
		}
	}
}

// TestQuotaCommandWorksOnEveryDialect is the acceptance test cleat#2046 asks
// for: a quota set via cleatctl takes effect (is readable back, and refuses
// a stale write) on all three dialects.
func TestQuotaCommandWorksOnEveryDialect(t *testing.T) {
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

			loaded := []*plugin.LoadedPlugin{{Plugin: tenantquota.New(), Healthy: true}}
			if err := plugin.RunMigrations(ctx, db, tc.d.query, nil, loaded); err != nil {
				t.Fatalf("apply tenantquota migrations on %s: %v", tc.name, err)
			}

			// tenant_quota carries no FK to tenants (unlike tenant_egress_allow),
			// so an invented UUID is fine and is the point: it proves the table
			// needs no tenants row to be written or read.
			tenant := uuid.New().String()
			const resource = "workflow_starts"

			// quotaConnFor, not db directly: on SQL Server tenant_quota is bound
			// to a SECURITY POLICY keyed on SESSION_CONTEXT('tenant_id'), same as
			// tenant_egress_allow and tenant_secrets (droptenant_mssql.go,
			// setsecret.go), and no role bypasses it. Postgres and MySQL don't
			// need this -- quotaConnFor returns db itself for them -- so this is
			// the one call that makes the test genuinely portable across all
			// three rather than passing on two by construction.
			exec, closeExec, err := quotaConnFor(ctx, db, tc.d, tenant)
			if err != nil {
				t.Fatalf("quotaConnFor: %v", err)
			}
			defer closeExec()

			// A tenant with no row reads as unmetered, not an error.
			q, err := readQuota(ctx, exec, tc.d, tenant, resource)
			if err != nil {
				t.Fatalf("reading an unconfigured tenant: %v", err)
			}
			if q.existed {
				t.Fatalf("a fresh tenant already has a quota row: %+v", q)
			}

			// First write: create.
			first := quotaRow{limitCount: 100, windowSeconds: 3600, enforce: false}
			if err := writeQuota(ctx, exec, tc.d, tenant, resource, q, first); err != nil {
				t.Fatalf("creating a quota: %v", err)
			}

			got, err := readQuota(ctx, exec, tc.d, tenant, resource)
			if err != nil {
				t.Fatalf("reading after create: %v", err)
			}
			if !got.existed || got.limitCount != 100 || got.windowSeconds != 3600 || got.enforce {
				t.Fatalf("readQuota after create = %+v, want limit=100 window=3600 enforce=false", got)
			}

			// Second write: update, using the revision just read -- must succeed.
			second := got
			second.limitCount = 200
			second.enforce = true
			if err := writeQuota(ctx, exec, tc.d, tenant, resource, got, second); err != nil {
				t.Fatalf("updating a quota with a fresh revision: %v", err)
			}
			got2, err := readQuota(ctx, exec, tc.d, tenant, resource)
			if err != nil {
				t.Fatalf("reading after update: %v", err)
			}
			if got2.limitCount != 200 || !got2.enforce {
				t.Fatalf("readQuota after update = %+v, want limit=200 enforce=true", got2)
			}
			if !got2.updatedAt.After(got.updatedAt) && !got2.updatedAt.Equal(got.updatedAt) {
				t.Errorf("updated_at did not advance: before=%v after=%v", got.updatedAt, got2.updatedAt)
			}

			// KNOWN-POSITIVE: writing again with the STALE (first) revision must
			// be refused, not silently applied over the second writer's change.
			// This is cleat#2046 owner decision 3B's explicit requirement --
			// "mirroring set-tenant-setting's shape including its stale-updated_at
			// refusal" -- falsified directly: writing with `got` (now stale)
			// rather than `got2` must return ErrQuotaConflict.
			stale := got
			stale.limitCount = 999
			err = writeQuota(ctx, exec, tc.d, tenant, resource, got, stale)
			if !errors.Is(err, ErrQuotaConflict) {
				t.Fatalf("writing with a stale revision = %v, want ErrQuotaConflict", err)
			}
			// And the row must be UNCHANGED by the refused write.
			got3, err := readQuota(ctx, exec, tc.d, tenant, resource)
			if err != nil {
				t.Fatalf("reading after a refused write: %v", err)
			}
			if got3.limitCount != 200 {
				t.Fatalf("a refused write still changed limit_count to %d, want 200 unchanged", got3.limitCount)
			}

			// KNOWN-POSITIVE, the other precondition: creating a row that
			// ALREADY exists (current.existed == false, but a row is actually
			// there) must also refuse rather than silently overwrite. This
			// exercises the INSERT branch's primary-key-violation path.
			err = writeQuota(ctx, exec, tc.d, tenant, resource, quotaRow{existed: false}, quotaRow{limitCount: 1, windowSeconds: 1})
			if !errors.Is(err, ErrQuotaConflict) {
				t.Fatalf("creating over an existing row = %v, want ErrQuotaConflict", err)
			}

			// A different tenant sees nothing: the read is scoped by tenant_id.
			// A fresh quotaConnFor for `other`: on MSSQL this is a SEPARATE
			// connection keyed to a DIFFERENT tenant, which is the actual case
			// being tested -- reusing `exec` here would still carry `tenant`'s
			// session key and prove nothing about cross-tenant isolation.
			other := uuid.New().String()
			otherExec, closeOtherExec, err := quotaConnFor(ctx, db, tc.d, other)
			if err != nil {
				t.Fatalf("quotaConnFor for a different tenant: %v", err)
			}
			defer closeOtherExec()
			otherQ, err := readQuota(ctx, otherExec, tc.d, other, resource)
			if err != nil || otherQ.existed {
				t.Errorf("a different tenant sees a row (%+v, err %v); the read is not scoped", otherQ, err)
			}
		})
	}
}

// TestQuotaFlags_AcceptsAnyOrder mirrors
// flags_work_after_the_tenant_argument_test.go's coverage for the rest of the
// package: parseQuotaFlags scans rather than uses flag.FlagSet, so this
// pins that --tenant, --resource and the value flags work in any order and
// with either `--flag value` or `--flag=value` spelling.
func TestQuotaFlags_AcceptsAnyOrder(t *testing.T) {
	tenant := uuid.New().String()
	for _, args := range [][]string{
		{"--tenant", tenant, "--limit-count", "10", "--window-seconds", "60"},
		{"--limit-count", "10", "--tenant", tenant, "--window-seconds", "60"},
		{"--limit-count=10", "--window-seconds=60", "--tenant=" + tenant},
		{"--window-seconds", "60", "--limit-count", "10", "--tenant", tenant},
	} {
		gotTenant, resource, limit, window, enforce, show, err := parseQuotaFlags(args)
		if err != nil {
			t.Errorf("parseQuotaFlags(%v): %v", args, err)
			continue
		}
		if gotTenant != tenant || limit != 10 || window != 60 {
			t.Errorf("parseQuotaFlags(%v) = tenant=%q limit=%d window=%d, want %q 10 60",
				args, gotTenant, limit, window, tenant)
		}
		if resource != defaultQuotaResource {
			t.Errorf("parseQuotaFlags(%v) resource = %q, want default %q", args, resource, defaultQuotaResource)
		}
		if enforce != "" || show {
			t.Errorf("parseQuotaFlags(%v) enforce=%q show=%v, want unset", args, enforce, show)
		}
	}
}

func TestQuotaFlags_RejectsBadEnforceValue(t *testing.T) {
	_, _, _, _, _, _, err := parseQuotaFlags([]string{"--tenant", uuid.New().String(), "--enforce", "yes"})
	if err == nil {
		t.Fatal("parseQuotaFlags accepted --enforce yes, want a rejection (only true/false)")
	}
}

func TestQuotaFlags_RejectsUnknownFlag(t *testing.T) {
	_, _, _, _, _, _, err := parseQuotaFlags([]string{"--tenant", uuid.New().String(), "--nonesuch", "x"})
	if err == nil || !strings.Contains(err.Error(), "nonesuch") {
		t.Fatalf("parseQuotaFlags(--nonesuch) = %v, want an error naming the flag", err)
	}
}

func TestQuotaGet_RejectsMissingTenant(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runGetQuota(context.Background(), nil, dialect{}, nil)
	})
	if !strings.Contains(stderr, "requires --tenant") {
		t.Errorf("quota get with no --tenant produced:\n%s", stderr)
	}
}

func TestQuotaSet_RejectsMissingTenant(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runSetQuota(context.Background(), nil, dialect{}, []string{"--limit-count", "1", "--window-seconds", "1"})
	})
	if !strings.Contains(stderr, "requires --tenant") {
		t.Errorf("quota set with no --tenant produced:\n%s", stderr)
	}
}

// TestQuotaSet_RejectsOversizedWindowSeconds is the known-positive for the
// CodeQL finding on cleat#2170 (pull/2170): --window-seconds is parsed as
// int64 but window_seconds is a 32-bit SQL column on all three dialects, and
// the narrowing int64->int conversion had no upper-bound check. A value
// above math.MaxInt32 must be refused before it reaches that conversion,
// not silently truncated into an unrelated, still-positive window.
func TestQuotaSet_RejectsOversizedWindowSeconds(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runSetQuota(context.Background(), nil, dialect{}, []string{
			"--tenant", uuid.New().String(),
			"--limit-count", "1",
			"--window-seconds", "5000000000",
		})
	})
	if !strings.Contains(stderr, "must fit in a 32-bit column") {
		t.Errorf("quota set with an oversized --window-seconds produced:\n%s", stderr)
	}
}

func TestQuotaSet_RejectsNonUUIDTenant(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runSetQuota(context.Background(), nil, dialect{}, []string{"--tenant", "not-a-uuid", "--limit-count", "1", "--window-seconds", "1"})
	})
	if !strings.Contains(stderr, "not a tenant UUID") {
		t.Errorf("quota set with a malformed tenant produced:\n%s", stderr)
	}
}
