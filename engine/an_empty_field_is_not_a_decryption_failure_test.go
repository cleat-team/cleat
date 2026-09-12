package engine

import (
	"context"
	"testing"
)

// --encrypt-sensitive-payloads had no round-trip test at all: every test in
// this package either writes with encryption on and asserts nothing about what
// comes back (TestFlushEvent_EncryptSensitivePayloads), or exercises
// PayloadEncryption directly without a store. So the write path and the read
// path have never been compared, and they disagree.
//
// engine/flush.go encrypts seven of the ten sensitive fields only when they
// are non-empty -- `if rec.ChildInput != "" { ... }` and six more like it. An
// empty field is therefore stored empty. engine/db.go's decryptField has no
// such guard, so it hands "" to DecryptString, which reports "ciphertext too
// short" for a zero-length input, and the field is replaced with the
// "[DECRYPTION_FAILED]" sentinel.
//
// That sentinel survives all the way to the caller. populateFromPayload runs
// after decryption and could overwrite it, but eventRecordToPayload omits
// empty fields from the payload JSON, so there is no key to overwrite it with.
//
// The effect is not a corner case. A plain `call` event leaves all seven
// empty, so with encryption enabled every ordinary event in every history
// reads back carrying seven false reports of data loss -- on the dashboard, in
// cleatctl, and in the replay path.
//
// The existing encryption test never saw it because it populates every field
// with a non-empty value.
func TestAnEmptyFieldIsNotADecryptionFailure(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	store := NewPostgresStore(db).WithEncryption(enc, true)
	eng := NewEngine(nil, nil, WithDB(db), WithEncryption(enc, true), WithTenantID(store.tenantID))
	ctx := context.Background()
	runID := appendChainWorkflow(t, store)

	// An ordinary call: two fields carry data, seven are empty. This is the
	// shape of nearly every event a workflow writes.
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "payments",
		Op:        "charge",
		Request:   `{"card":"4111111111111111"}`,
		Response:  `{"ok":true}`,
	}
	if err := eng.flushEvent(ctx, runID, rec, ""); err != nil {
		t.Fatalf("flushEvent: %v", err)
	}

	got, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("PRECONDITION FAILED: loaded %d events, want 1 -- nothing below is measuring a round trip", len(got))
	}

	// The control, and it is what gives the assertion below its meaning: a
	// field that DOES carry data must come back decrypted. Without this, a
	// store with encryption switched off passes every assertion in this test.
	if got[0].Request != rec.Request {
		t.Fatalf("PRECONDITION FAILED: Request came back %q, want %q -- encryption did not round-trip, "+
			"so an empty field reading back empty would prove nothing", got[0].Request, rec.Request)
	}
	// The guard in decryptField rests on this: the encryptor can never produce
	// an empty string, so an empty stored value was never encrypted. If
	// EncryptString ever returns "", "empty means never encrypted" stops being
	// true and the guard starts hiding real failures.
	if ct, err := enc.EncryptString(""); err != nil || ct == "" {
		t.Fatalf("EncryptString(\"\") = %q, %v -- decryptField treats an empty stored value as "+
			"never-encrypted, which is only sound while the encryptor cannot produce one", ct, err)
	}

	var stored string
	if err := db.QueryRow(`SELECT request FROM event_history WHERE workflow_id = $1 AND step = 0`,
		runID).Scan(&stored); err != nil {
		t.Fatalf("read stored request: %v", err)
	}
	if stored == rec.Request {
		t.Fatalf("PRECONDITION FAILED: request is stored as plaintext %q, so this test is not "+
			"exercising encryption at all", stored)
	}

	for _, f := range []struct {
		name string
		got  string
	}{
		{"SignalPayload", got[0].SignalPayload},
		{"ChildInput", got[0].ChildInput},
		{"NewInput", got[0].NewInput},
		{"PluginInput", got[0].PluginInput},
		{"PluginOutput", got[0].PluginOutput},
		{"PromiseResult", got[0].PromiseResult},
		{"PromiseError", got[0].PromiseError},
	} {
		if f.got != "" {
			t.Errorf("%s was never written, and the write path stores an empty field empty, "+
				"but it reads back as %q", f.name, f.got)
		}
	}
}
