package engine

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
)

// cleat#1776: payload ciphertexts are bound to their tenant by GCM AAD.
//
// WHY `err != nil` IS NOT THE ASSERTION, and this is the whole design of the
// file. A wrong-tenant open, a truncated blob, a non-base64 string and a wrong
// key all return non-nil. A test that only checks for an error passes on every
// one of them, so it cannot tell "the binding refused this" from "this input was
// malformed" -- and the second is satisfied by a fix that binds nothing.
//
// So the shape of every negative case here is a PAIR OVER ONE BLOB: the same
// ciphertext, the same key, the same call, opened under two tenants. One
// succeeds and one fails. That difference can only come from the AAD, because
// nothing else varied. The error text is checked as well, so a format error
// cannot pass as a refusal -- but the pair is what carries the proof, and it
// does not depend on Go's unexported errOpen string.

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

// sealWithNilAAD produces a ciphertext in the pre-cleat#1776 format, using the
// same key the PayloadEncryption holds.
//
// Hand-rolled rather than obtained by calling an older API, because there is no
// older API left -- Encrypt now requires a tenant, which is the point. This is
// the only way to build the legacy shape the fallback exists for, and building
// it here means the fallback is tested against a real artefact rather than
// against a mock of one.
func sealWithNilAAD(t *testing.T, keyBase64 string, plaintext []byte) []byte {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("new GCM: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(cryptorand.Reader, nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil)
}

// openWithAAD opens data with exactly one AAD and no fallback.
//
// The production Decrypt deliberately falls back to the legacy nil-AAD form, so
// it cannot answer "is this blob bound?" -- it succeeds either way, which is the
// residual hole itself. These tests need that distinction, and they take it here
// rather than from a no-fallback method in engine/encryption.go: nothing in
// production calls such a method yet, and an exported symbol whose only caller
// is a test falls between the two guards that exist to catch dead code --
// check-dead-exports.sh looks for zero callers anywhere, and
// check-test-only-code.sh runs U1000, which does not report exported
// identifiers in non-main packages at all. The re-seal that eventually needs it
// can export one when it has a real caller.
func openWithAAD(t *testing.T, keyBase64 string, data, aad []byte) ([]byte, error) {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("new GCM: %v", err)
	}
	if len(data) < gcm.NonceSize() {
		t.Fatalf("ciphertext too short: %d", len(data))
	}
	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, aad)
}

// boundTo reports whether data opens with tenantID as AAD and nothing else.
func boundTo(t *testing.T, keyBase64, tenantID string, data []byte) bool {
	t.Helper()
	_, err := openWithAAD(t, keyBase64, data, []byte(tenantID))
	return err == nil
}

// TestACiphertextFromOneTenantDoesNotOpenInAnother is the defect, stated as a
// pair so the result cannot be produced by anything other than the binding.
func TestACiphertextFromOneTenantDoesNotOpenInAnother(t *testing.T) {
	key := validKey(t)
	pe, err := NewPayloadEncryption(key)
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	secret := []byte(`{"card":"4111111111111111"}`)

	blob, err := pe.Encrypt(tenantA, secret)
	if err != nil {
		t.Fatalf("Encrypt as tenant A: %v", err)
	}

	// CONTROL: the blob is good, the key is right, and this call works. Without
	// this the failure below could be a broken fixture.
	back, err := pe.Decrypt(tenantA, blob)
	if err != nil {
		t.Fatalf("UNMEASURED: tenant A cannot open its own ciphertext (%v), so the "+
			"cross-tenant result below says nothing about binding", err)
	}
	if !bytes.Equal(back, secret) {
		t.Fatalf("UNMEASURED: tenant A round-trip returned %q, want %q", back, secret)
	}

	// THE DEFECT: same blob, same key, same method -- only the tenant differs.
	got, err := pe.Decrypt(tenantB, blob)
	if err == nil {
		t.Errorf("tenant B opened tenant A's ciphertext and got %q.\n\n"+
			"The payload is not bound to its tenant: GCM AAD is nil, so a blob moved "+
			"into another tenant's row decrypts to the real plaintext instead of "+
			"failing. Encrypt must pass the tenant id as additional authenticated "+
			"data. cleat#1776.", got)
		return
	}
	// And it failed for the right reason: authentication, not shape.
	if !strings.Contains(err.Error(), "message authentication failed") {
		t.Errorf("tenant B's open failed with %v, which is not an authentication "+
			"failure.\n\nThat means this test is passing on a malformed blob rather "+
			"than on a refused one -- a length check, a base64 error or a wrong key "+
			"would all satisfy `err != nil` here while proving nothing about the "+
			"binding.", err)
	}
}

