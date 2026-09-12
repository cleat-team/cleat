package engine

import (
	"context"
	"strings"
	"testing"
)

// --encrypt-sensitive-payloads covered engine/flush.go's per-step insert and
// left the batch path -- FinalizeWorkflowSegment, ContinueAsNew, the defer
// phase, the audit writes and AppendEventHistoryBatch -- storing plaintext.
// cleat#1306.
//
// The two assertions below are different questions and both are needed. That
// the ciphertext is not the plaintext says the value was transformed; that it
// decrypts back to the plaintext says it was transformed the right way and
// exactly once. A double-encrypted column passes the first and fails the
// second, and double-encryption is the specific hazard the old NOTE on
// PostgresStore.encryption gave as the reason not to do this at all.
func TestTheBatchWritePathEncrypts(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	store := NewPostgresStore(db).WithEncryption(enc, true)
	ctx := context.Background()
	runID := appendChainWorkflow(t, store)

	const card = `{"card":"4111111111111111"}`
	const ssn = `{"ssn":"123-45-6789"}`
	rec := EventRecord{
		Step: 0, EventType: EventTypeCall,
		Service: "payments", Op: "Charge",
		Request: card, Response: `{"ok":true}`, ChildInput: ssn,
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, []EventRecord{rec}); err != nil {
		t.Fatalf("append batch: %v", err)
	}

	var storedReq, storedChild, storedPayload string
	if err := db.QueryRow(`SELECT request, child_input, payload::text
		FROM event_history WHERE workflow_id = $1 AND step = 0`, runID).
		Scan(&storedReq, &storedChild, &storedPayload); err != nil {
		t.Fatalf("read stored columns: %v", err)
	}

	// Nothing recognisable is at rest. tryEncodeBase64 is what the plaintext
	// path stores, so comparing against it rather than against the raw string
	// is what makes this fail on the pre-fix tree -- the column held
	// base64("{\"card\":...}"), which is not the plaintext either.
	for _, f := range []struct{ name, stored, plain string }{
		{"request", storedReq, tryEncodeBase64(card)},
		{"child_input", storedChild, ssn},
	} {
		if f.stored == f.plain {
			t.Errorf("%s is stored exactly as the unencrypted path would store it (%.40q), "+
				"so this write path did not encrypt", f.name, f.stored)
		}
	}
	if strings.Contains(storedPayload, "4111111111111111") || strings.Contains(storedPayload, "123-45-6789") {
		t.Errorf("payload column carries the plaintext: %.80q", storedPayload)
	}

	// And it decrypts back, exactly once. Reading through the store is the
	// honest check: it is the path every consumer uses, and it is what would
	// break if the payload had been built from an already-encrypted record --
	// populateFromPayload runs after decryption and would write the ciphertext
	// field values back over the columns.
	got, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("PRECONDITION FAILED: loaded %d events, want 1", len(got))
	}
	//
	// Request and Response are the weakest of the three here, and it is worth
	// knowing why rather than trusting them. With the fix reverted, the
	// plaintext payload column REPAIRS them on read: decryptField leaves
	// "[DECRYPTION_FAILED]" in the columns, then populateFromPayload runs
	// afterwards and overwrites both from the payload's request_b64 and
	// response_b64 keys, which were also plaintext. So they round-trip
	// correctly through a store that encrypted nothing. ChildInput is not a
	// key on a call event's payload, so nothing repairs it -- which is the
	// only reason the round trip below fails on the pre-fix tree, and it is
	// the same mechanism that kept plaintext-at-rest invisible.
	for _, f := range []struct{ name, got, want string }{
		{"Request", got[0].Request, card},
		{"Response", got[0].Response, `{"ok":true}`},
		{"ChildInput", got[0].ChildInput, ssn},
	} {
		if f.got != f.want {
			t.Errorf("%s round-tripped to %q, want %q", f.name, f.got, f.want)
		}
	}

	// The checksum is computed over the plaintext record on every write path,
	// and VerifyWorkflowEvents recomputes it from the decrypted record it
	// loads. Checksumming the encrypted form instead would make every
	// encrypted workflow verify as corrupt, and nothing above would notice.
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Errorf("VerifyWorkflowEvents on an encrypted batch-written workflow: %v", err)
	}
}

