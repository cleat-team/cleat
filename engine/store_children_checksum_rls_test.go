package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestStartChildWorkflowAtomicChainsChecksumUnderRLS covers the one statement
// in StartChildWorkflowAtomic that its sibling RLS test cannot reach.
//
// StartChildWorkflowAtomic chains its event's checksum onto the previous
// step's, and to do that it has to read that previous checksum. Every other
// statement in the function runs on the open tx; this one read ran on s.db --
// the raw pool, with no RLS context set -- and discarded its error:
//
//	if event.Step > 1 {
//	    s.db.QueryRowContext(ctx,
//	        `SELECT COALESCE(checksum, '') FROM event_history WHERE workflow_id = $1 AND step = $2`,
//	        parentID, event.Step-1).Scan(&prevCS)      // error dropped
//	}
//	checksum := computeEventChecksum(event, prevCS)
//
// event_history carries FORCE ROW LEVEL SECURITY with
// `tenant_id = cleat.assert_tenant_set()` (migrations/postgres/001_schema.sql),
// and assert_tenant_set RAISES when cleat.tenant_id is unset. On a pooled
// connection that never had set_config called on it, that read therefore does
// not return a row -- it errors, measured 2026-08-31 as
//
//	pq: cleat.tenant_id is not set -- tenant context required for RLS-scoped
//	query (P0001)
//
// With the error dropped, prevCS stays "" and the event is checksummed as
// though it had no predecessor.
//
// One subtlety worth recording, because it makes the bug look intermittent:
// the raise only happens when a candidate row exists. PostgreSQL evaluates the
// policy expression per row, so against an empty event_history the same read
// returns plain sql.ErrNoRows and never calls assert_tenant_set at all. A probe
// that forgets to seed a row therefore "proves" the read is harmless. In
// production a step-1 row is always present when Step > 1, which is exactly the
// case that raises.
//
// The damage is not the failed read. It is that VerifyWorkflowEvents later
// recomputes the chain and compares, so a legitimate, untampered history fails
// verification with "checksum mismatch". The integrity feature reports tamper
// evidence for a row cleat itself wrote wrong.
//
// Why the existing coverage missed it, twice over:
//
//   - TestStartChildWorkflowAtomicUnderRLS drives the same function through the
//     same non-superuser connection, but with Step: 1. The bad read sits behind
//     `if event.Step > 1`, so the one test aimed at this function under RLS
//     skips the only statement in it that is not RLS-safe.
//   - Every non-RLS test connects as a superuser, which PostgreSQL exempts from
//     RLS entirely, so the read succeeds there and the chain looks right.
//
// So this test needs both halves at once: a non-superuser connection AND a step
// greater than 1. Against the unfixed code it fails with the stored checksum
// equal to computeEventChecksum(event, "").
func TestStartChildWorkflowAtomicChainsChecksumUnderRLS(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)

	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	// Deferred after appDB.Close() so it runs first (defers are LIFO).
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	tenant := "dddddddd-dddd-4ddd-dddd-dddddddddddd"
	store := NewPostgresStore(appDB).WithTenant(tenant)

	const defName = "rls-checksum-parent"
	const childDefName = "rls-checksum-child"
	for _, name := range []string{defName, childDefName} {
		def := &WorkflowDef{
			Name:       name,
			Version:    1,
			WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1,
			MinVersion: 1,
		}
		if err := store.DeployWorkflowDef(ctx, def); err != nil {
			t.Fatalf("DeployWorkflowDef(%s): %v", name, err)
		}
	}

	parentID := fmt.Sprintf("rls-checksum-parent-%d", time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, parentID, defName, 1, json.RawMessage(`{}`), "", tenant, 0); err != nil {
		t.Fatalf("StartNewRun(parent): %v", err)
	}

	// A real step-1 event, so there is a genuine predecessor to chain onto.
	// This goes through AppendEventHistory, which uses previousStoredChecksum
	// -- the correct, tx-scoped, tenant-qualified version of the same read.
	step1 := EventRecord{
		Step:        1,
		EventType:   EventTypeCall,
		TimestampMs: time.Now().UnixMilli(),
		Service:     "svc",
		Op:          "op",
		Request:     `{"a":1}`,
		Response:    `{"b":2}`,
	}
	if err := store.AppendEventHistory(ctx, parentID, step1); err != nil {
		t.Fatalf("AppendEventHistory(step 1): %v", err)
	}

	var step1Checksum string
	if err := adminDB.QueryRow(
		`SELECT COALESCE(checksum, '') FROM event_history WHERE workflow_id = $1 AND step = 1`,
		parentID,
	).Scan(&step1Checksum); err != nil {
		t.Fatalf("reading step 1 checksum: %v", err)
	}
	if step1Checksum == "" {
		t.Fatal("step 1 stored no checksum, so this test cannot distinguish a " +
			"correctly chained step 2 from an unchained one -- it would pass either way")
	}

	childID := fmt.Sprintf("rls-checksum-child-%d", time.Now().UnixNano())
	step2 := EventRecord{
		Step:        2,
		EventType:   EventTypeChildWorkflow,
		TimestampMs: time.Now().UnixMilli(),
		ChildName:   childDefName,
		ChildInput:  `{"k":"v"}`,
	}

	if _, err := store.StartChildWorkflowAtomic(ctx, childID, parentID, childDefName,
		`{"k":"v"}`, 1, "ABANDON", step2, 0); err != nil {
		t.Fatalf("StartChildWorkflowAtomic: %v", err)
	}

	// Read back through adminDB: the superuser connection is exempt from RLS,
	// so it reports what was actually stored rather than what this tenant can
	// see. A wrong-tenant row would otherwise read as "missing", a weaker
	// signal than comparing the value.
	var storedChecksum string
	if err := adminDB.QueryRow(
		`SELECT COALESCE(checksum, '') FROM event_history WHERE workflow_id = $1 AND step = 2`,
		parentID,
	).Scan(&storedChecksum); err != nil {
		t.Fatalf("reading step 2 checksum: %v", err)
	}

	// step2 is passed by value but StartChildWorkflowAtomic sets RunID on its
	// copy before checksumming, so reproduce that here.
	expectEvent := step2
	expectEvent.RunID = childID
	want := computeEventChecksum(expectEvent, step1Checksum)
	unchained := computeEventChecksum(expectEvent, "")

	if storedChecksum == unchained && want != unchained {
		t.Fatalf("step 2 was checksummed against an empty predecessor.\n"+
			"  stored:            %s\n"+
			"  want (chained):    %s\n"+
			"  unchained:         %s\n\n"+
			"StartChildWorkflowAtomic read the previous checksum on s.db (the raw pool, "+
			"no RLS context) and discarded the error. Under a non-superuser role "+
			"cleat.assert_tenant_set() raises, prevCS stays empty, and the chain is "+
			"broken -- which VerifyWorkflowEvents then reports as tampering.",
			storedChecksum, want, unchained)
	}
	if storedChecksum != want {
		t.Fatalf("step 2 checksum = %s, want %s (chained onto step 1's %s)",
			storedChecksum, want, step1Checksum)
	}

	// The consequence, asserted directly: an untampered history must verify.
	if err := store.VerifyWorkflowEvents(ctx, parentID); err != nil {
		t.Errorf("VerifyWorkflowEvents on an untampered history: %v\n\n"+
			"This is what the broken chain costs in production -- cleat reporting "+
			"tamper evidence against a row it wrote itself.", err)
	}
}
