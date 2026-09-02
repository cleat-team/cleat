package engine

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// Replaying a durable call must leave the session in the same state as the
// fresh call that recorded it.
//
// Two bugs in one day had the identical shape: the fresh path mutated a Go-side
// collection alongside recordEvent, and the replay branch reproduced the event
// but not the collection.
//
//	3.66  DurableDefer      s.deferrals  -- a defer registered before a
//	                                        suspension was dropped for good
//	3.68  SetScope (clear)  s.heldScopes -- a replayed segment released a
//	                                        virtual-object key it had given up,
//	                                        which is another workflow's lock
//
// Neither was catchable by reading, and neither was caught by the tests that
// existed: every one of them asserted on the event, the return value, or the
// step count, which is exactly the half that replay gets right. The state is
// the half it forgets.
//
// So this asserts the invariant directly rather than adding a third point
// test. For each durable call, run it fresh, feed the history it recorded to a
// second session in replay, run the same call, and require the two sessions to
// agree on every Go-side field. 3.68 proposed it; this is it.

// sessionState is the Go-side state a durable call can mutate outside the
// event record -- everything replay has to rebuild for itself, because none of
// it is reconstructed from history automatically.
type sessionState struct {
	Deferrals     map[string]string
	QueryState    map[string]string
	StateStore    map[string]string
	QueryHandlers []string
	HeldScopes    []string
	ScopeSet      bool
	ScopePrefix   string
	ScopeObjType  string
	ScopeInstKey  string
}

func snapshotSession(s *execSession) sessionState {
	cp := func(m map[string]string) map[string]string {
		if m == nil {
			return nil
		}
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return sessionState{
		Deferrals:     cp(s.deferrals),
		QueryState:    cp(s.queryState),
		StateStore:    cp(s.stateStore),
		QueryHandlers: append([]string(nil), s.queryHandlers...),
		HeldScopes:    append([]string(nil), s.heldScopes...),
		ScopeSet:      s.scopeSet,
		ScopePrefix:   s.scopePrefix,
		ScopeObjType:  s.scopeObjType,
		ScopeInstKey:  s.scopeInstKey,
	}
}

func newParitySession(t *testing.T) *execSession {
	t.Helper()
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = &recordingKeyStore{}
	return s
}

// parityCases drive one durable call, or a short sequence of them where the
// property only appears across a pair -- acquiring a scope and then clearing
// it is the 3.68 case, and neither half alone shows it.
var parityCases = []struct {
	name string
	run  func(ctx context.Context, s *execSession)
}{
	{"DurableDefer", func(ctx context.Context, s *execSession) {
		s.DurableDefer(ctx, nil, "release the lock", 0, 0)
	}},
	{"DurableDefer twice", func(ctx context.Context, s *execSession) {
		s.DurableDefer(ctx, nil, "first", 0, 0)
		s.DurableDefer(ctx, nil, "second", 0, 0)
	}},
	{"SetScope acquire", func(ctx context.Context, s *execSession) {
		s.SetScope(ctx, nil, "cart", "c1", 0, 0)
	}},
	{"SetScope acquire then clear", func(ctx context.Context, s *execSession) {
		s.SetScope(ctx, nil, "cart", "c1", 0, 0)
		s.SetScope(ctx, nil, "", "", 0, 0)
	}},
	{"SetScope switch objects", func(ctx context.Context, s *execSession) {
		s.SetScope(ctx, nil, "cart", "c1", 0, 0)
		s.SetScope(ctx, nil, "order", "o9", 0, 0)
	}},
	{"SetState", func(ctx context.Context, s *execSession) {
		s.SetState(ctx, nil, "status", "shipped")
	}},
	{"SetState then DeleteState", func(ctx context.Context, s *execSession) {
		s.SetState(ctx, nil, "status", "shipped")
		s.DeleteState(ctx, nil, "status")
	}},
	{"IncrState", func(ctx context.Context, s *execSession) {
		s.IncrState(ctx, nil, "attempts", 3)
	}},
	{"IncrState twice", func(ctx context.Context, s *execSession) {
		s.IncrState(ctx, nil, "attempts", 3)
		s.IncrState(ctx, nil, "attempts", 4)
	}},
}

// TestReplayReproducesFreshSessionState is the property.
//
// The vacuous-pass guard is the important part of the harness. If the replayed
// session diverges -- exits replay and re-runs the fresh path -- the two
// states match trivially and the case proves nothing. So each case must
// consume every recorded event and still be in replay at the end; a case that
// cannot is a finding, not a case to relax.
func TestReplayReproducesFreshSessionState(t *testing.T) {
	ctx := context.Background()

	for _, tc := range parityCases {
		t.Run(tc.name, func(t *testing.T) {
			fresh := newParitySession(t)
			tc.run(ctx, fresh)
			want := snapshotSession(fresh)
			recorded := append([]EventRecord(nil), fresh.history...)

			if len(recorded) == 0 {
				t.Fatalf("the fresh run recorded no events, so replay has nothing to " +
					"consume and this case cannot distinguish the two paths")
			}

			replayed := newParitySession(t)
			replayed.history = recorded
			replayed.isReplay = true
			tc.run(ctx, replayed)

			if !replayed.isReplay {
				t.Fatalf("the replayed session left replay after consuming %d of %d "+
					"events.\n\nIt then re-ran the fresh path, so the two states match "+
					"for the wrong reason and this case is vacuous. Either the call "+
					"diverges on identical input -- which is itself the bug -- or the "+
					"case needs a history the replay branch actually matches.",
					replayed.stepCount, len(recorded))
			}
			if replayed.stepCount != len(recorded) {
				t.Fatalf("replay consumed %d of %d recorded events; the call under test "+
					"did not reach the same steps twice", replayed.stepCount, len(recorded))
			}

			got := snapshotSession(replayed)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("replay did not reproduce the session state the fresh call built.\n"+
					"  fresh:  %s\n  replay: %s\n\n"+
					"Replaying an event must rebuild every Go-side field the fresh path "+
					"set, not just the event. This is the shape of IMPROVEMENT-PLAN 3.66 "+
					"(deferrals) and 3.68 (heldScopes).",
					describeState(want), describeState(got))
			}
		})
	}
}

