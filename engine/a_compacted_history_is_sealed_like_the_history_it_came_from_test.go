package engine

// Compaction must not turn sealed history back into plaintext (cleat#3774).
//
// With --encrypt-sensitive-payloads on, event_history seals the ten string
// columns and payload (encodeEventForStorage). CompactWorkflowHistory loads the
// history through the store, which opens it, copies the sensitive fields of the
// events it is compacting into a CompactionState, marshals that and hands the
// JSON to CompactHistory -- which used to write it to
// workflow_payloads.compaction_state exactly as it was. So the same data sat
// sealed in one table and in plaintext in another, and the plaintext half is
// what a database reader or a backup holds.
//
// WHAT THESE TESTS ASK. Not "is the column sealed", which is the question the
// fix answers, but "does the sensitive text appear ANYWHERE in the row of
// workflow_payloads, in any form a copy of it could take" -- raw, JSON-escaped
// and as the content of a base64 token -- so a future column added to
// compaction, or a second copy made some other way, fails here and not in the
// field. The search is itself checked on known-positive inputs
// (TestTheLeakSearchSeesEachFormItIsMeantToSee), and against a store with
// encryption OFF, where the markers MUST be found.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// compactionLeakFields names every sensitive field CompactionState copies out of
// an event, each with a marker that carries characters JSON escapes (a quote, an
// ampersand, angle brackets), so the JSON-escaped form differs from the raw one.
type compactionLeakField struct {
	name   string
	marker string
}

func compactionLeakMarker(field string) string {
	return fmt.Sprintf(`cleat3774-%s-"q"&<x>-%s`, field, markerFor("c"))
}

// compactionSeed returns events that between them carry every sensitive field
// that extractCompactionState copies (Request, Response, Err, SignalPayload,
// ChildInput, NewInput, PluginInput, PluginOutput, PromiseResult, PromiseError)
// followed by plain call events, and the markers by field.
func compactionSeed() ([]EventRecord, []compactionLeakField) {
	f := func(name string) string { return compactionLeakMarker(name) }
	fields := []compactionLeakField{
		{"request", f("request")}, {"response", f("response")}, {"err", f("err")},
		{"signal_payload", f("signal_payload")}, {"child_input", f("child_input")},
		{"new_input", f("new_input")}, {"plugin_input", f("plugin_input")},
		{"plugin_output", f("plugin_output")}, {"promise_result", f("promise_result")},
		{"promise_error", f("promise_error")},
	}
	m := map[string]string{}
	for _, x := range fields {
		m[x.name] = x.marker
	}
	evs := []EventRecord{
		{EventType: EventTypeCall, Service: "svc", Op: "op", Request: m["request"], Response: m["response"], Err: m["err"]},
		{EventType: EventTypeSignalReceived, SignalName: "go", SignalPayload: m["signal_payload"]},
		{EventType: EventTypeChildWorkflow, ChildName: "child", ChildInput: m["child_input"], RunID: "child-run-1"},
		{EventType: EventTypeContinueAsNew, NewInput: m["new_input"]},
		{EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "f", PluginInput: m["plugin_input"], PluginOutput: m["plugin_output"]},
		{EventType: EventTypePromiseResolved, PromiseID: "pr-1", PromiseResult: m["promise_result"]},
		{EventType: EventTypePromiseRejected, PromiseID: "pr-2", PromiseError: m["promise_error"]},
	}
	for i := 0; i < 5; i++ {
		evs = append(evs, EventRecord{EventType: EventTypeCall, Service: "svc", Op: "filler"})
	}
	for i := range evs {
		evs[i].Step = i
	}
	return evs, fields
}

// leakForms returns the ways text can show up in a stored row, as the strings to
// look for. base64 is handled separately by leaksInBase64Tokens, because where a
// marker falls inside a base64 token depends on its byte offset, so there is no
// single encoded needle.
func leakForms(marker string) map[string]string {
	goJSON, _ := json.Marshal(marker) // escapes \", &, <, >
	return map[string]string{
		"raw":              marker,
		"JSON-escaped":     strings.Trim(string(goJSON), `"`),
		"jsonb-text":       strings.ReplaceAll(marker, `"`, `\"`), // what PostgreSQL's ::text renders
		"base64 of marker": base64.StdEncoding.EncodeToString([]byte(marker)),
	}
}

var base64Token = regexp.MustCompile(`[A-Za-z0-9+/_-]{16,}={0,2}`)

// leaksInBase64Tokens decodes every base64-looking token in text and reports
// whether any decoded token contains marker.
func leaksInBase64Tokens(text, marker string) bool {
	for _, tok := range base64Token.FindAllString(text, -1) {
		for _, enc := range []*base64.Encoding{
			base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding,
		} {
			if b, err := enc.DecodeString(tok); err == nil && strings.Contains(string(b), marker) {
				return true
			}
		}
	}
	return false
}

