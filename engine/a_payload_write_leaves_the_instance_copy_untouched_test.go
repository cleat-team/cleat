package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// cleat#3245 B1b: on PostgreSQL, no write path puts a payload column on
// workflow_instances any more. Since B1a every reader takes those columns
// from workflow_payloads, so a value still landing on workflow_instances is
// a write nobody reads -- and the moment the baseline regeneration drops the
// columns (cleat#3362), the same write is a "column does not exist" error.
//
// Each subtest drives one write path on a fresh run, then asserts the
// instance row still holds the column defaults: input and query_state the
// '{}' their DEFAULT gives, everything else NULL. def_name and def_version
// are not checked: they are NOT NULL and carry the workflow_defs foreign
// key, so every insert must still supply them until the columns go.
//
// The payload side of each path is asserted by B1a's diverging tests and by
// the per-path lease-and-payload tests; this file asserts only the absence.

// payloadColumnsTable names the table a dialect keeps workflow payload
// columns in: workflow_payloads on PostgreSQL since cleat#3245 B1b,
// workflow_instances on MySQL and SQL Server, which have no split. dialect is
// a StoreBackend name.
func payloadColumnsTable(dialect string) string {
	if dialect == "postgres" {
		return "workflow_payloads"
	}
	return "workflow_instances"
}

// instancePayloadColumns reads the payload columns of one workflow_instances
// row as text, NULL as nil.
func instancePayloadColumns(t *testing.T, db *sql.DB, id string) map[string]*string {
	t.Helper()
	cols := []string{"input", "result", "query_state", "compaction_state", "compaction_step",
		"allowed_signals", "error_msg", "error_code", "error_op", "trace_id", "completed_at"}
	vals := make([]sql.NullString, len(cols))
	dest := make([]any, len(cols))
	for i := range vals {
		dest[i] = &vals[i]
	}
	if err := db.QueryRow(`SELECT input::text, result::text, query_state::text, compaction_state::text,
		compaction_step::text, allowed_signals::text, error_msg, error_code, error_op, trace_id,
		completed_at::text FROM workflow_instances WHERE id = $1`, id).Scan(dest...); err != nil {
		t.Fatalf("read workflow_instances payload columns for %s: %v", id, err)
	}
	out := make(map[string]*string, len(cols))
	for i, c := range cols {
		if vals[i].Valid {
			v := vals[i].String
			out[c] = &v
		} else {
			out[c] = nil
		}
	}
	return out
}

func assertInstancePayloadUntouched(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	for col, v := range instancePayloadColumns(t, db, id) {
		switch col {
		case "input", "query_state":
			if v != nil && *v != "{}" {
				t.Errorf("workflow_instances.%s = %s, want its '{}' default -- a write path still puts this payload column on workflow_instances", col, *v)
			}
		default:
			if v != nil {
				t.Errorf("workflow_instances.%s = %q, want NULL -- a write path still puts this payload column on workflow_instances", col, *v)
			}
		}
	}
}

