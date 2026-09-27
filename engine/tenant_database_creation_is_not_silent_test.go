package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// A MySQL worker runs the whole migration set twice at startup -- once against
// the main database, once against the tenant database -- and the stretch
// between the two passes was unlogged. Measured 2026-09-09 over four cold
// starts, each pass was a steady 2s while that stretch ranged 1s to 9s in
// silence.
//
// Anything deciding "is this starting worker alive?" from log output reads that
// silence as a hang; cleat-ports' scripts/worker.sh does exactly that and gave
// up on a healthy worker. This test asserts the phase announces itself, so the
// silence cannot come back unnoticed. cleat#1084.
func TestCreatingATenantDatabaseAnnouncesItselfBeforeDoingTheWork(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()

	var buf bytes.Buffer
	factory := NewMySQLStoreFactory(s.db, mysqlTestBaseDSN(t)).
		WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	defer factory.Close()

	// A tenant of this test's own, so the assertion is about a creation that
	// definitely happened rather than a cache hit on somebody else's.
	const tenantID = "1084c0de-0000-4000-8000-000000000001"
	_ = factory.DropTenantDatabase(tenantID)
	defer func() { _ = factory.DropTenantDatabase(tenantID) }()

	if _, err := factory.CreateTenantDatabase(context.Background(), tenantID); err != nil {
		t.Fatalf("CreateTenantDatabase: %v", err)
	}

	out := buf.String()
	before := strings.Index(out, "creating tenant database")
	after := strings.Index(out, "tenant database ready")

	if before < 0 {
		t.Errorf("nothing was logged before the database was created; a worker stalling here is\n"+
			"indistinguishable from a hung one. Log:\n%s", out)
	}
	if after < 0 {
		t.Errorf("completion was not logged. Log:\n%s", out)
	}
	// Order is the property that matters. A line written only on the way out
	// still leaves the slow region silent while it is being waited on, which
	// is precisely the case this exists to prevent.
	if before >= 0 && after >= 0 && before > after {
		t.Errorf("the announcement was logged after the work, not before it. Log:\n%s", out)
	}
	if !strings.Contains(out, "duration_ms") {
		t.Errorf("no duration_ms on the completion record; how long this took is the fact that\n"+
			"had to be reconstructed by hand from migration timestamps. Log:\n%s", out)
	}
}

// The cached path must stay quiet: it does no work, and a line here would fire
// on every OpenStore, which is the opposite of making a rare slow phase visible.
func TestAnAlreadyCreatedTenantDatabaseLogsNothing(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()

	var buf bytes.Buffer
	factory := NewMySQLStoreFactory(s.db, mysqlTestBaseDSN(t)).
		WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	defer factory.Close()

	const tenantID = "1084c0de-0000-4000-8000-000000000002"
	_ = factory.DropTenantDatabase(tenantID)
	defer func() { _ = factory.DropTenantDatabase(tenantID) }()

	if _, err := factory.CreateTenantDatabase(context.Background(), tenantID); err != nil {
		t.Fatalf("first CreateTenantDatabase: %v", err)
	}
	buf.Reset()

	if _, err := factory.CreateTenantDatabase(context.Background(), tenantID); err != nil {
		t.Fatalf("second CreateTenantDatabase: %v", err)
	}
	if out := buf.String(); strings.TrimSpace(out) != "" {
		t.Errorf("the cached path logged, and it does no work worth announcing:\n%s", out)
	}
}
