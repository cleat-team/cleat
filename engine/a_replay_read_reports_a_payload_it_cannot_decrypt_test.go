package engine

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// cleat#2311. decryptField and decryptPayloadJSON swallowed a failure: the
// first substituted "[DECRYPTION_FAILED]" for the field, the second returned
// the ciphertext, and neither said anything. The replay read (LoadEventHistory)
// therefore succeeded, and a worker holding the wrong key either failed the run
// on the checksum chain or -- with checksums off -- completed it on the
// placeholder. These tests pin the load itself: it must ERROR, and the error
// must be the one the worker keys its release on.

// sealedRecordFor returns a record whose ONLY populated sensitive field is
// `field`, sealed under enc the way the write path stores it.
func sealedRecordFor(t *testing.T, enc *PayloadEncryption, field string) *EventRecord {
	t.Helper()
	tc := mustSeal(t, enc, DefaultTenantUUID)
	sealed, err := tc.sealString("secret-" + field)
	if err != nil {
		t.Fatalf("seal %s: %v", field, err)
	}
	rec := &EventRecord{Step: 3, EventType: EventTypeCall}
	switch field {
	case "Request", "Response":
		// These two arrive raw: the read path base64-decodes them first.
		raw, err := tc.seal([]byte("secret-" + field))
		if err != nil {
			t.Fatalf("seal %s: %v", field, err)
		}
		if field == "Request" {
			rec.Request = string(raw)
		} else {
			rec.Response = string(raw)
		}
	case "Err":
		rec.Err = sealed
	case "SignalPayload":
		rec.SignalPayload = sealed
	case "ChildInput":
		rec.ChildInput = sealed
	case "NewInput":
		rec.NewInput = sealed
	case "PluginInput":
		rec.PluginInput = sealed
	case "PluginOutput":
		rec.PluginOutput = sealed
	case "PromiseResult":
		rec.PromiseResult = sealed
	case "PromiseError":
		rec.PromiseError = sealed
	default:
		t.Fatalf("unknown field %q", field)
	}
	return rec
}

var sensitiveEventFields = []string{
	"Request", "Response", "Err", "SignalPayload", "ChildInput", "NewInput",
	"PluginInput", "PluginOutput", "PromiseResult", "PromiseError",
}

// Every one of the ten fields must report a failure, by name -- including the
// signal payload and child input the issue named as unmeasured. Each is sealed
// under key A alone and read under key B, so a field that swallowed its own
// failure cannot hide behind another that reported one.
func TestEveryEncryptedFieldReportsAFailureToDecrypt(t *testing.T) {
	encA, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatal(err)
	}
	encB, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range sensitiveEventFields {
		t.Run(field, func(t *testing.T) {
			// Known-positive: the same record under the RIGHT key is clean.
			// Without it, "reports an error" could be "reports one always".
			okStore := NewPostgresStore(nil).WithEncryption(encA, true)
			if err := okStore.decryptAndRedactEventRecord(sealedRecordFor(t, encA, field), "wf-1"); err != nil {
				t.Fatalf("PRECONDITION FAILED: %s under the key that sealed it reported %v", field, err)
			}

			wrongStore := NewPostgresStore(nil).WithEncryption(encB, true)
			rec := sealedRecordFor(t, encA, field)
			err := wrongStore.decryptAndRedactEventRecord(rec, "wf-1")
			if !errors.Is(err, ErrPayloadDecryption) {
				t.Fatalf("%s sealed under another key: err = %v, want ErrPayloadDecryption", field, err)
			}
			if !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "step 3") {
				t.Errorf("the error should name the field and step: %v", err)
			}
		})
	}
}

// Every field is still processed after the first failure, so a caller that
// ignores the error (the display paths) gets a fully marked record.
func TestAFailureToDecryptOneFieldDoesNotStopTheRest(t *testing.T) {
	encA, _ := NewPayloadEncryption(validKey(t))
	encB, _ := NewPayloadEncryption(validKey(t))
	rec := sealedRecordFor(t, encA, "Err")
	rec.SignalPayload = sealedRecordFor(t, encA, "SignalPayload").SignalPayload
	rec.ChildInput = sealedRecordFor(t, encA, "ChildInput").ChildInput

	err := NewPostgresStore(nil).WithEncryption(encB, true).decryptAndRedactEventRecord(rec, "wf-1")
	if !errors.Is(err, ErrPayloadDecryption) {
		t.Fatalf("err = %v", err)
	}
	for name, got := range map[string]string{"Err": rec.Err, "SignalPayload": rec.SignalPayload, "ChildInput": rec.ChildInput} {
		if got != "[DECRYPTION_FAILED]" {
			t.Errorf("%s = %q, want the placeholder for a caller that ignores the error", name, got)
		}
	}
}

