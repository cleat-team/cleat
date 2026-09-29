package auditlog

// Tests for record_event, the workflow-callable host function cleat#2534 added.

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// callCtx builds the context the engine gives a plugin host function: a CallContext
// (what sendMessage's own test builds) PLUS plugin.ForTenant, which appendChained's own
// tests all use and chain_store.go's doc comment requires -- unlike a table sendMessage
// reads by an explicit tenant_id = $2 argument, audit_events and audit_chain_heads are
// row-level-secured, so a ctx with no tenant scoping fails closed inside appendChained
// rather than inside this function.
//
// RunID is set to workflowID, matching what the real engine does today
// (plugin.CallContext's own doc comment: "always set to the workflow id" --
// engine/plugin_call_context.go). step is the caller's own position in a
// sequence of calls sharing one workflow/tenant, and matters as of cleat#2618:
// two recordEvent calls through the SAME step now dedup by default (see
// recordEventCallCtxNoRunID below for the pre-#2618 shape, still exercised
// deliberately by TestRecordEventWithoutRunIDIsNotDeduped).
func recordEventCallCtx(tenant uuid.UUID, workflowID string, step int) context.Context {
	ctx := plugin.WithCallContext(context.Background(),
		&plugin.CallContext{TenantID: tenant.String(), WorkflowID: workflowID, RunID: workflowID, Step: step})
	return plugin.ForTenant(ctx, tenant)
}

// recordEventCallCtxNoRunID builds a CallContext the way one would have
// looked before cleat#2618 -- no RunID, no Step -- which is still a real
// shape: a plugin's own unit test, or an embedder that predates #2618,
// builds CallContext directly rather than through the engine's PluginCall
// dispatch. recordEvent must not default-dedup in this case (see its own
// doc comment); TestRecordEventWithoutRunIDIsNotDeduped pins that.
func recordEventCallCtxNoRunID(tenant uuid.UUID, workflowID string) context.Context {
	ctx := plugin.WithCallContext(context.Background(),
		&plugin.CallContext{TenantID: tenant.String(), WorkflowID: workflowID})
	return plugin.ForTenant(ctx, tenant)
}

// ---------------------------------------------------------------------------
// Against real databases
// ---------------------------------------------------------------------------

func TestRecordEventAppendsToTheCallersTenantChain(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()

		out, err := p.recordEvent(recordEventCallCtx(tenant, "wf-123", 0), RecordEventInput{
			EventType: "tenant.suspended",
			Details:   json.RawMessage(`{"reason":"trial_expired"}`),
		})
		if err != nil {
			t.Fatalf("recordEvent: %v", err)
		}
		if !out.Recorded {
			t.Fatalf("recordEvent output = %+v, want Recorded=true", out)
		}

		var method, path, userID, metadata string
		e.scan(tenant,
			`SELECT method, path, user_id, metadata FROM audit_events WHERE tenant_id = $1`,
			[]any{tenant.String()}, &method, &path, &userID, &metadata)
		if method != workflowEventMethod {
			t.Errorf("method = %q, want %q", method, workflowEventMethod)
		}
		if path != "tenant.suspended" {
			t.Errorf("path = %q, want the event_type", path)
		}
		if userID != "wf-123" {
			t.Errorf("user_id = %q, want the calling workflow's id", userID)
		}
		if !strings.Contains(metadata, "trial_expired") {
			t.Errorf("metadata = %q, want it to hold the supplied details", metadata)
		}

		rep := e.verify(tenant)
		if !rep.OK() {
			t.Fatalf("verify reported a break: %+v", rep.Break)
		}
		if rep.Checked != 1 {
			t.Fatalf("verify checked %d rows, want 1", rep.Checked)
		}
	})
}

func TestRecordEventDefaultsDetailsToAnEmptyObject(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()

		if _, err := p.recordEvent(recordEventCallCtx(tenant, "wf-no-details", 0),
			RecordEventInput{EventType: "tenant.provisioned"}); err != nil {
			t.Fatalf("recordEvent: %v", err)
		}

		var metadata string
		e.scan(tenant, `SELECT metadata FROM audit_events WHERE tenant_id = $1`,
			[]any{tenant.String()}, &metadata)
		if strings.TrimSpace(metadata) != "{}" {
			t.Errorf("metadata = %q, want the default empty object", metadata)
		}

		rep := e.verify(tenant)
		if !rep.OK() {
			t.Fatalf("verify reported a break: %+v", rep.Break)
		}
	})
}

