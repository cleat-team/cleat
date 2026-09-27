package main

import (
	"context"
	"encoding/base64"
	"io"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#1992. Before this, `cleatctl reseal-payloads` could upgrade a value's
// FORM under one key (reseal_binds_legacy_ciphertext_test.go covers that) but
// had no way to move a value off a retired KEY, because it only ever built a
// PayloadEncryption around a single key. This is the acceptance criterion from
// the issue: "payloads written under key A read correctly with A as previous
// and B as current; reseal-payloads moves them to B; removing A afterward
// leaves everything readable."

const (
	rotateTenant = "66666666-6666-6666-6666-666666666666"
)

func rotationKey(t *testing.T) string {
	t.Helper()
	return resealKey(t) // reuse reseal_binds_legacy_ciphertext_test.go's helper
}

// TestResealMovesValuesFromAPreviousKeyToTheCurrentOne is the whole acceptance
// criterion, run end to end against a real database.
func TestResealMovesValuesFromAPreviousKeyToTheCurrentOne(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	ctx := context.Background()

	keyABase64 := rotationKey(t)
	keyBBase64 := rotationKey(t)
	keyA, err := base64.StdEncoding.DecodeString(keyABase64)
	if err != nil {
		t.Fatalf("decode key A: %v", err)
	}
	keyB, err := base64.StdEncoding.DecodeString(keyBBase64)
	if err != nil {
		t.Fatalf("decode key B: %v", err)
	}

	// Step 1: everything written under key A, before any rotation.
	encAOnly, err := engine.NewPayloadEncryption(keyABase64)
	if err != nil {
		t.Fatalf("NewPayloadEncryption(A): %v", err)
	}
	const plain = "card 4111111111111111, written under key A"
	sealedUnderA, err := encAOnly.Encrypt(rotateTenant, []byte(plain))
	if err != nil {
		t.Fatalf("seal under A: %v", err)
	}

	seedResealRow(t, db, ctx, rotateTenant, "rotate-wf-1", map[string]string{
		"error": base64.StdEncoding.EncodeToString(sealedUnderA),
	})

	// PRECONDITION: a PayloadEncryption that only knows key B cannot read it
	// yet -- otherwise the sweep below proves nothing about moving the key.
	encBOnly, err := engine.NewPayloadEncryption(keyBBase64)
	if err != nil {
		t.Fatalf("NewPayloadEncryption(B): %v", err)
	}
	if _, err := encBOnly.Decrypt(rotateTenant, sealedUnderA); err == nil {
		t.Fatal("UNMEASURED: key B alone already opens a value sealed under key A -- " +
			"the two keys collided, and the sweep below would prove nothing")
	}

	// Step 2: deploy with B current, A previous -- the ring reseal-payloads
	// would build from --encryption-key-file B --from-key-file A.
	ring, err := engine.NewKeyRing(
		engine.VersionedKey{Version: 2, Key: keyB},
		engine.VersionedKey{Version: 1, Key: keyA},
	)
	if err != nil {
		t.Fatalf("build ring: %v", err)
	}
	encBA, err := engine.NewPayloadEncryptionWithRing(ring)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing: %v", err)
	}

	// The row is readable mid-rotation, before reseal has run at all.
	before, form, err := encBA.OpenAndClassify(rotateTenant, sealedUnderA)
	if err != nil {
		t.Fatalf("did not open under the B-current/A-previous ring: %v", err)
	}
	if string(before) != plain {
		t.Errorf("plaintext = %q, want %q", before, plain)
	}
	if form != engine.PayloadFormPreviousKey {
		t.Errorf("classified %s, want previous-key", form)
	}

	// Step 3: run the sweep.
	st, err := resealPayloads(ctx, db, encBA, false, io.Discard)
	if err != nil {
		t.Fatalf("resealPayloads: %v", err)
	}
	if st.Older != 1 || st.Rewrote != 1 {
		t.Errorf("stats: Older=%d Rewrote=%d, want 1 and 1", st.Older, st.Rewrote)
	}
	if st.Unreadable != 0 {
		t.Errorf("stats: Unreadable=%d, want 0", st.Unreadable)
	}

	var gotErr string
	if err := db.QueryRowContext(ctx,
		`SELECT "error" FROM event_history WHERE workflow_id = 'rotate-wf-1' AND step = 0`).
		Scan(&gotErr); err != nil {
		t.Fatalf("read back: %v", err)
	}
	raw, derr := base64.StdEncoding.DecodeString(gotErr)
	if derr != nil {
		t.Fatalf("stored value is not base64 after the sweep: %v", derr)
	}

	// Step 4: the acceptance criterion's last clause -- remove A from the
	// ring entirely, and the row is STILL readable, because reseal moved it
	// to B rather than merely learning to also read A.
	pt, form, err := encBOnly.OpenAndClassify(rotateTenant, raw)
	if err != nil {
		t.Fatalf("does not open under key B ALONE after the sweep (A removed): %v", err)
	}
	if form != engine.PayloadFormDerived {
		t.Errorf("classified %s under key B alone, want derived", form)
	}
	if string(pt) != plain {
		t.Errorf("plaintext changed: got %q, want %q", pt, plain)
	}

	// And key A alone can no longer read it -- the value actually MOVED, it
	// was not merely made openable by a second key.
	if _, err := encAOnly.Decrypt(rotateTenant, raw); err == nil {
		t.Error("the resealed value still opens under key A alone -- it was not moved, " +
			"only made readable under two keys")
	}

	// Idempotent: a second sweep under the same B-current/A-previous ring
	// finds nothing left to do.
	st2, err := resealPayloads(ctx, db, encBA, false, io.Discard)
	if err != nil {
		t.Fatalf("second resealPayloads: %v", err)
	}
	if st2.Older != 0 || st2.Rewrote != 0 {
		t.Errorf("second sweep still found work: Older=%d Rewrote=%d", st2.Older, st2.Rewrote)
	}
}
