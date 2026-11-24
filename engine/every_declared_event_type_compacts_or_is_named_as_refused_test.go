package engine

// cleat#4316. call_attempt_failed had a compaction CODE and no capture ARM, so extractCompactionStateFrom refused any history that held one (every call that retried),
// the run was never compacted, and it grew without bound. Nothing noticed: TestEveryDeclaredEventTypeHasACode compares codeToEventType with eventTypeToCode (it takes its types
// "from the map's own inverse"), and a code with no arm agrees with both maps.
//
// This is the guard for the whole class. The population is every EventType constant DECLARED in types.go, read with go/parser and not from either map, so a new event type
// has to be added to nothing for this test to see it. Each is either COMPACTABLE (extract accepts it, and what comes back is the same type) or NAMED here as refused with a
// reason, and a name must actually refuse. A type that is neither fails with its name, which is the moment the next new event type would otherwise poison compaction quietly.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"
	"time"
)

// compactionRefusedEventTypes names the event types compaction DELIBERATELY refuses, each with why. It is empty: every declared type compacts. An entry here is a decision, and
// the test below fails if a named type compacts after all (delete the entry) or a refused one is not named.
var compactionRefusedEventTypes = map[EventType]string{}

// declaredEventTypes reads every `EventType`-typed string constant from types.go. A parse and not a grep: the declarations are in one const block with comments between them,
// and a regex over that text cannot tell a declaration from a sentence that quotes one.
func declaredEventTypes(t *testing.T) []EventType {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "types.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse types.go: %v", err)
	}
	var out []EventType
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != "EventType" {
				continue
			}
			for _, v := range vs.Values {
				if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", lit.Value, err)
					}
					out = append(out, EventType(s))
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func TestEveryDeclaredEventTypeCompactsOrIsNamedAsRefused(t *testing.T) {
	types := declaredEventTypes(t)
	// PRECONDITION, as a predicate and not a floor: the scan must see types it is known to see, or it is not reading the declarations.
	seen := map[EventType]bool{}
	for _, et := range types {
		seen[et] = true
	}
	for _, must := range []EventType{EventTypeCall, EventTypeCallAttemptFailed, EventTypeAwaitAnyChild} {
		if !seen[must] {
			t.Fatalf("PRECONDITION FAILED: the scan of types.go did not find %q, so it is not reading the EventType declarations", must)
		}
	}
	for et, why := range compactionRefusedEventTypes {
		if why == "" {
			t.Errorf("%s is named as refused with no reason: a refusal is a decision and says why", et)
		}
		if !seen[et] {
			t.Errorf("%s is named as refused but is not a declared EventType", et)
		}
	}
	for _, et := range types {
		t.Run(string(et), func(t *testing.T) {
			cs, err := extractCompactionStateFrom(nil, []EventRecord{{Step: 0, EventType: et}})
			if why, refused := compactionRefusedEventTypes[et]; refused {
				if err == nil {
					t.Errorf("%s is named as refused (%s) but compaction accepts it: delete the entry", et, why)
				}
				return
			}
			if _, coded := eventTypeToCode[et]; !coded {
				t.Fatalf("%s is declared but has no compaction code and is not named in compactionRefusedEventTypes: add the capture and expansion arms, or name it with a reason", et)
			}
			if err != nil {
				t.Fatalf("%s has a code but compaction refuses a history that holds one (%v): a run with such an event can never be compacted and grows without bound (cleat#4316). Add the capture and expansion arms, or name it in compactionRefusedEventTypes with a reason", et, err)
			}
			out := buildFullHistoryFromCompaction(nil, cs)
			if len(out) != 1 || out[0].EventType != et {
				t.Errorf("%s came back as %+v: the expansion does not restore the type", et, out)
			}
		})
	}
}

// The proof row for cleat#4316 through the real store: a history holding a failed attempt compacts, the state that comes back carries the attempt, and the tail is what is left.
func TestAHistoryWithAFailedAttemptCompactsOnEveryStore(t *testing.T) {
	ctx := context.Background()
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			id := fmt.Sprintf("failed-attempt-%d", time.Now().UnixNano())
			if _, _, err := store.StartNewRun(ctx, id, "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatal(err)
			}
			evs := []EventRecord{
				{EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{}`},
				{EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{}`},
				{EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{}`},
				{EventType: EventTypeCallAttemptFailed, Service: "svc", Op: "flaky", Attempt: 2, Err: "boom"},
				{EventType: EventTypeCall, Service: "svc", Op: "flaky", Request: `{}`, Response: `{}`},
				{EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{}`},
				{EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{}`},
				{EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{}`},
			}
			for i, ev := range evs {
				ev.Step = i
				if err := store.AppendEventHistory(ctx, id, ev); err != nil {
					t.Fatal(err)
				}
			}
			if err := CompactWorkflowHistory(ctx, store, id, 6, nil); err != nil {
				t.Fatalf("a history holding a call_attempt_failed event refused to compact: %v", err)
			}
			cs, err := store.LoadCompactionState(ctx, id)
			if err != nil || cs == nil {
				t.Fatalf("no compaction state written (state=%v, err=%v)", cs, err)
			}
			tail, err := store.LoadEventHistory(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(tail) == 0 || len(tail) >= len(evs) {
				t.Errorf("PRECONDITION: compaction left a tail of %d of %d events; it did not compact", len(tail), len(evs))
			}
			full := buildFullHistoryFromCompaction(tail, cs)
			if len(full) != len(evs) {
				t.Fatalf("expanded history has %d events, want %d", len(full), len(evs))
			}
			got := full[3]
			if got.EventType != EventTypeCallAttemptFailed || got.Service != "svc" || got.Op != "flaky" || got.Attempt != 2 || got.Err != "boom" {
				t.Errorf("the failed attempt did not survive compaction and the store: %+v", got)
			}
		})
	}
}