// TestRecordEventTwoCallsChainCorrectly also proves the default step-based
// dedup (cleat#2618) does not collide two DIFFERENT real calls: each
// iteration is step i, matching how the engine would actually advance
// s.stepCount between two distinct record_event calls in one workflow.
func TestRecordEventTwoCallsChainCorrectly(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()

		for i, et := range []string{"tenant.provisioned", "tenant.plan_changed"} {
			if _, err := p.recordEvent(recordEventCallCtx(tenant, "wf-two-calls", i),
				RecordEventInput{EventType: et}); err != nil {
				t.Fatalf("recordEvent(%q, step=%d): %v", et, i, err)
			}
		}

		rep := e.verify(tenant)
		if !rep.OK() {
			t.Fatalf("verify reported a break: %+v", rep.Break)
		}
		if rep.Checked != 2 || rep.HeadSeq != 2 {
			t.Fatalf("verify: %+v, want 2 rows checked and head at seq 2", rep)
		}
	})
}

// TestRecordEventWithoutAnEventIDIsIdempotentByStep is the default-path
// counterpart to TestRecordEventWithAnEventIDIsIdempotent: two calls at the
// SAME step, no event_id supplied, append exactly one row -- simulating a
// worker crash-and-retry replaying the same PluginCall dispatch (same
// RunID, same Step) rather than a caller-chosen key.
func TestRecordEventWithoutAnEventIDIsIdempotentByStep(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		ctx := recordEventCallCtx(tenant, "wf-retry-same-step", 3)

		input := RecordEventInput{EventType: "tenant.provisioned"}
		for i := 0; i < 2; i++ {
			out, err := p.recordEvent(ctx, input)
			if err != nil {
				t.Fatalf("recordEvent call %d: %v", i+1, err)
			}
			if !out.Recorded {
				t.Fatalf("recordEvent call %d output = %+v, want Recorded=true", i+1, out)
			}
		}

		rep := e.verify(tenant)
		if !rep.OK() {
			t.Fatalf("verify reported a break: %+v", rep.Break)
		}
		if rep.Checked != 1 || rep.HeadSeq != 1 {
			t.Fatalf("verify: %+v, want exactly ONE row despite two calls at the same (RunID, Step)", rep)
		}
	})
}

// TestRecordEventWithoutRunIDIsNotDeduped proves the fallback: a CallContext
// with no RunID (built directly, not through the engine's PluginCall
// dispatch) gets the plain AtLeastOnce behaviour -- no default dedup, two
// calls with identical input append two rows, matching the pre-#2618 shape.
func TestRecordEventWithoutRunIDIsNotDeduped(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		ctx := recordEventCallCtxNoRunID(tenant, "wf-no-run-id")

		input := RecordEventInput{EventType: "tenant.provisioned"}
		for i := 0; i < 2; i++ {
			if _, err := p.recordEvent(ctx, input); err != nil {
				t.Fatalf("recordEvent call %d: %v", i+1, err)
			}
		}

		rep := e.verify(tenant)
		if !rep.OK() {
			t.Fatalf("verify reported a break: %+v", rep.Break)
		}
		if rep.Checked != 2 || rep.HeadSeq != 2 {
			t.Fatalf("verify: %+v, want 2 rows: no RunID means no default dedup", rep)
		}
	})
}

// TestRecordEventWithAnEventIDIsIdempotent is the falsifiable regression test
// for the closing-the-residual escape hatch cleat-review suggested on #2616:
// two calls carrying the same event_id append exactly one row, and the
// second call still reports success.
func TestRecordEventWithAnEventIDIsIdempotent(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		ctx := recordEventCallCtx(tenant, "wf-idempotent", 0)

		input := RecordEventInput{EventType: "tenant.provisioned", EventID: "provision-once"}
		for i := 0; i < 2; i++ {
			out, err := p.recordEvent(ctx, input)
			if err != nil {
				t.Fatalf("recordEvent call %d: %v", i+1, err)
			}
			if !out.Recorded {
				t.Fatalf("recordEvent call %d output = %+v, want Recorded=true", i+1, out)
			}
		}

		rep := e.verify(tenant)
		if !rep.OK() {
			t.Fatalf("verify reported a break: %+v", rep.Break)
		}
		if rep.Checked != 1 || rep.HeadSeq != 1 {
			t.Fatalf("verify: %+v, want exactly ONE row despite two calls with the same event_id", rep)
		}
	})
}

