package engine

// Tests for the claim-limit invariant backstop. See claim_limit.go for the
// IMPROVEMENT-PLAN.md 2.11 history: a claim for 3 that returned 10, cause
// still unknown, and the plan's instruction to capture evidence if it recurs.
//
// The unit tests below cover the decision. The database test covers the part
// that would actually matter on recurrence -- that the excess is handed back
// to `ready` rather than left claimed by a worker that will never run it,
// which is the 2.17 stranding shape.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func instances(ids ...string) []*WorkflowInstance {
	out := make([]*WorkflowInstance, 0, len(ids))
	for _, id := range ids {
		out = append(out, &WorkflowInstance{ID: id})
	}
	return out
}

func TestEnforceClaimLimit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		limit      int
		claimed    []*WorkflowInstance
		wantKeep   int
		wantExcess int
		wantLog    bool
	}{
		{"under the limit", 3, instances("a", "b"), 2, 0, false},
		{"exactly the limit", 3, instances("a", "b", "c"), 3, 0, false},
		{"the 2.11 observation", 3, instances("a", "b", "c", "d", "e", "f", "g", "h", "i", "j"), 3, 7, true},
		{"one over", 1, instances("a", "b"), 1, 1, true},
		{"empty", 3, nil, 0, 0, false},
		// limit <= 0 is not a claim for zero rows anywhere in the store; it
		// must not be read as "everything is excess".
		{"zero limit is not enforced", 0, instances("a", "b"), 2, 0, false},
		{"negative limit is not enforced", -1, instances("a"), 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, nil))

			keep, excess := enforceClaimLimit(context.Background(), log, "postgres", "worker-1", tc.limit, tc.claimed)

			if len(keep) != tc.wantKeep || len(excess) != tc.wantExcess {
				t.Errorf("keep=%d excess=%d, want keep=%d excess=%d", len(keep), len(excess), tc.wantKeep, tc.wantExcess)
			}
			if logged := strings.Contains(buf.String(), "more workflows than its limit"); logged != tc.wantLog {
				t.Errorf("logged=%v, want %v (log: %s)", logged, tc.wantLog, buf.String())
			}
			if tc.wantLog {
				// The log line is the whole point: 2.11 could not be
				// diagnosed because nothing recorded what happened.
				for _, want := range []string{"limit=3", "returned=10", "worker_id=worker-1", "dialect=postgres"} {
					if tc.limit == 3 && !strings.Contains(buf.String(), want) {
						t.Errorf("log line is missing %q, so a recurrence would still be undiagnosable: %s", want, buf.String())
					}
				}
			}
		})
	}
}

// TestFinishClaim_ReleasesTheExcess is the one that matters. Truncating an
// over-claim without releasing is what made 2.17 a bug rather than a
// nuisance: the rows stay 'running' with assigned_to set, held by a worker
// that will never execute them, until the lease expires.
//
// It constructs the over-claim directly -- claim 10 rows, then hand
// finishClaim a limit of 3 -- because the SQL is not known to over-claim and
// 24,000 attempts failed to make it do so. What is being tested is the
// recovery, not the trigger.
func TestFinishClaim_ReleasesTheExcess(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	tenant := "eeeeeeee-eeee-4eee-eeee-eeeeeeeeeeee"
	store := NewPostgresStore(adminDB).WithTenant(tenant)

	const defName = "claim-limit-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	stamp := time.Now().UnixNano()
	for i := 0; i < 10; i++ {
		if _, _, err := store.StartNewRun(ctx, fmt.Sprintf("claim-limit-%d-%d", stamp, i), defName, 1,
			json.RawMessage(`{}`), "", tenant, 0); err != nil {
			t.Fatalf("StartNewRun[%d]: %v", i, err)
		}
	}

	const workerID = "worker-over-claim"
	claimed, err := store.ClaimWorkflows(ctx, workerID, 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	if len(claimed) != 10 {
		t.Fatalf("setup claimed %d rows, want 10", len(claimed))
	}

	// Replay the over-claim: the same ten rows, presented as the result of a
	// claim whose limit was 3.
	tx, err := store.beginTxWithRLS(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	keep, err := store.finishClaim(ctx, tx, workerID, 3, claimed)
	if err != nil {
		t.Fatalf("finishClaim: %v", err)
	}
	if len(keep) != 3 {
		t.Fatalf("finishClaim returned %d workflows for a limit of 3", len(keep))
	}

	kept := map[string]bool{}
	for _, wf := range keep {
		kept[wf.ID] = true
	}

	for _, wf := range claimed {
		var status string
		var assignedTo *string
		if err := adminDB.QueryRowContext(ctx,
			`SELECT status, assigned_to FROM workflow_instances WHERE id = $1`, wf.ID).Scan(&status, &assignedTo); err != nil {
			t.Fatalf("status query for %s: %v", wf.ID, err)
		}

		if kept[wf.ID] {
			if status != "running" {
				t.Errorf("kept workflow %s has status %q, want %q", wf.ID, status, "running")
			}
			continue
		}
		if status != "ready" {
			owner := "<null>"
			if assignedTo != nil {
				owner = *assignedTo
			}
			t.Errorf("over-claimed workflow %s was not released: status=%q assigned_to=%s -- "+
				"it is claimed by a worker that will never execute it and stays that way until its lease expires",
				wf.ID, status, owner)
		}
	}
}