// The payload JSON column: a sealed-shaped value that will not open is a
// failure; a plaintext payload (mixed history from before encryption was
// switched on) is not, and must not be turned into one.
func TestPayloadColumnDistinguishesUnsealedFromUnopenable(t *testing.T) {
	encA, _ := NewPayloadEncryption(validKey(t))
	encB, _ := NewPayloadEncryption(validKey(t))

	sealed, err := mustSeal(t, encA, DefaultTenantUUID).sealJSON([]byte(`{"k":"v"}`))
	if err != nil {
		t.Fatal(err)
	}

	wrong := NewPostgresStore(nil).WithEncryption(encB, true)
	if _, err := wrong.decryptPayloadJSON(string(sealed)); !errors.Is(err, ErrPayloadDecryption) {
		t.Errorf("sealed under another key: err = %v, want ErrPayloadDecryption", err)
	}
	if out, err := wrong.decryptPayloadJSON(`{"k":"plain"}`); err != nil || out != `{"k":"plain"}` {
		t.Errorf("a plaintext payload is not a failure: out=%q err=%v", out, err)
	}
	right := NewPostgresStore(nil).WithEncryption(encA, true)
	if out, err := right.decryptPayloadJSON(string(sealed)); err != nil || out != `{"k":"v"}` {
		t.Errorf("under the right key: out=%q err=%v", out, err)
	}
}

// The end-to-end shape: events written under key A through the real write
// path, read under key B. The replay read errors; the display read carries on
// with the placeholder, as it did before -- so this fix cannot have blanked a
// dashboard that used to show something. Read under key A, the same rows load
// clean, which is the control for both.
func TestLoadEventHistoryFailsOnAnUnreadableHistoryButDisplayReadsDoNot(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	encA, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatal(err)
	}
	encB, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatal(err)
	}
	storeA := NewPostgresStore(db).WithEncryption(encA, true)
	eng := NewEngine(nil, nil, WithDB(db), WithEncryption(encA, true), WithTenantID(storeA.tenantID))
	ctx := context.Background()
	runID := appendChainWorkflow(t, storeA)

	rec := EventRecord{
		Step: 0, EventType: EventTypeCall, Service: "payments", Op: "charge",
		Request: `{"card":"4111111111111111"}`, Response: `{"ok":true}`,
	}
	if err := eng.flushEvent(ctx, runID, rec, ""); err != nil {
		t.Fatalf("flushEvent: %v", err)
	}

	got, err := storeA.LoadEventHistory(ctx, runID)
	if err != nil || len(got) != 1 || got[0].Request != rec.Request {
		t.Fatalf("PRECONDITION FAILED: under the sealing key: %d events, err=%v -- nothing below measures a wrong key", len(got), err)
	}

	storeB := NewPostgresStore(db).WithEncryption(encB, true)
	if _, err := storeB.LoadEventHistory(ctx, runID); !errors.Is(err, ErrPayloadDecryption) {
		t.Fatalf("LoadEventHistory under the wrong key: err = %v, want ErrPayloadDecryption", err)
	}

	page, err := storeB.LoadEventHistoryPaginated(ctx, runID, 0, 10)
	if err != nil {
		t.Fatalf("the display read must not fail on an unreadable history: %v", err)
	}
	if len(page) != 1 || page[0].Request != "[DECRYPTION_FAILED]" {
		t.Fatalf("display read under the wrong key: %+v, want one event carrying the placeholder", page)
	}
}

// sealedRun writes three events under encA through the real write path -- a
// call, a signal and a child start -- and returns the store and run id. Between
// them the rows carry the request/response columns, signal_payload,
// child_input and the sealed payload column, which is what lets the next tests
// pin the field check and the payload-column check each on its own.
func sealedRun(t *testing.T, encA *PayloadEncryption) (*PostgresStore, string) {
	t.Helper()
	db := testDB(t)
	t.Cleanup(func() { db.Close() })
	storeA := NewPostgresStore(db).WithEncryption(encA, true)
	eng := NewEngine(nil, nil, WithDB(db), WithEncryption(encA, true), WithTenantID(storeA.tenantID))
	runID := appendChainWorkflow(t, storeA)
	for i, rec := range []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "p", Op: "c", Request: `{"a":1}`, Response: `{"ok":true}`},
		{Step: 1, EventType: EventTypeSignalReceived, SignalName: "go", SignalPayload: `{"x":1}`},
		{Step: 2, EventType: EventTypeChildWorkflow, ChildName: "kid", ChildInput: `{"k":1}`, RunID: "r"},
	} {
		if err := eng.flushEvent(context.Background(), runID, rec, ""); err != nil {
			t.Fatalf("flushEvent %d: %v", i, err)
		}
	}
	if got, err := storeA.LoadEventHistory(context.Background(), runID); err != nil || len(got) != 3 {
		t.Fatalf("PRECONDITION FAILED: under the sealing key: %d events, err=%v", len(got), err)
	}
	return storeA, runID
}