// TestRecordEventDifferentEventIDsAppendSeparately proves the idempotency
// test above isn't vacuous: two DIFFERENT event_ids for the same tenant and
// workflow must both land.
func TestRecordEventDifferentEventIDsAppendSeparately(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		ctx := recordEventCallCtx(tenant, "wf-distinct-ids", 0)

		for _, id := range []string{"step-1", "step-2"} {
			if _, err := p.recordEvent(ctx, RecordEventInput{EventType: "tenant.provisioned", EventID: id}); err != nil {
				t.Fatalf("recordEvent(event_id=%q): %v", id, err)
			}
		}

		rep := e.verify(tenant)
		if !rep.OK() {
			t.Fatalf("verify reported a break: %+v", rep.Break)
		}
		if rep.Checked != 2 || rep.HeadSeq != 2 {
			t.Fatalf("verify: %+v, want 2 distinct rows for 2 distinct event_ids", rep)
		}
	})
}

// TestRecordEventDeterministicIDIsScopedToTenantAndWorkflow proves the same
// event_id string, for a DIFFERENT tenant or a different workflow, is not
// treated as the same event -- otherwise two unrelated callers reusing an
// obvious id like "start" would silently suppress each other.
func TestRecordEventDeterministicIDIsScopedToTenantAndWorkflow(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	idA := recordEventDeterministicID(tenantA, "wf-1", "start")
	idB := recordEventDeterministicID(tenantB, "wf-1", "start")
	if idA == idB {
		t.Fatalf("recordEventDeterministicID gave the same id for two different tenants: %s", idA)
	}
	idC := recordEventDeterministicID(tenantA, "wf-2", "start")
	if idA == idC {
		t.Fatalf("recordEventDeterministicID gave the same id for two different workflows: %s", idA)
	}
}

// ---------------------------------------------------------------------------
// Input validation -- no database reached, so no dialect matrix is needed: a
// non-dialing *sql.DB is enough to prove appendChained is never called.
// ---------------------------------------------------------------------------

func unreachableDBPlugin(t *testing.T) *Plugin {
	t.Helper()
	// postgres never dials on Open; a query would fail to connect, which is exactly
	// what proves these tests fail BEFORE reaching the database.
	db, err := sql.Open("postgres", "postgres://unused-in-this-test/db")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &Plugin{
		db:      &engine.SQLDBAdapter{DB: db, Dialect: plugin.DialectPostgres},
		dialect: plugin.DialectPostgres,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestRecordEventRequiresATenantContext(t *testing.T) {
	p := unreachableDBPlugin(t)
	if _, err := p.recordEvent(context.Background(), RecordEventInput{EventType: "tenant.suspended"}); err == nil ||
		!strings.Contains(err.Error(), "no tenant context") {
		t.Fatalf("recordEvent with no call context: err = %v, want a \"no tenant context\" error", err)
	}
}

func TestRecordEventRequiresEventType(t *testing.T) {
	p := unreachableDBPlugin(t)
	ctx := recordEventCallCtx(uuid.New(), "wf-1", 0)
	if _, err := p.recordEvent(ctx, RecordEventInput{}); err == nil ||
		!strings.Contains(err.Error(), "event_type is required") {
		t.Fatalf("recordEvent with no event_type: err = %v, want an \"event_type is required\" error", err)
	}
}

// TestRecordEventRejectsMalformedInput exercises the real call site a
// workflow reaches -- the registered PluginFunc, which does its own JSON
// unmarshaling via plugin.RegisterTyped -- rather than p.recordEvent
// directly. recordEvent itself (cleat#2626/#2681) no longer parses JSON;
// that moved into RegisterTyped's wrapper, tested on its own merits in
// plugin/typed_test.go. This proves record_event's own registration still
// rejects malformed input end-to-end.
func TestRecordEventRejectsMalformedInput(t *testing.T) {
	p := unreachableDBPlugin(t)
	reg := newFakeFuncRegistry()
	if err := p.RegisterHostFunctions(reg); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}
	ctx := recordEventCallCtx(uuid.New(), "wf-1", 0)
	if _, err := reg.Get("record_event")(ctx, `{not json`); err == nil ||
		!strings.Contains(err.Error(), "invalid input") {
		t.Fatalf("recordEvent with malformed JSON: err = %v, want an \"invalid input\" error", err)
	}
}

// TestRecordEventRejectsNonObjectDetails covers the branch
// TestRecordEventRejectsMalformedInput cannot reach: RegisterTyped's decode
// already refuses input that is not syntactically valid JSON, so "details is
// not valid JSON" is unreachable -- what IS reachable, because
// encoding/json accepts it as a well-formed json.RawMessage, is a details
// value that parses but is not a JSON OBJECT.
func TestRecordEventRejectsNonObjectDetails(t *testing.T) {
	p := unreachableDBPlugin(t)
	ctx := recordEventCallCtx(uuid.New(), "wf-1", 0)
	for _, details := range []string{`"a string"`, `42`, `[1,2,3]`, `true`} {
		input := RecordEventInput{EventType: "x", Details: json.RawMessage(details)}
		if _, err := p.recordEvent(ctx, input); err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
			t.Errorf("recordEvent with details=%s: err = %v, want a \"must be a JSON object\" error", details, err)
		}
	}
}