// TestEveryWrappedFormIsBoundToo covers the three shapes a ciphertext is stored
// in, because the binding lives in Encrypt and every wrapper has to route
// through it.
//
// Not redundant with the test above: these are the methods the stores actually
// call -- EncryptString for the event columns, EncryptJSON for the payload JSONB
// column -- and a wrapper that base64s its own way, or an EncryptJSON that
// called an unbound path, would leave a hole the raw test cannot see.
func TestEveryWrappedFormIsBoundToo(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	t.Run("EncryptString", func(t *testing.T) {
		blob, err := pe.EncryptString(tenantA, "sensitive")
		if err != nil {
			t.Fatalf("EncryptString: %v", err)
		}
		if got, err := pe.DecryptString(tenantA, blob); err != nil || got != "sensitive" {
			t.Fatalf("UNMEASURED: own-tenant round-trip got (%q, %v)", got, err)
		}
		if got, err := pe.DecryptString(tenantB, blob); err == nil {
			t.Errorf("tenant B read tenant A's string field as %q", got)
		}
	})

	t.Run("EncryptJSON", func(t *testing.T) {
		payload := []byte(`{"k":"v"}`)
		blob, err := pe.EncryptJSON(tenantA, payload)
		if err != nil {
			t.Fatalf("EncryptJSON: %v", err)
		}
		if got, err := pe.DecryptJSON(tenantA, blob); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("UNMEASURED: own-tenant round-trip got (%q, %v)", got, err)
		}
		if got, err := pe.DecryptJSON(tenantB, blob); err == nil {
			t.Errorf("tenant B read tenant A's payload column as %q", got)
		}
	})
}