// Each strict check is pinned on its own. With both a sealed field and a sealed
// payload on every row, either check alone would make the load fail, so
// removing one of them changes nothing a single test can see. Here one is
// stripped at a time with SQL: fields-only rows have no payload, payload-only
// rows have no sealed field.
func TestTheFieldCheckAndThePayloadCheckEachFailTheLoadOnTheirOwn(t *testing.T) {
	encA, _ := NewPayloadEncryption(validKey(t))
	encB, _ := NewPayloadEncryption(validKey(t))
	storeA, runID := sealedRun(t, encA)
	ctx := context.Background()
	db := storeA.db
	storeB := NewPostgresStore(db).WithEncryption(encB, true)

	// fields only: drop the payload column.
	if _, err := db.Exec(`UPDATE event_history SET payload = NULL WHERE workflow_id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := storeA.LoadEventHistory(ctx, runID); err != nil {
		t.Fatalf("PRECONDITION FAILED: fields-only rows under the sealing key: %v", err)
	}
	err := func() error { _, e := storeB.LoadEventHistory(ctx, runID); return e }()
	if !errors.Is(err, ErrPayloadDecryption) || strings.Contains(err.Error(), "payload column") {
		t.Errorf("fields-only rows under the wrong key: err = %v, want a FIELD failure", err)
	}

	// payload only: put the payload back is not possible, so build a second run.
	storeA2, runID2 := sealedRun(t, encA)
	if _, err := db.Exec(`UPDATE event_history SET request = NULL, response = NULL, signal_payload = NULL, child_input = NULL WHERE workflow_id = $1`, runID2); err != nil {
		t.Fatal(err)
	}
	if _, err := storeA2.LoadEventHistory(ctx, runID2); err != nil {
		t.Fatalf("PRECONDITION FAILED: payload-only rows under the sealing key: %v", err)
	}
	err = func() error { _, e := storeB.LoadEventHistory(ctx, runID2); return e }()
	if !errors.Is(err, ErrPayloadDecryption) || !strings.Contains(err.Error(), "payload column") {
		t.Errorf("payload-only rows under the wrong key: err = %v, want a PAYLOAD-COLUMN failure", err)
	}
}

// The three admin operations that read the history and then write to it must
// refuse an unreadable one BEFORE writing. Each is checked against a run whose
// status would let it proceed, and the event count is compared before and after
// so "refused" cannot be a refusal for some other reason.
func TestAdminOperationsRefuseAnUnreadableHistoryBeforeWriting(t *testing.T) {
	encA, _ := NewPayloadEncryption(validKey(t))
	encB, _ := NewPayloadEncryption(validKey(t))
	storeA, runID := sealedRun(t, encA)
	ctx := context.Background()
	db := storeA.db
	storeB := NewPostgresStore(db).WithEncryption(encB, true)

	count := func() (n int) {
		if err := db.QueryRow(`SELECT count(*) FROM event_history WHERE workflow_id = $1`, runID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}
	status := func(s string) {
		if _, err := db.Exec(`UPDATE workflow_instances SET status = $2 WHERE id = $1`, runID, s); err != nil {
			t.Fatalf("set status %s: %v", s, err)
		}
	}
	before := count()

	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrPayloadDecryption) || !errors.Is(err, ErrAdminStateConflict) {
			t.Errorf("%s: err = %v, want both ErrPayloadDecryption and ErrAdminStateConflict (a 409)", name, err)
			return
		}
		for _, leak := range []string{"message authentication", "cipher", "decrypt: open"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: the operator-facing message carries driver text %q: %v", name, leak, err)
			}
		}
		if !strings.Contains(err.Error(), "Nothing was changed") {
			t.Errorf("%s: the message should say nothing was changed: %v", name, err)
		}
		if got := count(); got != before {
			t.Errorf("%s: event count went %d -> %d; a refused operation must not write", name, before, got)
		}
	}

	status("failed")
	check("re-replay", ReReplay(ctx, storeB, runID, 0, "op"))
	var st string
	_ = db.QueryRow(`SELECT status FROM workflow_instances WHERE id = $1`, runID).Scan(&st)
	if st != "failed" {
		t.Errorf("re-replay changed the status to %q", st)
	}

	status("dead_lettered")
	check("retry", RetryWorkflow(ctx, storeB, runID))

	check("resolve step", ResolveStep(ctx, storeB, runID, 0, `{"ok":true}`, "op"))

	// force-complete and force-fail do not load the history themselves; they
	// append their audit event through adminAppendAudit. They are refused there,
	// inside the transaction that holds their status change, so the change is
	// rolled back with it. Both answered 200 in the first version of this fix.
	status("ready")
	var gen int64
	if err := db.QueryRow(`SELECT generation FROM workflow_instances WHERE id = $1`, runID).Scan(&gen); err != nil {
		t.Fatal(err)
	}
	check("force-complete", ForceComplete(ctx, storeB, runID, gen, "op", `{"done":true}`))
	check("force-fail", ForceFail(ctx, storeB, runID, gen, "op", "boom", ErrUnknown.String()))
	_ = db.QueryRow(`SELECT status FROM workflow_instances WHERE id = $1`, runID).Scan(&st)
	if st != "ready" {
		t.Errorf("a refused force-resolve left the run %q; its status change must roll back with the audit event", st)
	}

	// Known-positive for the two checks above: the same calls on a store that
	// CAN read the history go through (a refusal that fires for every store
	// would satisfy everything above).
	if err := ForceFail(ctx, storeA, runID, gen, "op", "boom", ErrUnknown.String()); err != nil {
		t.Fatalf("PRECONDITION FAILED: force-fail under the sealing key was refused: %v", err)
	}
}

// Plaintext is not a decryption failure. Rows that were never sealed exist
// legitimately: everything written before an operator turns
// --encrypt-sensitive-payloads on for an existing deployment, child events
// before cleat#2328 was fixed, and sharded writes before cleat#2308. The strict
// replay load used to refuse them ("illegal base64 data") and release such a run
// forever, where develop read them fine. Only a value that is sealed-shaped and
// fails to open is an error.
func TestAPlaintextFieldIsNotADecryptionFailure(t *testing.T) {
	enc, _ := NewPayloadEncryption(validKey(t))
	store := NewPostgresStore(nil).WithEncryption(enc, true)
	store.disableReadRedaction = true

	rec := &EventRecord{
		Step: 4, EventType: EventTypeChildWorkflow,
		Request: `{"a":1}`, Response: `{"ok":true}`,
		Err: "connection refused: dial tcp 10.0.0.1:443", SignalPayload: `{"x":1}`,
		ChildInput: `{"order":"o-1"}`, NewInput: `{"n":2}`, PluginInput: `{"p":3}`,
		PluginOutput: `{"q":4}`, PromiseResult: `"done"`, PromiseError: "boom",
	}
	want := *rec
	if err := store.decryptAndRedactEventRecord(rec, "wf-1"); err != nil {
		t.Fatalf("a plaintext record was reported as a decryption failure: %v", err)
	}
	if *rec != want {
		t.Errorf("a plaintext record was changed on read:\n got %+v\nwant %+v", *rec, want)
	}
}

// The boundary of "sealed-shaped", pinned on both sides so that moving it is a
// decision: the shortest value the encryptor can produce is 12 (nonce) + 16
// (tag) = 28 bytes.
func TestSealedShapeBoundary(t *testing.T) {
	b64 := func(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }
	for _, c := range []struct {
		name string
		v    string
		raw  bool
		want bool
	}{
		{"string: JSON", `{"a":1}`, false, false},
		{"string: text with spaces", "connection refused", false, false},
		{"string: base64 of 27 bytes", b64(27), false, false},
		{"string: base64 of 28 bytes", b64(28), false, true},
		{"string: base64 of 100 bytes", b64(100), false, true},
		{"raw: JSON", `{"a":1}`, true, false},
		{"raw: long JSON", `{"customer":"a very long value indeed","n":1234567890}`, true, false},
		{"raw: 27 non-UTF-8 bytes", strings.Repeat("\xff", 27), true, false},
		{"raw: 28 non-UTF-8 bytes", strings.Repeat("\xff", 28), true, true},
	} {
		if got := sealedShape(c.v, c.raw); got != c.want {
			t.Errorf("%s: sealedShape = %v, want %v", c.name, got, c.want)
		}
	}
	// The encryptor's real output is always sealed-shaped: the guard would be
	// wrong the day it were not.
	enc, _ := NewPayloadEncryption(validKey(t))
	tc := mustSeal(t, enc, DefaultTenantUUID)
	for _, plain := range []string{"", "x", `{"k":"v"}`, strings.Repeat("a", 500)} {
		s, err := tc.sealString(plain)
		if err != nil {
			t.Fatal(err)
		}
		if !sealedShape(s, false) {
			t.Errorf("sealString(%q) is not sealed-shaped", plain)
		}
		r, err := tc.seal([]byte(plain))
		if err != nil {
			t.Fatal(err)
		}
		if !sealedShape(string(r), true) {
			t.Errorf("seal(%q) is not sealed-shaped as raw bytes", plain)
		}
	}
}

// Turning encryption on for a deployment that already has runs in flight. The
// events written before the switch are plaintext, the ones after are sealed, and
// the run's history is both. A worker holding the key reads all of it; a worker
// with a different key still refuses, because the SEALED rows are unreadable to
// it; and a history that is entirely plaintext is readable by anyone, since
// there is nothing to open.
func TestTurningEncryptionOnMidRunLeavesTheHistoryReadable(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	encA, _ := NewPayloadEncryption(validKey(t))
	encB, _ := NewPayloadEncryption(validKey(t))
	ctx := context.Background()

	plainStore := NewPostgresStore(db)
	plainEng := NewEngine(nil, nil, WithDB(db), WithTenantID(plainStore.tenantID))
	runID := appendChainWorkflow(t, plainStore)
	for i, rec := range []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "p", Op: "c", Request: `{"a":1}`, Response: `{"ok":true}`},
		{Step: 1, EventType: EventTypeSignalReceived, SignalName: "go", SignalPayload: `{"x":1}`},
		{Step: 2, EventType: EventTypeChildWorkflow, ChildName: "kid", ChildInput: `{"k":1}`, RunID: "r"},
	} {
		if err := plainEng.flushEvent(ctx, runID, rec, ""); err != nil {
			t.Fatalf("plaintext flushEvent %d: %v", i, err)
		}
	}
	// Control: these really are plaintext on disk, or none of this measures
	// what it says.
	var stored string
	if err := db.QueryRow(`SELECT COALESCE(child_input, '') FROM event_history WHERE workflow_id = $1 AND step = 2`, runID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != `{"k":1}` {
		t.Fatalf("PRECONDITION FAILED: child_input is stored as %q, want plaintext", stored)
	}

	storeA := NewPostgresStore(db).WithEncryption(encA, true)
	storeB := NewPostgresStore(db).WithEncryption(encB, true)

	read := func(name string, st *PostgresStore) []EventRecord {
		t.Helper()
		h, err := st.LoadEventHistory(ctx, runID)
		if err != nil {
			t.Fatalf("%s: a history with only plaintext rows failed to load: %v", name, err)
		}
		return h
	}
	check := func(name string, h []EventRecord) {
		t.Helper()
		if len(h) < 3 || h[0].Request != `{"a":1}` || h[0].Response != `{"ok":true}` ||
			h[1].SignalPayload != `{"x":1}` || h[2].ChildInput != `{"k":1}` {
			t.Errorf("%s: plaintext rows read back changed: %+v", name, h)
		}
	}
	check("encryption on, key A", read("key A", storeA))
	check("encryption on, key B", read("key B", storeB)) // nothing sealed yet

	// Encryption is switched on; the run carries on and writes a sealed event.
	sealedEng := NewEngine(nil, nil, WithDB(db), WithEncryption(encA, true), WithTenantID(storeA.tenantID))
	if err := sealedEng.flushEvent(ctx, runID, EventRecord{Step: 3, EventType: EventTypeCall, Service: "p", Op: "d",
		Request: `{"b":2}`, Response: `{"ok":true}`}, ""); err != nil {
		t.Fatalf("sealed flushEvent: %v", err)
	}
	h := read("key A, mixed", storeA)
	check("mixed history, key A", h)
	if len(h) != 4 || h[3].Request != `{"b":2}` {
		t.Errorf("the sealed event did not read back under its own key: %+v", h)
	}
	if _, err := storeB.LoadEventHistory(ctx, runID); !errors.Is(err, ErrPayloadDecryption) {
		t.Errorf("a mixed history under the wrong key: err = %v, want ErrPayloadDecryption (the sealed row is unreadable)", err)
	}
}
