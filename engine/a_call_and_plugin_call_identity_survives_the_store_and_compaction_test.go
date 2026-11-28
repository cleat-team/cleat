package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// cleat#4424. The comparison itself is two strings, and a unit test over an in-memory session proves only that. What can make "the same body" come back DIFFERENT is the layer between
// the guest and the replay: a store that normalises a column (a jsonb would reorder keys and rewrite whitespace; these are text, longtext and nvarchar(max)), a payload cipher, and
// compaction, which rebuilds the history from a trimmed copy. Each is exercised here with a body chosen to expose a normaliser: irregular whitespace, keys out of order, non-ASCII, a
// trailing newline, and enough bytes to cross any inline size threshold. A comparison that diverged on any of them would fail every compacted, encrypted or stored run on its next replay.
//
// The history holds ALL SIX call kinds that compare a request (call, plugin_call, call_with_retry, call_with_heartbeat, durable_send, fetch), one after the other, so that a run is replayed
// in order as a workflow would replay it, and "diverges at the right step" means something: the calls before it must have replayed and consumed exactly their own records.

func awkwardBody(tag string) string {
	return "{ \"z\" : 1,\n  \"a\": [3, 2,1],\"ü\":\"日本語\", \"n\":null , \"pad\":\"" + strings.Repeat("x", 100_000) + "\", \"tag\":\"" + tag + "\" }\n"
}

type seqKind struct {
	name    string
	records func(body string) []EventRecord // the records one call of this kind leaves, steps not yet numbered
	call    func(ctx context.Context, s *execSession, body string) int64
}

var seqKinds = []seqKind{
	{"call", func(b string) []EventRecord {
		return []EventRecord{{EventType: EventTypeCall, Service: "svc", Op: "op", Request: b, Response: `"a"`}}
	}, func(ctx context.Context, s *execSession, b string) int64 {
		return s.DurableCall(ctx, nil, "svc", "op", b, 0, idBufLen)
	}},
	{"plugin_call", func(b string) []EventRecord {
		return []EventRecord{{EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "plain", PluginInput: b, PluginOutput: `"a"`}}
	}, func(ctx context.Context, s *execSession, b string) int64 {
		return s.PluginCall(ctx, nil, "p", "plain", b, 0, idBufLen)
	}},
	{"call_with_retry", func(b string) []EventRecord {
		return []EventRecord{
			{EventType: EventTypeCallAttemptFailed, Service: "svc", Op: "op2", Attempt: 1, Err: "TRY_AGAIN: 503"},
			{EventType: EventTypeCall, Service: "svc", Op: "op2", Request: b, Response: `"a"`},
		}
	}, func(ctx context.Context, s *execSession, b string) int64 {
		return s.DurableCallWithRetry(ctx, nil, "svc", "op2", b, 3, 1, 100, 1, "", 0, idBufLen)
	}},
	{"call_with_heartbeat", func(b string) []EventRecord {
		return []EventRecord{
			{EventType: EventTypeHeartbeat, Service: "svc", Op: "op3"},
			{EventType: EventTypeCall, Service: "svc", Op: "op3", Request: b, Response: `"a"`},
		}
	}, func(ctx context.Context, s *execSession, b string) int64 {
		return s.DurableCallWithHeartbeat(ctx, nil, "svc", "op3", b, 1000, 0, idBufLen)
	}},
	{"durable_send", func(b string) []EventRecord {
		return []EventRecord{{EventType: EventTypeDurableSend, Service: "svc", Op: "op4", Request: b}}
	}, func(ctx context.Context, s *execSession, b string) int64 {
		return s.DurableSend(ctx, nil, "svc", "op4", b)
	}},
	// fetch compared its body byte for byte until cleat#4424 and now compares it as a call's request is; it is stored and compacted through other fields (fetch_body, ChildInput).
	{"fetch", func(b string) []EventRecord {
		return []EventRecord{{EventType: EventTypeFetch, FetchMethod: "POST", FetchURL: "http://example.invalid/x", FetchBody: b, FetchResponse: `"a"`}}
	}, func(ctx context.Context, s *execSession, b string) int64 {
		return s.Fetch(ctx, nil, "POST", "http://example.invalid/x", "{}", b, 0, idBufLen)
	}},
}

// seqBodies is one distinct awkward body per kind.
func seqBodies() []string {
	b := make([]string, len(seqKinds))
	for i, k := range seqKinds {
		b[i] = awkwardBody(k.name)
	}
	return b
}