// findLeaks returns the forms in which marker appears in text.
func findLeaks(text, marker string) []string {
	var found []string
	for name, needle := range leakForms(marker) {
		if strings.Contains(text, needle) {
			found = append(found, name)
		}
	}
	if leaksInBase64Tokens(text, marker) {
		found = append(found, "inside a base64 token")
	}
	return found
}

// TestTheLeakSearchSeesEachFormItIsMeantToSee is the search's own known-positive:
// a search that cannot find a planted copy proves nothing about its absence.
func TestTheLeakSearchSeesEachFormItIsMeantToSee(t *testing.T) {
	marker := compactionLeakMarker("self")
	goJSON, _ := json.Marshal(map[string]string{"resp": marker})
	pgText := `{"resp": "` + strings.ReplaceAll(marker, `"`, `\"`) + `"}`
	// A base64 token whose offset inside the encoded text is not a multiple of
	// three, so base64 of the marker alone is NOT a substring of it.
	shifted := base64.StdEncoding.EncodeToString([]byte("xy" + marker + "zz"))

	cases := []struct{ name, text string }{
		{"raw", "prefix " + marker + " suffix"},
		{"Go JSON", string(goJSON)},
		{"PostgreSQL jsonb text", pgText},
		{"base64 at a shifted offset", `"` + shifted + `"`},
	}
	for _, c := range cases {
		if len(findLeaks(c.text, marker)) == 0 {
			t.Errorf("the search did not find a marker planted as %s in %q", c.name, c.text)
		}
	}
	if got := findLeaks(`{"resp":"AAAAAAAAAAAAAAAAAAAAAAAA"}`, marker); len(got) != 0 {
		t.Errorf("the search reported %v in a row that holds no marker", got)
	}
}

