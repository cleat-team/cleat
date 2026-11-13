package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3766: replay and debug read a run through the store of the tenant that
// owns it.
//
// cleatctl opens ONE store, for the default tenant. Since #3740 every PostgreSQL
// read of a definition, and already every read of an event history, carries the
// store's tenant, so a run of any other tenant read through it came back with the
// DEFAULT tenant's definition of the same name and version (or none) and no
// events. These tests give two tenants the same definition name and version, as
// two builds that differ in length, and one run of the non-default tenant with
// three events, on a connection that bypasses row-level security (the situation
// the issue is about), and ask what each command says it read.

const (
	ownTenantA     = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	ownTenantDef   = "own-tenant-def"
	ownTenantRun   = "own-tenant-run-1"
	ownTenantBuild = 25 // the owning tenant's build, in bytes
	defaultBuild   = 10 // the default tenant's build of the same name and version
	ownTenantEvs   = 3
)

type ownTenantFixture struct {
	db      *sql.DB
	factory engine.StoreFactory
	def     engine.WorkflowStore // the store main opens: the default tenant's
}

func newOwnTenantFixture(t *testing.T) *ownTenantFixture {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	ctx := context.Background()

	for _, tn := range []string{defaultTenantID, ownTenantA} {
		if _, err := db.ExecContext(ctx, `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			tn, "own-tenant-"+tn[:8]); err != nil {
			t.Fatalf("seed tenant %s: %v", tn, err)
		}
	}
	// The same name and version for both tenants: two builds, told apart by length.
	for tn, n := range map[string]int{defaultTenantID: defaultBuild, ownTenantA: ownTenantBuild} {
		if _, err := db.ExecContext(ctx, `INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id) VALUES ($1, 1, $2, $3)
			ON CONFLICT DO NOTHING`, ownTenantDef, make([]byte, n), tn); err != nil {
			t.Fatalf("seed workflow_defs for %s: %v", tn, err)
		}
	}
	testutil.SeedRun(t, db, testutil.RunSeed{ID: ownTenantRun, DefName: ownTenantDef, TenantID: ownTenantA})

	factory := engine.NewPostgresStoreFactory(db, "public")
	own, closeOwn, err := factory.OpenStore(ctx, ownTenantA)
	if err != nil {
		t.Fatalf("open the owning tenant's store: %v", err)
	}
	t.Cleanup(func() { closeOwn.Close() })
	for i := 0; i < ownTenantEvs; i++ {
		if err := own.AppendEventHistory(ctx, ownTenantRun, engine.EventRecord{
			Step: i, EventType: engine.EventTypeCall, Service: "svc", Op: "op", Response: fmt.Sprintf("r%d", i),
		}); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
	def, closeDef, err := factory.OpenStore(ctx, defaultTenantID)
	if err != nil {
		t.Fatalf("open the default tenant's store: %v", err)
	}
	t.Cleanup(func() { closeDef.Close() })

	prev := tenantStoreOpener
	tenantStoreOpener = func(ctx context.Context, tenantID string) (engine.WorkflowStore, io.Closer, error) {
		return factory.OpenStore(ctx, tenantID)
	}
	t.Cleanup(func() { tenantStoreOpener = prev })
	return &ownTenantFixture{db: db, factory: factory, def: def}
}

// run runs fn with osExit trapped, returning what it wrote. The commands exit 1
// after the part under test (the build is not a real module), which is fine: what
// is asserted is what they printed before it.
func runCapturing(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	return withExitPanicOutput(t, fn)
}

func TestReplayOfARunOfAnotherTenantReadsThatTenantsHistoryAndDefinition(t *testing.T) {
	f := newOwnTenantFixture(t)
	stdout, stderr := runCapturing(t, func() {
		runReplay(context.Background(), f.def, f.db, dialectPostgres, []string{ownTenantRun, "--entry-point", "Main"})
	})
	for _, want := range []string{
		fmt.Sprintf("Events: %d", ownTenantEvs),
		fmt.Sprintf("WASM size: %d bytes", ownTenantBuild),
		"Tenant: " + ownTenantA,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("replay output lacks %q.\n stdout: %s\n stderr: %s", want, stdout, stderr)
		}
	}
	if strings.Contains(stdout, fmt.Sprintf("WASM size: %d bytes", defaultBuild)) {
		t.Errorf("replay read the DEFAULT tenant's build of the same name and version:\n%s", stdout)
	}
}

func TestDebugOfARunOfAnotherTenantReadsThatTenantsHistoryAndDefinition(t *testing.T) {
	f := newOwnTenantFixture(t)
	stdout, stderr := runCapturing(t, func() {
		runDebug(context.Background(), f.def, f.db, dialectPostgres, []string{ownTenantRun, "--entry-point", "Main"})
	})
	for _, want := range []string{
		fmt.Sprintf("Events: %d", ownTenantEvs),
		fmt.Sprintf("WASM size: %d bytes", ownTenantBuild),
		"Tenant: " + ownTenantA,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("debug output lacks %q.\n stdout: %s\n stderr: %s", want, stdout, stderr)
		}
	}
}

func TestDebugWatchOfARunOfAnotherTenantCountsThatTenantsEvents(t *testing.T) {
	f := newOwnTenantFixture(t)
	// runDebugWatch counts, prints, then waits on the context: cancel it after the
	// count has had time to happen rather than before, which would fail the lookup.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(1500*time.Millisecond, cancel)
	defer cancel()
	stdout, stderr := runCapturing(t, func() {
		runDebug(ctx, f.def, f.db, dialectPostgres, []string{ownTenantRun, "--watch"})
	})
	if want := fmt.Sprintf("(%d events so far)", ownTenantEvs); !strings.Contains(stdout, want) {
		t.Errorf("watch did not count the owning tenant's events, want %q.\n stdout: %s\n stderr: %s", want, stdout, stderr)
	}
}

// A run of the default tenant prints exactly what it always did: no Tenant line,
// and the opener is never asked for a second store.
func TestARunOfTheDefaultTenantIsReadThroughTheStoreMainOpened(t *testing.T) {
	f := newOwnTenantFixture(t)
	opened := 0
	tenantStoreOpener = func(context.Context, string) (engine.WorkflowStore, io.Closer, error) {
		opened++
		return nil, nil, fmt.Errorf("must not be called")
	}
	testutil.SeedRun(t, f.db, testutil.RunSeed{ID: "default-tenant-run", DefName: ownTenantDef, TenantID: defaultTenantID})

	stdout, _ := runCapturing(t, func() {
		runReplay(context.Background(), f.def, f.db, dialectPostgres, []string{"default-tenant-run", "--entry-point", "Main"})
	})
	if opened != 0 {
		t.Errorf("a second store was opened %d times for a run of the default tenant", opened)
	}
	if strings.Contains(stdout, "Tenant:") {
		t.Errorf("a run of the default tenant printed a Tenant line:\n%s", stdout)
	}
	if want := fmt.Sprintf("WASM size: %d bytes", defaultBuild); !strings.Contains(stdout, want) {
		t.Errorf("want %q (the default tenant's own build):\n%s", want, stdout)
	}
}

// MySQL keeps a tenant in a database of its own, and OpenStore for one whose
// database is missing creates it. A read-only command must refuse, and never open.
// Known-positive: the same run on PostgreSQL does open a store (the first test).
func TestARunOfAnotherTenantOnMySQLIsRefusedAndNoStoreIsOpened(t *testing.T) {
	opened := 0
	prev := tenantStoreOpener
	tenantStoreOpener = func(context.Context, string) (engine.WorkflowStore, io.Closer, error) {
		opened++
		return nil, nil, fmt.Errorf("must not be called")
	}
	t.Cleanup(func() { tenantStoreOpener = prev })

	inst := &engine.WorkflowInstance{ID: "wf-1", TenantID: ownTenantA}
	_, _, err := storeForInstance(context.Background(), nil, dialectMySQL, inst)
	if err == nil {
		t.Fatal("a run of another tenant on MySQL was not refused")
	}
	if opened != 0 {
		t.Errorf("a store was opened %d times on MySQL, which would create the tenant's database", opened)
	}
	for _, want := range []string{ownTenantA, "database of its own", "would create its database"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
}

func TestStoreForInstanceRefusesATenantThatIsNotAUUID(t *testing.T) {
	_, _, err := storeForInstance(context.Background(), nil, dialectPostgres, &engine.WorkflowInstance{ID: "wf-1", TenantID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), "not a UUID") {
		t.Fatalf("err = %v, want a refusal naming that it is not a UUID", err)
	}
}

// A connection that row-level security applies to cannot see another tenant's
// run, and the message must say that, not just "not found". The role is the one
// the worker's own tests model.
func TestAConnectionThatRLSAppliesToSaysTheRunMayNotBeVisible(t *testing.T) {
	f := newOwnTenantFixture(t)
	testutil.SetupPostgresRLSRole(t, f.db)
	appDB := testutil.OpenPostgresRLSTestDB(t, f.db)
	defer appDB.Close()

	_, stderr := runCapturing(t, func() {
		runReplay(context.Background(), f.def, appDB, dialectPostgres, []string{ownTenantRun, "--entry-point", "Main"})
	})
	for _, want := range []string{
		"error loading workflow instance",
		"subject to row-level security",
		"not visible to this connection's tenant",
		"no sharded store",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("what an RLS-subject connection printed lacks %q:\n%s", want, stderr)
		}
	}
	// Known-positive: the SAME run through the superuser connection is read fine,
	// so the refusal above is about the connection and not about the run.
	stdout, _ := runCapturing(t, func() {
		runReplay(context.Background(), f.def, f.db, dialectPostgres, []string{ownTenantRun, "--entry-point", "Main"})
	})
	if !strings.Contains(stdout, "Workflow: "+ownTenantRun) {
		t.Errorf("the run is not readable through the superuser connection either, so the test above proves nothing:\n%s", stdout)
	}
}

func TestANotFoundRunSaysItMayNotBeVisibleAndThatThereIsOneDatabase(t *testing.T) {
	f := newOwnTenantFixture(t)
	_, stderr := runCapturing(t, func() {
		runReplay(context.Background(), f.def, f.db, dialectPostgres, []string{"no-such-run", "--entry-point", "Main"})
	})
	for _, want := range []string{
		"not found, or is not visible to this connection's tenant",
		"BYPASSRLS",
		"no sharded store",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the not-found message does not contain %q:\n%s", want, stderr)
		}
	}
}
