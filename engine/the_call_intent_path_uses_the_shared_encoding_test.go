package engine

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

// The call-intent pair wrote its own SQL rather than using the encoding every
// other writer uses, and diverged on both axes the encoding covers.
// cleat#1379.
//
//   - WriteCallIntent bound rec.Request RAW. Every INSERT path base64-encodes
//     it and every read path applies tryDecodeBase64, which falls back to the
//     raw string only when decoding FAILS -- so a raw request that happens to
//     be valid base64 decodes to the wrong bytes. #1319 measures six of nine
//     ordinary short values as valid base64; "true" and "null" are two of them.
//
//   - Neither statement encrypted, so --encrypt-sensitive-payloads left every
//     write-ahead intent's request, and every completion's response and
//     payload, in the clear.
//
// Both are fixed by routing through encodeEventForStorage (#1380).
func TestTheCallIntentPathBase64EncodesTheRequest(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()

	// Values chosen because they are valid base64: 4 characters from
	// [A-Za-z0-9+/] with no padding needed. A raw "true" in the column is
	// decoded by tryDecodeBase64 into three bytes of binary.
	for _, req := range []string{"true", "null", "1234"} {
		runID := appendChainWorkflow(t, store)
		rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: req}
		if err := store.WriteCallIntent(ctx, runID, rec, "", 0); err != nil {
			t.Fatalf("write call intent %q: %v", req, err)
		}

		var stored string
		if err := db.QueryRowContext(ctx,
			`SELECT request FROM event_history WHERE workflow_id = $1 AND step = 0`, runID).
			Scan(&stored); err != nil {
			t.Fatalf("read stored request for %q: %v", req, err)
		}
		want := base64.StdEncoding.EncodeToString([]byte(req))
		if stored != want {
			t.Errorf("WriteCallIntent stored request %q as %q, want %q.\n"+
				"Read back through tryDecodeBase64 the raw form decodes to %q, which is not what "+
				"the workflow sent", req, stored, want, tryDecodeBase64(stored))
		}
	}

	// The control, and it is what makes the assertion above mean something:
	// the column is read through tryDecodeBase64, so storing base64 is only
	// correct if the read undoes exactly one layer.
	runID := appendChainWorkflow(t, store)
	rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: "true"}
	if err := store.WriteCallIntent(ctx, runID, rec, "", 0); err != nil {
		t.Fatalf("write call intent: %v", err)
	}
	rec.Response = `{"ok":true}`
	payload, _ := eventRecordToPayload(rec)
	if err := store.CompleteCallIntent(ctx, runID, rec, payload, "chk-1379", "", 0); err != nil {
		t.Fatalf("complete call intent: %v", err)
	}
	got, err := store.LoadEventHistory(ctx, runID)
	if err != nil || len(got) != 1 {
		t.Fatalf("load history: %v len=%d", err, len(got))
	}
	if got[0].Request != "true" {
		t.Errorf("Request round-tripped to %q, want %q", got[0].Request, "true")
	}
}

func TestTheCallIntentPathEncrypts(t *testing.T) {
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
	rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "payments", Op: "Charge", Request: card}
	if err := store.WriteCallIntent(ctx, runID, rec, "", 0); err != nil {
		t.Fatalf("write call intent: %v", err)
	}

	var storedReq string
	if err := db.QueryRowContext(ctx,
		`SELECT request FROM event_history WHERE workflow_id = $1 AND step = 0`, runID).Scan(&storedReq); err != nil {
		t.Fatalf("read stored request: %v", err)
	}
	// Against the form the UNENCRYPTED path stores, not against the raw
	// string. Comparing to the raw string would pass on a tree that merely
	// base64-encoded and never encrypted.
	if storedReq == tryEncodeBase64(card) {
		t.Errorf("the write-ahead intent's request is stored exactly as the unencrypted path would "+
			"store it (%.40q), so WriteCallIntent did not encrypt it", storedReq)
	}
	if plain, err := enc.DecryptString(storedReq); err != nil {
		t.Errorf("the stored request does not decrypt: %v", err)
	} else if plain != card {
		t.Errorf("the stored request decrypts to %q, want %q -- encrypted, but not exactly once", plain, card)
	}

	rec.Response = ssn
	payload, _ := eventRecordToPayload(rec)
	checksum := computeEventChecksum(rec, "")
	if err := store.CompleteCallIntent(ctx, runID, rec, payload, checksum, "", 0); err != nil {
		t.Fatalf("complete call intent: %v", err)
	}

	var storedResp, storedPayload string
	if err := db.QueryRowContext(ctx,
		`SELECT response, payload::text FROM event_history WHERE workflow_id = $1 AND step = 0`, runID).
		Scan(&storedResp, &storedPayload); err != nil {
		t.Fatalf("read stored completion: %v", err)
	}
	if storedResp == tryEncodeBase64(ssn) {
		t.Errorf("the completion's response is stored as the unencrypted path would store it (%.40q)", storedResp)
	}
	// NOT `strings.Contains(storedPayload, "4111111111111111")`. That is what
	// this check said first, and it could never have fired: the plaintext
	// payload carries request_b64 and response_b64, so the card number is
	// base64 inside it either way and the literal string is absent from a
	// completely unencrypted row. Compare against the form the unencrypted
	// path stores instead -- that is the thing that can disagree.
	// Nor `storedPayload == string(payload)`, which was the second attempt and
	// is a decoration for a different reason: the column is jsonb, so
	// payload::text comes back normalised and never equals the Go-marshalled
	// bytes -- with or without encryption. Measured: with encryption reverted
	// it did not fire, and only the decrypt below did.
	//
	// The shape is the thing that actually distinguishes them. EncryptJSON
	// produces a JSON STRING literal; eventRecordToPayload produces an
	// OBJECT. That is the same discriminator DecryptJSON uses internally.
	if !strings.HasPrefix(storedPayload, `"`) {
		t.Errorf("the payload column holds a JSON object, which is the unencrypted form -- an "+
			"encrypted payload is a JSON string literal: %.60q", storedPayload)
	}
	if decrypted, err := enc.DecryptJSON([]byte(storedPayload)); err != nil {
		t.Errorf("the stored payload does not decrypt: %v", err)
	} else if string(decrypted) != string(payload) {
		t.Errorf("the stored payload decrypts to %.60q, want %.60q", decrypted, payload)
	}

	got, err := store.LoadEventHistory(ctx, runID)
	if err != nil || len(got) != 1 {
		t.Fatalf("load history: %v len=%d", err, len(got))
	}
	if got[0].Request != card {
		t.Errorf("Request round-tripped to %q, want %q", got[0].Request, card)
	}
	if got[0].Response != ssn {
		t.Errorf("Response round-tripped to %q, want %q", got[0].Response, ssn)
	}

	// The checksum is the caller's, computed over the plaintext record before
	// any of this ran, and VerifyWorkflowEvents recomputes it from the
	// DECRYPTED row. Encrypting the payload the caller handed us must not
	// change what that recomputation sees.
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Errorf("VerifyWorkflowEvents on an encrypted call-intent workflow: %v", err)
	}
}