// The per-event path and the batch path now share one encoding, so an event
// written by either is indistinguishable at rest. That is the property that
// makes "which path wrote this row" stop mattering -- and it is what the two
// paths did not have, in both directions: the batch path did not encrypt at
// all, and the per-event path encrypted empty values that the batch path
// would have stored empty.
func TestBothWritePathsStoreAnEventTheSameWay(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	store := NewPostgresStore(db).WithEncryption(enc, true)
	eng := NewEngine(nil, nil, WithDB(db), WithEncryption(enc, true), WithTenantID(store.tenantID))
	ctx := context.Background()

	rec := EventRecord{
		Step: 0, EventType: EventTypeCall,
		Service: "payments", Op: "Charge",
		Request: `{"card":"4111111111111111"}`, Response: `{"ok":true}`,
	}

	perEvent := appendChainWorkflow(t, store)
	if err := eng.flushEvent(ctx, perEvent, rec, ""); err != nil {
		t.Fatalf("flushEvent: %v", err)
	}
	batch := appendChainWorkflow(t, store)
	if err := store.AppendEventHistoryBatch(ctx, batch, []EventRecord{rec}); err != nil {
		t.Fatalf("append batch: %v", err)
	}

	// Compare what CAN be compared. The ciphertexts differ every time -- a
	// fresh nonce per Encrypt call -- so comparing the stored bytes would
	// fail on a correct tree. What has to agree is which columns are NULL,
	// which are empty and which carry data, because that is where the two
	// encodings actually diverged.
	shape := func(wfID string) map[string]string {
		t.Helper()
		var req, resp, child, errCol, payload *string
		if err := db.QueryRow(`SELECT request, response, child_input, error, payload::text
			FROM event_history WHERE workflow_id = $1 AND step = 0`, wfID).
			Scan(&req, &resp, &child, &errCol, &payload); err != nil {
			t.Fatalf("read %s: %v", wfID, err)
		}
		out := map[string]string{}
		for name, v := range map[string]*string{
			"request": req, "response": resp, "child_input": child,
			"error": errCol, "payload": payload,
		} {
			switch {
			case v == nil:
				out[name] = "NULL"
			case *v == "":
				out[name] = "empty"
			default:
				out[name] = "present"
			}
		}
		return out
	}

	a, b := shape(perEvent), shape(batch)
	for _, col := range []string{"request", "response", "child_input", "error", "payload"} {
		if a[col] != b[col] {
			t.Errorf("column %s: per-event path stored %s, batch path stored %s -- "+
				"the two paths do not agree on the encoding of one event", col, a[col], b[col])
		}
	}

	// The control: without it, two paths that both stored nothing would pass
	// every assertion above.
	if a["request"] != "present" || a["payload"] != "present" {
		t.Fatalf("PRECONDITION FAILED: the per-event path stored request=%s payload=%s, "+
			"so agreement with the batch path proves nothing", a["request"], a["payload"])
	}

	// NULL/empty/present agreement alone is too weak to earn this test's
	// name, and that is measured rather than supposed: with the batch path's
	// encryption reverted, every assertion above still passed, because
	// plaintext and ciphertext are both "present". So compare what the two
	// stored forms MEAN. The ciphertexts themselves cannot be compared -- a
	// fresh nonce per call makes them differ on a correct tree -- but they
	// must decrypt to the same thing.
	for _, wf := range []struct{ label, id string }{{"per-event", perEvent}, {"batch", batch}} {
		var storedReq string
		if err := db.QueryRow(`SELECT request FROM event_history WHERE workflow_id = $1 AND step = 0`,
			wf.id).Scan(&storedReq); err != nil {
			t.Fatalf("read %s request: %v", wf.label, err)
		}
		plain, err := enc.DecryptString(storedReq)
		if err != nil {
			t.Errorf("the %s path's stored request does not decrypt (%v), so that path did not "+
				"encrypt it: %.40q", wf.label, err, storedReq)
			continue
		}
		if plain != rec.Request {
			t.Errorf("the %s path's stored request decrypts to %q, want %q", wf.label, plain, rec.Request)
		}
	}
}
