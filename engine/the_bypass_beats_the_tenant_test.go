package engine

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
)

// TestTheBypassBeatsTheTenantAndIsSilentAboutIt pins the precedence inside
// beginTenantTx, because the only thing asserting it today is a comment.
//
// The ordering is deliberate and documented: a context marked with
// plugin.AcrossAllTenants widens rather than narrows, so an admin endpoint that
// rebuilds an index for every tenant still sweeps every tenant when it runs on
// a request context that carries the caller's own. Resolving it the other way
// would quietly scope that sweep to whoever called it.
//
// The consequence is the hazard, and it is the reason this test exists rather
// than the ordering itself: plugin.ForTenant inside an AcrossAllTenants scope
// is IGNORED, and nothing says so. No error, no log line -- the tenant branch
// is simply never reached. Two plugins hit the "a tenant that went missing"
// shape independently in one evening (auditlog's recordAudit, eventtriggers'
// retryEvent), so the combination of both markers on one context is a live
// possibility rather than a hypothetical, and a doc comment asserting an
// ordering is exactly the prose that rots.
//
// THE LOAD-BEARING ASSERTION IS THAT cleat.tenant_id IS EMPTY in the first
// case. "cross_tenant is set" passes equally against an implementation that
// sets BOTH -- which would be a policy-visible difference, since
// cleat.tenant_row_is_visible reads cross_tenant first but a later policy
// written the other way would not. Asserting only the positive is the version
// of this test that cannot fail.
func TestTheBypassBeatsTheTenantAndIsSilentAboutIt(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })

	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	const reason = "a sweep that named itself"

	// settings reads both GUCs back from inside the transaction. missing_ok is
	// true because an unset setting must read as "" rather than raise -- which
	// is also how the policy tests it (see beginTenantTx's note on emptiness
	// versus presence).
	settings := func(t *testing.T, tx *sql.Tx) (tenantID, crossTenant string) {
		t.Helper()
		if err := tx.QueryRow(
			`SELECT coalesce(current_setting('cleat.tenant_id', true), ''),
			        coalesce(current_setting('cleat.cross_tenant', true), '')`,
		).Scan(&tenantID, &crossTenant); err != nil {
			t.Fatalf("reading the settings back: %v", err)
		}
		return tenantID, crossTenant
	}

	t.Run("both markers: the bypass wins and the tenant is not set at all", func(t *testing.T) {
		ctx := tenantctx.WithCrossTenant(tenantctx.With(context.Background(), tenant), reason)
		tx, err := beginTenantTx(ctx, db, plugin.DialectPostgres, nil)
		if err != nil {
			t.Fatalf("beginTenantTx: %v", err)
		}
		if tx == nil {
			t.Fatal("expected a transaction, got nil")
		}
		defer func() { _ = tx.Rollback() }()

		tid, cross := settings(t, tx)
		if cross != reason {
			t.Errorf("cleat.cross_tenant = %q, want %q", cross, reason)
		}
		if tid != "" {
			t.Errorf("cleat.tenant_id = %q, want empty.\n\n"+
				"The bypass is tested before the tenant, so the tenant branch must never run. "+
				"If both are set, a plugin that called ForTenant inside an AcrossAllTenants scope "+
				"would appear to be scoped while the policy admits every row -- the worst of the "+
				"three outcomes, because it looks correct at the call site.", tid)
		}
	})

	t.Run("tenant only: scoped, and no bypass leaks in", func(t *testing.T) {
		ctx := tenantctx.With(context.Background(), tenant)
		tx, err := beginTenantTx(ctx, db, plugin.DialectPostgres, nil)
		if err != nil {
			t.Fatalf("beginTenantTx: %v", err)
		}
		if tx == nil {
			t.Fatal("expected a transaction, got nil")
		}
		defer func() { _ = tx.Rollback() }()

		tid, cross := settings(t, tx)
		if tid != tenant.String() {
			t.Errorf("cleat.tenant_id = %q, want %q", tid, tenant)
		}
		// NEGATIVE CONTROL for the case above: this is what proves the first
		// subtest measured the ordering rather than an implementation that
		// never sets cleat.tenant_id at all.
		if cross != "" {
			t.Errorf("cleat.cross_tenant = %q, want empty -- nothing asked for a bypass", cross)
		}
	})

	t.Run("bypass only: set, which is the other half of the control", func(t *testing.T) {
		ctx := tenantctx.WithCrossTenant(context.Background(), reason)
		tx, err := beginTenantTx(ctx, db, plugin.DialectPostgres, nil)
		if err != nil {
			t.Fatalf("beginTenantTx: %v", err)
		}
		if tx == nil {
			t.Fatal("expected a transaction, got nil")
		}
		defer func() { _ = tx.Rollback() }()

		if _, cross := settings(t, tx); cross != reason {
			t.Errorf("cleat.cross_tenant = %q, want %q", cross, reason)
		}
	})

	t.Run("an empty reason is refused rather than treated as a bypass", func(t *testing.T) {
		ctx := tenantctx.WithCrossTenant(tenantctx.With(context.Background(), tenant), "   ")
		tx, err := beginTenantTx(ctx, db, plugin.DialectPostgres, nil)
		if tx != nil {
			_ = tx.Rollback()
			t.Fatal("expected no transaction for an empty reason")
		}
		if err == nil {
			t.Fatal("expected an error for a whitespace-only reason, got nil.\n\n" +
				"Falling through to the tenant branch here would scope the statement to the " +
				"caller's tenant, which is the opposite of what a sweep asked for, and would " +
				"do it silently.")
		}
	})

	t.Run("neither marker: no transaction and no error", func(t *testing.T) {
		tx, err := beginTenantTx(context.Background(), db, plugin.DialectPostgres, nil)
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
		if tx != nil {
			_ = tx.Rollback()
			t.Fatal("expected nil transaction when the context carries neither marker: " +
				"an unscoped caller must be left exactly as it was")
		}
	})
}