// seqHistory is the records the five calls leave, in order, with their steps numbered.
func seqHistory(bodies []string) []EventRecord {
	var hist []EventRecord
	for i, k := range seqKinds {
		for _, r := range k.records(bodies[i]) {
			r.Step = len(hist)
			hist = append(hist, r)
		}
	}
	return hist
}

// replaySeq replays the five calls in order against hist with the given bodies and stops at the first one that did not consume its records. It returns that call's index, or -1.
func replaySeq(opts []EngineOption, hist []EventRecord, bodies []string) (int, *execSession) {
	s := stepSession(opts, hist)
	buf := make([]byte, idBufLen)
	ctx := context.WithValue(context.Background(), wasmMemBufKey{}, buf)
	for i, k := range seqKinds {
		before := s.stepCount
		k.call(ctx, s, bodies[i])
		if s.stepCount == before {
			return i, s
		}
	}
	return -1, s
}

func assertReplaysTheRecordedSequence(t *testing.T, opts []EngineOption, hist []EventRecord, bodies []string) {
	t.Helper()
	// CONTROL: the same bodies replay, every call, consuming every record.
	if at, s := replaySeq(opts, hist, bodies); at != -1 || s.stepCount != len(hist) {
		t.Fatalf("the SAME bodies did not replay (a comparison that diverges on everything): stopped at %d (%s) with stepCount %d of %d", at, nameOf(at), s.stepCount, len(hist))
	}
	// The SAME requests written another way (keys sorted, no whitespace: another SDK's serialiser) replay too, call after call. This is the cross-language guarantee, ABI.md §6.7.
	other := make([]string, len(bodies))
	for i := range bodies {
		other[i] = reserialised(t, bodies[i])
	}
	if at, s := replaySeq(opts, hist, other); at != -1 || s.stepCount != len(hist) {
		t.Errorf("the same requests re-serialised did not replay: stopped at %d (%s) with stepCount %d of %d", at, nameOf(at), s.stepCount, len(hist))
	}
	// Each kind in turn: a body with ONE VALUE changed diverges AT THAT CALL, having consumed exactly the records of the calls before it.
	for i, k := range seqKinds {
		changed := append([]string(nil), bodies...)
		changed[i] = strings.Replace(bodies[i], `"tag":"`, `"tag":"X`, 1)
		if changed[i] == bodies[i] {
			t.Fatalf("PRECONDITION FAILED: the changed body for %s is unchanged", k.name)
		}
		want := 0
		for j := 0; j < i; j++ {
			want += len(seqKinds[j].records(bodies[j]))
		}
		at, s := replaySeq(opts, hist, changed)
		if at != i || s.stepCount != want {
			t.Errorf("%s: a body with one value changed: diverged at call %d (%s) with stepCount %d, want call %d at stepCount %d", k.name, at, nameOf(at), s.stepCount, i, want)
		}
	}
}

// reserialised is the same JSON value written another way: Go's compact form with every key sorted, which is what another SDK's serialiser may produce for the same request
// (ABI.md §6.7). It must come out as different BYTES, or the row that uses it measures nothing.
func reserialised(t *testing.T, body string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("PRECONDITION FAILED: the test body is not JSON: %v", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: cannot re-serialise the test body: %v", err)
	}
	if string(out) == body {
		t.Fatalf("PRECONDITION FAILED: re-serialising changed no byte, so the row would measure nothing")
	}
	return string(out)
}

func nameOf(i int) string {
	if i < 0 || i >= len(seqKinds) {
		return "none"
	}
	return seqKinds[i].name
}