// TestALegacyCiphertextStillOpensAndIsReportedAsUnbound pins BOTH halves of the
// transition, and the second half is the one that keeps the fix honest.
//
// Rows written before cleat#1776 are sealed with nil AAD and no migration can
// re-seal them -- a numbered migration has no key. So Decrypt has to keep
// opening them, which means the hole is still open for exactly those rows. That
// is not something to leave implied: the second half below asserts the legacy
// blob is recognisable AS unbound, using a no-fallback open, so "how many
// unbound blobs are left" is an answerable question and a re-seal has something
// to verify against.
func TestALegacyCiphertextStillOpensAndIsReportedAsUnbound(t *testing.T) {
	key := validKey(t)
	pe, err := NewPayloadEncryption(key)
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	secret := []byte("written before the fix")
	legacy := sealWithNilAAD(t, key, secret)

	// Existing data must stay readable, or the fix is an outage.
	back, err := pe.Decrypt(tenantA, legacy)
	if err != nil {
		t.Fatalf("a pre-cleat#1776 ciphertext no longer decrypts (%v).\n\n"+
			"Every encrypted row written before this change is sealed with nil AAD, "+
			"so dropping the fallback makes them unreadable -- and on most read paths "+
			"that surfaces as ciphertext served as plaintext, not as an error.", err)
	}
	if !bytes.Equal(back, secret) {
		t.Errorf("legacy round-trip got %q, want %q", back, secret)
	}

	// And it is reported as what it is, rather than passing for bound.
	if boundTo(t, key, tenantA, legacy) {
		t.Error("a pre-cleat#1776 ciphertext reports as bound to a tenant.\n\n" +
			"It was sealed with nil AAD, so it cannot be: if this passes, the check " +
			"is not distinguishing the two forms and nothing can count what is left " +
			"to re-seal.")
	}
	// The distinction is not merely that one call failed: the SAME blob opens
	// with nil AAD, which is what makes it legacy rather than corrupt.
	if _, err := openWithAAD(t, key, legacy, nil); err != nil {
		t.Errorf("the legacy blob does not open with nil AAD either (%v), so it is "+
			"malformed and this test is not about the transition at all", err)
	}

	// The control: a BOUND blob is not reported as unbound.
	bound, err := pe.Encrypt(tenantA, secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !boundTo(t, key, tenantA, bound) {
		t.Error("a freshly sealed blob does not report as bound to its own tenant")
	}
	if _, err := openWithAAD(t, key, bound, nil); err == nil {
		t.Error("a bound blob also opens with nil AAD, so the two forms are not " +
			"distinguishable and Decrypt's fallback would accept anything")
	}
}

// TestSealingWithNoTenantIsRefused guards the one way this fix can be present in
// the source and absent in the data.
//
// MEASURED, and it is why this is an error rather than a defaulted empty string:
// GCM treats a zero-length AAD and a nil AAD identically. Seal(..., []byte(""))
// and Seal(..., nil) produce BYTE-IDENTICAL output, and either opens with the
// other. So a caller passing "" writes a blob that is not bound to anything,
// the legacy fallback opens it happily, and every cross-tenant test still
// passes -- because the blob was never bound, not because the binding works.
// There is no later observation that catches it.
func TestSealingWithNoTenantIsRefused(t *testing.T) {
	key := validKey(t)
	pe, err := NewPayloadEncryption(key)
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	for _, c := range []struct {
		name string
		call func() error
	}{
		{"Encrypt", func() error { _, err := pe.Encrypt("", []byte("x")); return err }},
		{"EncryptString", func() error { _, err := pe.EncryptString("", "x"); return err }},
		{"EncryptJSON", func() error { _, err := pe.EncryptJSON("", []byte(`{}`)); return err }},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); !errors.Is(err, ErrNoTenantForEncryption) {
				t.Errorf("%s with an empty tenant returned %v, want ErrNoTenantForEncryption", c.name, err)
			}
		})
	}

	// THE KNOWN-POSITIVE for the claim in the doc comment: prove the two AADs
	// really are indistinguishable, so the refusal above is load-bearing rather
	// than tidiness. If this ever stops holding, the guard can be relaxed.
	legacy := sealWithNilAAD(t, key, []byte("x"))
	if _, err := pe.Decrypt("", legacy); err != nil {
		t.Errorf("a nil-AAD blob did not open under an empty tenant (%v), which would "+
			"mean zero-length and nil AAD are distinguishable after all -- re-check "+
			"ErrNoTenantForEncryption's reasoning before changing it", err)
	}
}