func describeState(s sessionState) string {
	return fmt.Sprintf("deferrals=%v queryState=%v stateStore=%v queryHandlers=%v "+
		"heldScopes=%v scopeSet=%v scopePrefix=%q",
		s.Deferrals, s.QueryState, s.StateStore, s.QueryHandlers,
		s.HeldScopes, s.ScopeSet, s.ScopePrefix)
}

// TestParityHarnessCanFail is the control for the control.
//
// The property above passes today, which is the same thing a harness that
// compares nothing would do. This drives a deliberately asymmetric mutation
// through the identical machinery -- state present after the fresh run,
// absent after the replayed one -- and requires the comparison to catch it.
//
// Without this, every future reader has to take on faith that
// TestReplayReproducesFreshSessionState is capable of going red.
func TestParityHarnessCanFail(t *testing.T) {
	ctx := context.Background()

	fresh := newParitySession(t)
	fresh.DurableDefer(ctx, nil, "cleanup", 0, 0)
	want := snapshotSession(fresh)

	replayed := newParitySession(t)
	replayed.history = append([]EventRecord(nil), fresh.history...)
	replayed.isReplay = true
	replayed.DurableDefer(ctx, nil, "cleanup", 0, 0)
	// Reintroduce 3.66 by hand: the event was consumed, the map was not built.
	replayed.deferrals = map[string]string{}
	got := snapshotSession(replayed)

	if reflect.DeepEqual(want, got) {
		t.Fatal("the parity comparison reports a session that dropped a registered " +
			"defer as identical to one that kept it.\n\n" +
			"That is IMPROVEMENT-PLAN 3.66 exactly, so the property test above cannot " +
			"be relied on to catch its own motivating bug.")
	}
}