// TestRecordEventAcceptsNullDetails: null is valid JSON and decodes cleanly
// into a nil map, so it is treated the same as an omitted details field
// rather than refused as "not an object".
func TestRecordEventAcceptsNullDetails(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		ctx := recordEventCallCtx(tenant, "wf-null-details", 0)
		if _, err := p.recordEvent(ctx, RecordEventInput{EventType: "x", Details: json.RawMessage(`null`)}); err != nil {
			t.Fatalf("recordEvent with details=null: %v", err)
		}
		var metadata string
		e.scan(tenant, `SELECT metadata FROM audit_events WHERE tenant_id = $1`,
			[]any{tenant.String()}, &metadata)
		if strings.TrimSpace(metadata) != "{}" {
			t.Errorf("metadata = %q, want the default empty object", metadata)
		}
	})
}

func TestRecordEventRequiresADatabase(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := recordEventCallCtx(uuid.New(), "wf-1", 0)
	if _, err := p.recordEvent(ctx, RecordEventInput{EventType: "tenant.suspended"}); err == nil ||
		!strings.Contains(err.Error(), "no database") {
		t.Fatalf("recordEvent with p.db == nil: err = %v, want a \"no database\" error", err)
	}
}

// ---------------------------------------------------------------------------
// RegisterHostFunctions
// ---------------------------------------------------------------------------

func TestRecordEvent_RegisterHostFunctions_NilRegistry(t *testing.T) {
	p := unreachableDBPlugin(t)
	if err := p.RegisterHostFunctions(nil); err == nil ||
		!strings.Contains(err.Error(), "nil function registry") {
		t.Fatalf("RegisterHostFunctions(nil): err = %v, want a nil-registry error", err)
	}
}

func TestRecordEvent_RegisterHostFunctions_Valid(t *testing.T) {
	p := unreachableDBPlugin(t)
	reg := newFakeFuncRegistry()
	if err := p.RegisterHostFunctions(reg); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}
	if !reg.Has("record_event") {
		t.Error("expected record_event to be registered")
	}
}

type fakeFuncRegistry struct {
	funcs map[string]plugin.PluginFunc
}

func newFakeFuncRegistry() *fakeFuncRegistry {
	return &fakeFuncRegistry{funcs: make(map[string]plugin.PluginFunc)}
}

func (r *fakeFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	r.funcs[opts.Name] = fn
	return nil
}

func (r *fakeFuncRegistry) Has(name string) bool {
	_, ok := r.funcs[name]
	return ok
}

func (r *fakeFuncRegistry) Get(name string) plugin.PluginFunc {
	return r.funcs[name]
}