// TestTheUntenantedWritePathBindsToTheTenantTheRowGets covers tenantForAAD.
//
// An empty tenant at a call site is not "no tenant": the untenanted flush path
// (engine/flush.go, an Engine with no tenantID) inserts without setting RLS, so
// the row takes the tenant_id column DEFAULT -- DefaultTenantUUID on all three
// dialects -- while a store reading it defaults its own tenantID to the same
// constant (engine/db.go:78). If the two ends resolved "" differently the write
// would succeed and the read would fail, which on most paths means ciphertext
// served as plaintext rather than an error.
func TestTheUntenantedWritePathBindsToTheTenantTheRowGets(t *testing.T) {
	if got := tenantForAAD(""); got != DefaultTenantUUID {
		t.Errorf("tenantForAAD(\"\") = %q, want %q -- the tenant an untenanted "+
			"insert actually lands in", got, DefaultTenantUUID)
	}
	if got := tenantForAAD(tenantA); got != tenantA {
		t.Errorf("tenantForAAD(%q) = %q, want it unchanged", tenantA, got)
	}

	// And end to end: a blob written through the untenanted path opens for a
	// store that defaults to the same constant, and not for anyone else.
	key := validKey(t)
	pe, err := NewPayloadEncryption(key)
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	blob, err := pe.Encrypt(tenantForAAD(""), []byte("untenanted write"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !boundTo(t, key, tenantForAAD(""), blob) {
		t.Error("the default tenant cannot open what the untenanted path wrote")
	}
	if boundTo(t, key, tenantA, blob) {
		t.Error("tenant A opened a blob written under the default tenant")
	}
}

// TestWhatTheStoreWritesIsBoundToTheWritingTenant is the wiring, not the crypto.
//
// The tests above prove Encrypt binds when it is handed a tenant. They cannot
// prove the WRITE PATH hands it the right one -- EventRecord carries no tenant
// (82 fields, none of them one), so the value travels as a parameter through
// encodeEventForStorage from four different callers, and any of them could pass
// something else, or "" , and every test above would still pass.
//
// So this one reads the bytes off disk and asks the column itself.
//
// PostgreSQL only, and that is a property of the feature rather than of this
// test. THE EVIDENCE MATTERS HERE, because the obvious version of it does not
// work: an empty `git grep 'EncryptString\|EncryptJSON' engine/mysql_events.go
// engine/mssql_events.go` proves nothing at all. Those files are not the write
// path, and both say so in a comment -- "the fields below are encrypted by
// flushEvent" -- so zero hits is what a CORRECT dialect-agnostic design would
// also produce. Reading that absence as "encryption is postgres-only" was an
// invalid inference that happened to reach a true conclusion; WS-1 caught it.
//
// What actually establishes it, three ways over:
//
//	cmd/cleat-worker/main.go:742  --encrypt-sensitive-payloads requires
//	                              --driver=postgres, and the worker exits
//	cmd/cleat-worker/main.go:768  the encryptor is attached behind a type
//	                     and :796 assertion to *engine.PostgresStoreFactory,
//	                              so no other factory can receive one
//	engine/flush.go:167           insertEventSQL is $N placeholders and
//	                              ON CONFLICT DO UPDATE -- postgres syntax, so
//	                              flushEvent could not run elsewhere anyway
//
// and the two stores document their own decrypt blocks as forward-compatibility
// guards: "encryption is not yet supported and will never be true".
func TestWhatTheStoreWritesIsBoundToTheWritingTenant(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	// ONE key, captured. validKey(t) generates a FRESH RANDOM key on every
	// call, so a second call is a different key -- and opening a blob with it
	// fails with "message authentication failed", which is indistinguishable
	// from a binding refusal. A negative assertion written that way passes
	// whether or not the AAD does anything. This cost a real failure while
	// writing the file; the control below is what caught it.
	key := validKey(t)
	enc, err := NewPayloadEncryption(key)
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	store := NewPostgresStore(db).WithEncryption(enc, true)
	eng := NewEngine(nil, nil, WithDB(db), WithEncryption(enc, true), WithTenantID(store.tenantID))
	ctx := context.Background()
	runID := appendChainWorkflow(t, store)

	const secret = `{"card":"4111111111111111"}`
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "payments",
		Op:        "charge",
		Request:   secret,
	}
	if err := eng.flushEvent(ctx, runID, rec, ""); err != nil {
		t.Fatalf("flushEvent: %v", err)
	}

	var stored string
	if err := db.QueryRow(
		`SELECT request FROM event_history WHERE workflow_id = $1 AND step = 0`,
		runID).Scan(&stored); err != nil {
		t.Fatalf("read stored request: %v", err)
	}
	// PRECONDITION: it really is encrypted. With encryption off, every
	// assertion below would be about a plaintext column.
	if stored == secret {
		t.Fatalf("UNMEASURED: request is stored as plaintext, so this test is not "+
			"exercising encryption at all: %q", stored)
	}
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		t.Fatalf("UNMEASURED: stored value is not base64 (%v), so it is not the "+
			"EncryptString output this test assumes: %q", err, stored)
	}

	// CONTROL, and it is the half that proves the write path passed a tenant at
	// all: the bytes on disk open BOUND to the store's own tenant. A write that
	// passed "" would leave the blob unbound, which is precisely the
	// silent-unbinding case ErrNoTenantForEncryption exists to prevent.
	back, err := openWithAAD(t, key, raw, []byte(store.tenantID))
	if err != nil {
		t.Fatalf("what the store wrote is not bound to the tenant that wrote it: %v\n\n"+
			"The ciphertext is on disk and decryptable, but not with this tenant as AAD, "+
			"so encodeEventForStorage was handed the wrong tenant (or an empty one).", err)
	}
	if string(back) != secret {
		t.Fatalf("bound decrypt returned %q, want %q", back, secret)
	}

	// THE ASSERTION: another tenant cannot open it. Same bytes, same key, same
	// call -- only the tenant differs.
	if got, err := openWithAAD(t, key, raw, []byte(tenantB)); err == nil {
		t.Errorf("another tenant opened this row's ciphertext and got %q", got)
	}
	if got, err := enc.Decrypt(tenantB, raw); err == nil {
		t.Errorf("another tenant opened this row's ciphertext through Decrypt and got %q.\n\n"+
			"Decrypt falls back to the legacy nil-AAD form, so this failing means the "+
			"blob is genuinely bound; this succeeding would mean the write path stored "+
			"an unbound ciphertext that the fallback happily reads for anyone.", got)
	}

	// And the ordinary read path still works, which is what stops this being a
	// fix that binds the data and loses it.
	got, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	if len(got) != 1 || got[0].Request != secret {
		t.Errorf("the owning tenant's read path did not return the plaintext: %+v", got)
	}
}