// rowText returns every column of the run's workflow_payloads row as one string,
// read on a connection that never decrypts, so a copy of the text in ANY column
// is in what is searched.
func compactionRowText(t *testing.T, store *PostgresStore, id string) string {
	t.Helper()
	var raw string
	if err := store.db.QueryRow(`SELECT to_jsonb(p)::text FROM workflow_payloads p WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read workflow_payloads row: %v", err)
	}
	return raw
}

// seedAndCompact starts a run, writes compactionSeed's events through the real
// store, and compacts at threshold 4. It returns the run id, the fields, the
// events as the store reports them before compaction, and the keep step.
func seedAndCompact(t *testing.T, store *PostgresStore, tag string) (string, []compactionLeakField, []EventRecord) {
	t.Helper()
	ctx := context.Background()
	worker := "compaction-seal-" + tag
	id := startPayloadReaderRun(t, store, "compaction-seal-"+tag)
	wf, err := store.ClaimWorkflow(ctx, worker)
	if err != nil || wf == nil || wf.ID != id {
		t.Fatalf("ClaimWorkflow = %v, %v; want %s", wf, err, id)
	}
	eng := NewEngine(nil, nil, WithDB(store.db), WithWorkflowStore(store), WithWorkerID(worker),
		WithGeneration(wf.Generation), WithTenantID(DefaultTenantUUID))

	events, fields := compactionSeed()
	for _, rec := range events {
		if err := eng.flushEvent(ctx, id, rec, ""); err != nil {
			t.Fatalf("seed event %d (%s): %v", rec.Step, rec.EventType, err)
		}
	}
	before, err := store.LoadEventHistory(ctx, id)
	if err != nil {
		t.Fatalf("LoadEventHistory before compaction: %v", err)
	}
	if err := CompactWorkflowHistory(ctx, store, id, 4, nil); err != nil {
		t.Fatalf("CompactWorkflowHistory: %v", err)
	}
	return id, fields, before
}

// With encryption ON, no sensitive text may appear in the run's workflow_payloads
// row after compaction, in any form. Fails on develop at 79d7751fe: the
// compaction_state column holds the events' fields as JSON, in the clear.
func TestCompactionStateHoldsNoSensitivePlaintextWhenPayloadsAreSealed(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	store := NewPostgresStore(db).WithEncryption(enc, true)

	id, fields, _ := seedAndCompact(t, store, "sealed")

	// Control 1: the events themselves are sealed on disk, so a marker found
	// below came from compaction and not from the seeding.
	var rawEvents string
	if err := db.QueryRow(`SELECT coalesce(string_agg(to_jsonb(e)::text, '|'), '') FROM event_history e WHERE workflow_id = $1`, id).Scan(&rawEvents); err != nil {
		t.Fatalf("read raw events: %v", err)
	}
	for _, f := range fields {
		if got := findLeaks(rawEvents, f.marker); len(got) != 0 {
			t.Fatalf("control failed: event_history holds %s in the clear (%v), so this test would measure nothing", f.name, got)
		}
	}

	// Control 2: compaction ran and wrote a state.
	var stateBytes int
	if err := db.QueryRow(`SELECT coalesce(length(compaction_state::text), 0) FROM workflow_payloads WHERE id = $1`, id).Scan(&stateBytes); err != nil {
		t.Fatalf("read compaction_state length: %v", err)
	}
	if stateBytes == 0 {
		t.Fatal("control failed: compaction wrote no compaction_state, so there is nothing to search")
	}

	row := compactionRowText(t, store, id)
	for _, f := range fields {
		if got := findLeaks(row, f.marker); len(got) != 0 {
			t.Errorf("workflow_payloads holds the plaintext of the sealed event field %s, as %v (row is %d bytes)",
				f.name, got, len(row))
		}
	}
}

// The known-positive for the test above: with encryption OFF the same run leaves
// the markers in the row, so the search CAN see them in a real row of this table.
func TestTheSameSearchFindsTheMarkersWhenPayloadsAreNotSealed(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	store := NewPostgresStore(db)

	id, fields, _ := seedAndCompact(t, store, "plain")

	row := compactionRowText(t, store, id)
	for _, f := range fields {
		if len(findLeaks(row, f.marker)) == 0 {
			t.Errorf("with encryption off the marker for %s is not in the row: the search cannot see a leak in this table, so the sealed test above proves nothing", f.name)
		}
	}
}

// A sealed compaction state reads back to the same CompactionState, and the
// history rebuilt from it matches the history before compaction, field for
// field. That rebuilt history is what replay consumes.
func TestASealedCompactionStateReplaysTheSameHistory(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	store := NewPostgresStore(db).WithEncryption(enc, true)
	ctx := context.Background()

	id, _, before := seedAndCompact(t, store, "replay")

	tail, err := store.LoadEventHistory(ctx, id)
	if err != nil {
		t.Fatalf("LoadEventHistory after compaction: %v", err)
	}
	if len(tail) >= len(before) {
		t.Fatalf("precondition: compaction removed no events from event_history (%d before, %d after), so the state is not carrying anything", len(before), len(tail))
	}
	t.Logf("event_history: %d events before compaction, %d after; the other %d exist only in compaction_state",
		len(before), len(tail), len(before)-len(tail))

	cs, err := store.LoadCompactionState(ctx, id)
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if cs == nil {
		t.Fatal("LoadCompactionState returned no state for a compacted run")
	}

	rebuilt := buildFullHistoryFromCompaction(tail, cs)
	if len(rebuilt) != len(before) {
		t.Fatalf("rebuilt history has %d events, the history before compaction had %d", len(rebuilt), len(before))
	}
	for i := range before {
		w, g := before[i], rebuilt[i]
		same := w.EventType == g.EventType && w.Request == g.Request && w.Response == g.Response && w.Err == g.Err &&
			w.SignalPayload == g.SignalPayload && w.ChildInput == g.ChildInput && w.NewInput == g.NewInput &&
			w.PluginInput == g.PluginInput && w.PluginOutput == g.PluginOutput &&
			w.PromiseResult == g.PromiseResult && w.PromiseError == g.PromiseError
		if !same {
			t.Errorf("event %d (%s) differs after a round trip through the sealed compaction state:\n  before  %+v\n  rebuilt %+v", i, w.EventType, w, g)
		}
	}
}

// A run compacted by a worker that wrote the state in the clear (every release
// through 0.5.0 with encryption on) must still read: the mixed-history rule every
// other sealed column follows.
func TestAPlaintextCompactionStateStillReadsWhenPayloadsAreSealed(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	ctx := context.Background()

	plain := NewPostgresStore(db) // what wrote the state before the fix, as far as this column goes
	id, _, _ := seedAndCompact(t, plain, "legacy")
	want, err := plain.LoadCompactionState(ctx, id)
	if err != nil || want == nil {
		t.Fatalf("read the plaintext state back with the plain store: %v, %v", want, err)
	}

	sealed := NewPostgresStore(db).WithEncryption(enc, true)
	got, err := sealed.LoadCompactionState(ctx, id)
	if err != nil {
		t.Fatalf("a plaintext compaction state must still read with encryption on, got: %v", err)
	}
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if string(gj) != string(wj) {
		t.Errorf("the plaintext state read differently through the encrypting store:\n got  %s\n want %s", gj, wj)
	}
}

// A sealed state that this worker cannot open is an error that says so, not a nil
// state: the worker would otherwise replay the tail alone and diverge.
func TestASealedCompactionStateThatCannotBeOpenedIsAnErrorNotAnEmptyState(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	encA, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption A: %v", err)
	}
	ctx := context.Background()

	storeA := NewPostgresStore(db).WithEncryption(encA, true)
	id, _, _ := seedAndCompact(t, storeA, "wrongkey")

	encB, err := NewPayloadEncryption(validKey(t)) // a different random key
	if err != nil {
		t.Fatalf("NewPayloadEncryption B: %v", err)
	}
	storeB := NewPostgresStore(db).WithEncryption(encB, true)
	cs, err := storeB.LoadCompactionState(ctx, id)
	if err == nil {
		t.Fatalf("a compaction state sealed under another key loaded without error (%d events)", len(cs.Events))
	}
	if !errors.Is(err, ErrPayloadDecryption) {
		t.Errorf("error = %v, want one wrapping ErrPayloadDecryption (the worker releases a run on that, as it does for history)", err)
	}
}