// All five kinds recorded THROUGH A REAL STORE replay on the same bodies and diverge on a different one, on every dialect, with payload encryption off and on.
func TestEveryRequestCarryingCallPersistedInAStoreReplaysOnTheSameRequestOnEveryDialect(t *testing.T) {
	opts := idPluginRegistry(t)
	for _, backend := range registeredBackends {
		backend := backend
		// Payload encryption at rest is PostgreSQL's alone (WithEncryption panics on MySQL, citing cleat#3906), so the encrypted variant exists only there: generating it for
		// the other dialects would be a row that cannot run, and a skip in its place would read as measured.
		variants := []bool{false}
		if backend.Name() == "postgres" {
			variants = append(variants, true)
		}
		for _, encrypted := range variants {
			encrypted := encrypted
			name := backend.Name() + "/plaintext"
			if encrypted {
				name = backend.Name() + "/encrypted"
			}
			t.Run(name, func(t *testing.T) {
				store, teardown := backend.Setup(t)
				defer teardown()
				if encrypted {
					enc, err := NewPayloadEncryption(validKey(t))
					if err != nil {
						t.Fatalf("NewPayloadEncryption: %v", err)
					}
					st, ok := store.(*PostgresStore)
					if !ok {
						t.Fatalf("PRECONDITION FAILED: %T is not a *PostgresStore, so the encrypted row would measure the plaintext path", store)
					}
					st.WithEncryption(enc, true)
				}
				ctx := context.Background()
				wfID := newIntentWorkflow(t, ctx, store, fmt.Sprintf("identity-%s", strings.ReplaceAll(name, "/", "-")))
				bodies := seqBodies()
				written := seqHistory(bodies)
				for _, rec := range written {
					if err := store.AppendEventHistory(ctx, wfID, rec); err != nil {
						t.Fatalf("AppendEventHistory step %d (%s): %v", rec.Step, rec.EventType, err)
					}
				}
				if encrypted {
					// CONTROL, read from the column with no decryption in the path: LoadEventHistory treats a value it cannot decrypt as never encrypted, so an "encrypted" row
					// that was in fact stored in the clear would pass everything below. The raw request must NOT be the body, and must not show the padding.
					var raw string
					if err := store.(*PostgresStore).db.QueryRowContext(ctx, `SELECT COALESCE(request, '') FROM event_history WHERE workflow_id = $1 AND step = 0`, wfID).Scan(&raw); err != nil {
						t.Fatalf("read the raw request column: %v", err)
					}
					if raw == "" || raw == bodies[0] || strings.Contains(raw, "xxxxxxxxxx") {
						t.Fatalf("PRECONDITION FAILED: the stored request is not ciphertext (%d bytes, equal to the body: %v), so the encrypted row measures the plaintext path", len(raw), raw == bodies[0])
					}
				}
				loaded, err := store.LoadEventHistory(ctx, wfID)
				if err != nil || len(loaded) != len(written) {
					t.Fatalf("LoadEventHistory = %d records, err %v; want %d", len(loaded), err, len(written))
				}
				// PRECONDITION: the store handed every body back byte for byte. If it did not, the comparison below would be measuring the store, and this says so first.
				for i := range written {
					if loaded[i].Request != written[i].Request || loaded[i].PluginInput != written[i].PluginInput || loaded[i].FetchBody != written[i].FetchBody {
						t.Fatalf("PRECONDITION FAILED: the %s store did not return step %d (%s) unchanged (request %d bytes want %d, plugin input %d bytes want %d)",
							name, i, written[i].EventType, len(loaded[i].Request), len(written[i].Request), len(loaded[i].PluginInput), len(written[i].PluginInput))
					}
				}
				assertReplaysTheRecordedSequence(t, opts, loaded, bodies)
			})
		}
	}
}

// The same history after a compaction round trip: the compacted copy must still carry every request and input, or every compacted run would diverge on its next replay.
func TestEveryRequestCarryingCallReplaysOnTheSameRequestAfterCompaction(t *testing.T) {
	opts := idPluginRegistry(t)
	bodies := seqBodies()
	hist := seqHistory(bodies)
	cs, err := extractCompactionStateFrom(nil, hist)
	if err != nil {
		t.Fatalf("extractCompactionStateFrom: %v", err)
	}
	rebuilt := buildFullHistoryFromCompaction(nil, cs)
	if len(rebuilt) != len(hist) {
		t.Fatalf("PRECONDITION FAILED: compaction rebuilt %d records, want %d", len(rebuilt), len(hist))
	}
	for i := range hist {
		// A record that carries no request (an attempt, a heartbeat) must stay without one; the others must carry it whole.
		if rebuilt[i].Request != hist[i].Request || rebuilt[i].PluginInput != hist[i].PluginInput || rebuilt[i].FetchBody != hist[i].FetchBody {
			t.Fatalf("compaction changed step %d (%s): request %d bytes want %d, plugin input %d bytes want %d",
				i, hist[i].EventType, len(rebuilt[i].Request), len(hist[i].Request), len(rebuilt[i].PluginInput), len(hist[i].PluginInput))
		}
	}
	assertReplaysTheRecordedSequence(t, opts, rebuilt, bodies)
}