// TestTheEncoderBindsToTheTenantItIsGiven closes the gap the disk test above
// cannot: whether the binding follows the PARAMETER or a constant.
//
// The e2e test runs on a store whose tenant is DefaultTenantUUID, so a write
// path that ignored its tenant and hardcoded the default would pass it. This one
// hands the encoder a tenant that is not the default and asserts the output
// follows, which no constant can satisfy. Together with the four call sites all
// passing tenantForAAD(<their own tenant>) --
// `git grep -n 'encodeEventForStorage(' -- '*.go' | grep -v _test` -- that is the
// whole path from writer to ciphertext.
//
// No database: this is the encoder, and the point is the argument.
func TestTheEncoderBindsToTheTenantItIsGiven(t *testing.T) {
	// One key -- see the note in TestWhatTheStoreWritesIsBoundToTheWritingTenant
	// on why a second validKey(t) call would make every negative here vacuous.
	key := validKey(t)
	enc, err := NewPayloadEncryption(key)
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	const secret = "sensitive request"
	rec := EventRecord{Step: 0, EventType: EventTypeCall, Request: secret, Err: secret}

	stored, err := encodeEventForStorage(rec, enc, true, tenantA)
	if err != nil {
		t.Fatalf("encodeEventForStorage: %v", err)
	}
	// Err is one of the fields sealed through EncryptString; Request goes
	// through tryEncodeBase64 first, so Err is the cleaner probe.
	if stored.Err == secret {
		t.Fatalf("UNMEASURED: err field was stored as plaintext, so encryption did "+
			"not run and nothing below is about binding: %q", stored.Err)
	}
	raw, err := base64.StdEncoding.DecodeString(stored.Err)
	if err != nil {
		t.Fatalf("UNMEASURED: stored err is not base64: %v", err)
	}

	back, err := openWithAAD(t, key, raw, []byte(tenantA))
	if err != nil {
		t.Fatalf("the encoder did not bind to the tenant it was given: %v\n\n"+
			"tenantA was passed in and the output does not open bound to it, so the "+
			"parameter is being dropped or replaced somewhere in encodeEventForStorage.", err)
	}
	if string(back) != secret {
		t.Fatalf("bound decrypt returned %q, want %q", back, secret)
	}
	if got, err := enc.Decrypt(tenantB, raw); err == nil {
		t.Errorf("tenant B opened a field the encoder sealed for tenant A, getting %q", got)
	}
	// And the default tenant specifically, since that is what a hardcoded
	// binding would most plausibly be.
	if got, err := enc.Decrypt(DefaultTenantUUID, raw); err == nil {
		t.Errorf("the DEFAULT tenant opened a field sealed for tenant A, getting %q.\n\n"+
			"That is what a write path binding to a constant rather than to its own "+
			"tenant would produce, and the on-disk test cannot see it because its "+
			"store's tenant IS the default.", got)
	}
}
