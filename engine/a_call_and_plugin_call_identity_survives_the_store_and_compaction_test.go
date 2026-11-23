package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// cleat#4424. The comparison itself is two strings, and a unit test over an in-memory session proves only that. What can make "the same body" come back DIFFERENT is the layer between
// the guest and the replay: a store that normalises a column (a jsonb would reorder keys and rewrite whitespace; these are text, longtext and nvarchar(max)), a payload cipher, and
// compaction, which rebuilds the history from a trimmed copy. Each is exercised here with a body chosen to expose a normaliser: irregular whitespace, keys out of order, non-ASCII, a
// trailing newline, and enough bytes to cross any inline size threshold. A comparison that diverged on any of them would fail every compacted, encrypted or stored run on its next replay.

func awkwardBody(tag string) string {
	return "{ \"z\" : 1,\n  \"a\": [3, 2,1],\"ü\":\"日本語\", \"n\":null , \"pad\":\"" + strings.Repeat("x", 100_000) + "\", \"tag\":\"" + tag + "\" }\n"
}

// replayBoth replays history's step 0 as a call with callBody and step 1 as a plugin_call with pluginInput, in ONE session, and says whether either diverged.
func replayBoth(opts []EngineOption, history []EventRecord, callBody, pluginInput string) (callMsg, pluginMsg string, s *execSession) {
	s = stepSession(opts, history)
	buf := make([]byte, idBufLen)
	read := func() string {
		n := 0
		for n < len(buf) && buf[n] != 0 {
			n++
		}
		return string(buf[:n])
	}
	ctx := context.WithValue(context.Background(), wasmMemBufKey{}, buf)
	s.DurableCall(ctx, nil, "svc", "op", callBody, 0, idBufLen)
	callMsg = read()
	for i := range buf {
		buf[i] = 0
	}
	s.PluginCall(ctx, nil, "p", "plain", pluginInput, 0, idBufLen)
	pluginMsg = read()
	return callMsg, pluginMsg, s
}

func recordedPair() (callBody, pluginInput string, history []EventRecord) {
	callBody, pluginInput = awkwardBody("call"), awkwardBody("plugin")
	return callBody, pluginInput, []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: callBody, Response: `"call answer"`},
		{Step: 1, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "plain", PluginInput: pluginInput, PluginOutput: `"plugin answer"`},
	}
}

func assertReplaysTheRecordedPair(t *testing.T, opts []EngineOption, hist []EventRecord, callBody, pluginInput string) {
	t.Helper()
	// CONTROL: the same bodies replay, both steps, with the recorded answers.
	cm, pm, s := replayBoth(opts, hist, callBody, pluginInput)
	if isDivergence(cm) || isDivergence(pm) || cm != `"call answer"` || pm != `"plugin answer"` || s.stepCount != 2 {
		t.Fatalf("the SAME bodies did not replay (a comparison that diverges on everything): call=%q plugin=%q stepCount=%d", trimMsg(cm), trimMsg(pm), s.stepCount)
	}
	// A different call body diverges at step 0 and consumes nothing.
	cm, _, s = replayBoth(opts, hist, callBody+" ", pluginInput)
	if !isDivergence(cm) || s.stepCount != 0 {
		t.Errorf("a call body differing by one trailing space was served the recorded answer: %q (stepCount %d)", trimMsg(cm), s.stepCount)
	}
	// The same call, then a different plugin input: step 0 replays, step 1 diverges.
	cm, pm, s = replayBoth(opts, hist, callBody, pluginInput+" ")
	if isDivergence(cm) || !isDivergence(pm) || s.stepCount != 1 {
		t.Errorf("a plugin input differing by one trailing space: call=%q plugin=%q stepCount=%d, want the call to replay and the plugin_call to diverge at step 1", trimMsg(cm), trimMsg(pm), s.stepCount)
	}
	// And neither body is ever printed (100 KB of padding would show).
	if strings.Contains(pm, "xxxxxxxxxx") || strings.Contains(cm, "xxxxxxxxxx") {
		t.Errorf("a divergence message prints part of a body")
	}
}

func trimMsg(s string) string {
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}

// A call and a plugin_call recorded THROUGH A REAL STORE replay on the same body and diverge on a different one, on every dialect, with payload encryption off and on.
func TestACallAndAPluginCallPersistedInAStoreReplayOnTheSameRequestOnEveryDialect(t *testing.T) {
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
				callBody, pluginInput, written := recordedPair()
				for _, rec := range written {
					if err := store.AppendEventHistory(ctx, wfID, rec); err != nil {
						t.Fatalf("AppendEventHistory step %d: %v", rec.Step, err)
					}
				}
				if encrypted {
					// CONTROL, read from the column with no decryption in the path: LoadEventHistory treats a value it cannot decrypt as never encrypted, so an "encrypted" row
					// that was in fact stored in the clear would pass everything below. The raw request must NOT be the body, and must not show the padding.
					var raw string
					if err := store.(*PostgresStore).db.QueryRowContext(ctx, `SELECT COALESCE(request, '') FROM event_history WHERE workflow_id = $1 AND step = 0`, wfID).Scan(&raw); err != nil {
						t.Fatalf("read the raw request column: %v", err)
					}
					if raw == "" || raw == callBody || strings.Contains(raw, "xxxxxxxxxx") {
						t.Fatalf("PRECONDITION FAILED: the stored request is not ciphertext (%d bytes, equal to the body: %v), so the encrypted row measures the plaintext path", len(raw), raw == callBody)
					}
				}
				loaded, err := store.LoadEventHistory(ctx, wfID)
				if err != nil || len(loaded) != 2 {
					t.Fatalf("LoadEventHistory = %d records, err %v; want 2", len(loaded), err)
				}
				// PRECONDITION: the store handed the bodies back byte for byte. If it did not, the comparison below would be measuring the store, and this says so first.
				if loaded[0].Request != callBody || loaded[1].PluginInput != pluginInput {
					t.Fatalf("PRECONDITION FAILED: the %s store did not return the bodies unchanged (call request %d bytes want %d, plugin input %d bytes want %d)",
						name, len(loaded[0].Request), len(callBody), len(loaded[1].PluginInput), len(pluginInput))
				}
				assertReplaysTheRecordedPair(t, opts, loaded, callBody, pluginInput)
			})
		}
	}
}

// The same history after a compaction round trip: the compacted copy must still carry the request and the input, or every compacted run would diverge on its next replay.
func TestACallAndAPluginCallReplayOnTheSameRequestAfterCompaction(t *testing.T) {
	opts := idPluginRegistry(t)
	callBody, pluginInput, hist := recordedPair()
	cs, err := extractCompactionStateFrom(nil, hist)
	if err != nil {
		t.Fatalf("extractCompactionStateFrom: %v", err)
	}
	rebuilt := buildFullHistoryFromCompaction(nil, cs)
	if len(rebuilt) != 2 {
		t.Fatalf("PRECONDITION FAILED: compaction rebuilt %d records, want 2", len(rebuilt))
	}
	if rebuilt[0].Request != callBody || rebuilt[1].PluginInput != pluginInput {
		t.Fatalf("compaction did not carry the request (%d bytes, want %d) or the plugin input (%d bytes, want %d)", len(rebuilt[0].Request), len(callBody), len(rebuilt[1].PluginInput), len(pluginInput))
	}
	assertReplaysTheRecordedPair(t, opts, rebuilt, callBody, pluginInput)
}