func TestAPayloadWriteLeavesTheInstanceCopyUntouched(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	const worker = "b1b-worker"
	qs := map[string]string{"k": "v"}

	start := func(t *testing.T, name string) string {
		t.Helper()
		return startPayloadReaderRun(t, store, "b1b-"+name)
	}
	claim := func(t *testing.T, id string) int64 {
		t.Helper()
		wf, err := store.ClaimWorkflow(ctx, worker)
		if err != nil {
			t.Fatalf("ClaimWorkflow: %v", err)
		}
		if wf == nil || wf.ID != id {
			t.Fatalf("ClaimWorkflow claimed %v, want %s", wf, id)
		}
		return wf.Generation
	}
	// Each subtest starts from an empty table, so ClaimWorkflow claims the
	// run that subtest started.
	clean := func(t *testing.T) {
		t.Helper()
		if _, err := db.Exec(`DELETE FROM workflow_instances`); err != nil {
			t.Fatalf("clear workflow_instances: %v", err)
		}
	}

	paths := []struct {
		name string
		run  func(t *testing.T, id string) []string // returns every id to check
	}{
		{"start", func(t *testing.T, id string) []string { return []string{id} }},
		{"complete", func(t *testing.T, id string) []string {
			gen := claim(t, id)
			if err := store.CompleteWorkflow(ctx, id, worker, gen, `{"r":1}`, qs); err != nil {
				t.Fatalf("CompleteWorkflow: %v", err)
			}
			return []string{id}
		}},
		{"finalize done", func(t *testing.T, id string) []string {
			gen := claim(t, id)
			if err := store.FinalizeWorkflowSegment(ctx, id, worker, gen, nil, "done", `{"r":1}`, "", "", qs, time.Time{}); err != nil {
				t.Fatalf("FinalizeWorkflowSegment(done): %v", err)
			}
			return []string{id}
		}},
		{"finalize ready", func(t *testing.T, id string) []string {
			gen := claim(t, id)
			if err := store.FinalizeWorkflowSegment(ctx, id, worker, gen, nil, "ready", "", "", "", qs, time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("FinalizeWorkflowSegment(ready): %v", err)
			}
			return []string{id}
		}},
		{"fail", func(t *testing.T, id string) []string {
			gen := claim(t, id)
			if err := store.FailWorkflow(ctx, id, worker, gen, "boom", "E_B1B", "op", qs); err != nil {
				t.Fatalf("FailWorkflow: %v", err)
			}
			return []string{id}
		}},
		{"dead letter", func(t *testing.T, id string) []string {
			gen := claim(t, id)
			if err := store.MoveToDeadLetterQueue(ctx, id, worker, gen, "boom", "E_B1B", "op", qs); err != nil {
				t.Fatalf("MoveToDeadLetterQueue: %v", err)
			}
			return []string{id}
		}},
		{"terminate", func(t *testing.T, id string) []string {
			if err := store.TerminateWorkflow(ctx, id, "stop"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}
			return []string{id}
		}},
		{"cancel", func(t *testing.T, id string) []string {
			if err := store.CancelWorkflow(ctx, id, "stop"); err != nil {
				t.Fatalf("CancelWorkflow: %v", err)
			}
			return []string{id}
		}},
		{"admin force complete", func(t *testing.T, id string) []string {
			if err := store.AdminForceComplete(ctx, id, 0, `{"ok":true}`, "operator"); err != nil {
				t.Fatalf("AdminForceComplete: %v", err)
			}
			return []string{id}
		}},
		{"admin force fail", func(t *testing.T, id string) []string {
			if err := store.AdminForceFail(ctx, id, 0, "forced", "E_FORCED", "operator"); err != nil {
				t.Fatalf("AdminForceFail: %v", err)
			}
			return []string{id}
		}},
		{"trace", func(t *testing.T, id string) []string {
			if err := store.TraceWorkflow(ctx, id, "trace-b1b"); err != nil {
				t.Fatalf("TraceWorkflow: %v", err)
			}
			return []string{id}
		}},
		{"allowed signals", func(t *testing.T, id string) []string {
			if err := store.SetAllowedSignalCallers(ctx, id, []string{"svc-b1b"}); err != nil {
				t.Fatalf("SetAllowedSignalCallers: %v", err)
			}
			return []string{id}
		}},
		{"compact history", func(t *testing.T, id string) []string {
			if err := store.CompactHistory(ctx, id, []byte(`{"s":1}`), 3, 3); err != nil {
				t.Fatalf("CompactHistory: %v", err)
			}
			return []string{id}
		}},
		{"child", func(t *testing.T, id string) []string {
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: "b1b-child", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef(child): %v", err)
			}
			child, err := store.StartChildWorkflow(ctx, id, "b1b-child", `{"c":1}`, 1, "", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}
			return []string{id, child}
		}},
		{"continue as new", func(t *testing.T, id string) []string {
			gen := claim(t, id)
			next, err := store.ContinueAsNew(ctx, id, worker, gen, "b1b-continue as new", 1,
				json.RawMessage(`{"n":2}`), nil, `{"ok":true}`, qs, 0)
			if err != nil {
				t.Fatalf("ContinueAsNew: %v", err)
			}
			return []string{id, next}
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			clean(t)
			id := start(t, p.name)
			for _, checked := range p.run(t, id) {
				assertInstancePayloadUntouched(t, db, checked)
			}
		})
	}
}
