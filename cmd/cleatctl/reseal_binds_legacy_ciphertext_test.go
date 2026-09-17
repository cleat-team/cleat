package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#1794: `cleatctl reseal-payloads` converts the ciphertexts #1792 could
// not -- the ones already on disk, sealed with nil AAD and therefore bound to
// no tenant.
//
// The property under test is not "the command ran". It is that a value which
// opened for ANY tenant before now opens only for its own, AND that its
// plaintext survived. Either half alone is satisfied by a broken sweep: one
// that skipped everything preserves plaintext perfectly, and one that wrote
// garbage binds it beautifully.

const (
	resealTenant = "44444444-4444-4444-4444-444444444444"
	otherTenant  = "55555555-5555-5555-5555-555555555555"
)

// sealNilAAD produces a pre-cleat#1776 ciphertext: the stored form, base64, with
// no tenant binding.
//
// Hand-rolled because there is no API left that can produce one -- Encrypt now
// requires a tenant, which is the fix. Building the legacy shape by hand is the
// only way the conversion is tested against a real artefact rather than a mock.
func sealNilAAD(t *testing.T, keyBase64 string, plaintext string) string {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), nil))
}

func resealKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		t.Fatalf("key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// seedResealRow creates the tenant, definition, instance and one event row.
func seedResealRow(t *testing.T, db *sql.DB, ctx context.Context,
	tenant, workflowID string, cols map[string]string) {
	t.Helper()
	_, _ = db.ExecContext(ctx, `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, tenant, "reseal-"+tenant[:8])
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id)
		VALUES ($1, 1, '\x00', $2) ON CONFLICT DO NOTHING`, "reseal-def", tenant); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_instances (id, def_name, def_version, status, tenant_id)
		VALUES ($1, 'reseal-def', 1, 'ready', $2)`, workflowID, tenant); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}

	names := []string{"workflow_id", "step", "tenant_id"}
	args := []any{workflowID, 0, tenant}
	ph := []string{"$1", "$2", "$3"}
	i := 4
	for c, v := range cols {
		names = append(names, `"`+c+`"`)
		args = append(args, v)
		ph = append(ph, "$"+strconv.Itoa(i))
		i++
	}
	q := "INSERT INTO event_history (" + strings.Join(names, ", ") + ") VALUES (" +
		strings.Join(ph, ", ") + ")"
	if _, err := db.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("seed event_history: %v", err)
	}
}

// TestResealBindsLegacyCiphertextAndPreservesThePlaintext is the whole point of
// the command, asserted on both halves at once.
func TestResealBindsLegacyCiphertextAndPreservesThePlaintext(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	ctx := context.Background()

	key := resealKey(t)
	enc, err := engine.NewPayloadEncryption(key)
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	const errPlain = "boom: card 4111111111111111"
	const payloadPlain = `{"step":0,"card":"4111111111111111"}`

	// `error` is a plain base64 column; `payload` is JSONB and holds the same
	// base64 WRAPPED IN QUOTES, because EncryptJSON writes a JSON string
	// literal. Both forms are seeded so the conversion is tested against both,
	// and so that writing a Go string back into a jsonb column is exercised
	// rather than assumed to work.
	legacyErr := sealNilAAD(t, key, errPlain)
	legacyPayload := `"` + sealNilAAD(t, key, payloadPlain) + `"`

	// The control: a value ALREADY bound to this tenant. A sweep that rewrote
	// everything unconditionally would also pass the assertions above, and this
	// is what stops it.
	boundRaw, err := enc.Encrypt(resealTenant, []byte("already bound"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	boundChild := base64.StdEncoding.EncodeToString(boundRaw)

	// And a column holding PLAINTEXT, which must not be double-encrypted.
	const plaintextCol = "this was never encrypted"

	seedResealRow(t, db, ctx, resealTenant, "reseal-wf-1", map[string]string{
		"error":       legacyErr,
		"payload":     legacyPayload,
		"child_input": boundChild,
		"new_input":   plaintextCol,
	})

	// PRECONDITION: the legacy value really is readable by the WRONG tenant
	// before the sweep. That is the defect; without it, the assertion after the
	// sweep proves nothing about what changed.
	rawBefore, derr := base64.StdEncoding.DecodeString(legacyErr)
	if derr != nil {
		t.Fatalf("UNMEASURED: fixture is not base64: %v", derr)
	}
	if _, err := enc.Decrypt(otherTenant, rawBefore); err != nil {
		t.Fatalf("UNMEASURED: the seeded legacy value does not open for a foreign "+
			"tenant (%v), so it is not the unbound shape this command exists to "+
			"convert and the post-sweep check is vacuous", err)
	}

	st, err := resealPayloads(ctx, db, enc, false, io.Discard)
	if err != nil {
		t.Fatalf("resealPayloads: %v", err)
	}
	if st.Legacy != 2 || st.Rewrote != 2 {
		t.Errorf("stats: Legacy=%d Rewrote=%d, want 2 and 2", st.Legacy, st.Rewrote)
	}
	if st.Bound != 1 {
		t.Errorf("stats: Bound=%d, want 1 (the already-bound child_input)", st.Bound)
	}
	if st.NotCipher != 1 {
		t.Errorf("stats: NotCipher=%d, want 1 (the plaintext new_input)", st.NotCipher)
	}
	if st.Unreadable != 0 {
		t.Errorf("stats: Unreadable=%d, want 0", st.Unreadable)
	}

	var gotErr, gotPayload, gotChild, gotNew string
	if err := db.QueryRowContext(ctx,
		`SELECT "error", payload::text, child_input, new_input
		   FROM event_history WHERE workflow_id = 'reseal-wf-1' AND step = 0`).
		Scan(&gotErr, &gotPayload, &gotChild, &gotNew); err != nil {
		t.Fatalf("read back: %v", err)
	}

	// THE FIX: bound to its own tenant now, and NOT readable by another.
	for _, c := range []struct {
		name, stored, want string
	}{
		{"error", gotErr, errPlain},
		{"payload", gotPayload, payloadPlain},
	} {
		body := c.stored
		if strings.HasPrefix(body, `"`) && strings.HasSuffix(body, `"`) {
			body = body[1 : len(body)-1]
		}
		raw, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			t.Errorf("%s: stored value is not base64 after the sweep: %v", c.name, err)
			continue
		}
		back, err := enc.DecryptTenantBound(resealTenant, raw)
		if err != nil {
			t.Errorf("%s: not bound to its own tenant after the sweep: %v", c.name, err)
			continue
		}
		if string(back) != c.want {
			t.Errorf("%s: plaintext changed: got %q, want %q", c.name, back, c.want)
		}
		if _, err := enc.Decrypt(otherTenant, raw); err == nil {
			t.Errorf("%s: still opens for a foreign tenant after the sweep", c.name)
		}
	}

	// payload must still be a JSON string literal, or the column no longer
	// round-trips through the read path.
	if !strings.HasPrefix(gotPayload, `"`) || !strings.HasSuffix(gotPayload, `"`) {
		t.Errorf("payload lost its JSON string quoting: %q", gotPayload)
	}
	// The untouched ones, byte for byte.
	if gotChild != boundChild {
		t.Errorf("an already-bound value was rewritten: got %q", gotChild)
	}
	if gotNew != plaintextCol {
		t.Errorf("a plaintext value was rewritten: got %q", gotNew)
	}

	// Idempotent: a second sweep finds nothing left.
	st2, err := resealPayloads(ctx, db, enc, false, io.Discard)
	if err != nil {
		t.Fatalf("second resealPayloads: %v", err)
	}
	if st2.Legacy != 0 || st2.Rewrote != 0 {
		t.Errorf("second sweep still found work: Legacy=%d Rewrote=%d", st2.Legacy, st2.Rewrote)
	}
}

// TestResealRefusesAnRLSRestrictedConnection pins the failure mode of pointing
// this command at the wrong kind of role.
//
// printUsage requires --db to name a role row-level security does not apply to.
// On one it does apply to, the sweep must FAIL rather than convert whatever
// subset it can see and report success -- a partial sweep reporting clean is
// indistinguishable from a finished one, and it is the reason this command reads
// tenant_id off the row instead of iterating a tenant list.
func TestResealRefusesAnRLSRestrictedConnection(t *testing.T) {
	superDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer superDB.Close()
	testutil.SetupFullSchema(t, superDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, superDB)
	ctx := context.Background()

	key := resealKey(t)
	enc, err := engine.NewPayloadEncryption(key)
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	seedResealRow(t, superDB, ctx, resealTenant, "reseal-wf-rls", map[string]string{
		"error": sealNilAAD(t, key, "secret"),
	})

	appDB := testutil.OpenPostgresRLSTestDB(t, superDB)
	defer appDB.Close()

	// CONTROL: the superuser connection CAN do the sweep, so a failure below is
	// about the role and not about the fixture.
	if st, err := resealPayloads(ctx, superDB, enc, true, io.Discard); err != nil || st.Legacy != 1 {
		t.Fatalf("UNMEASURED: the privileged connection could not see the work "+
			"(err=%v Legacy=%d), so the refusal below says nothing about RLS", err, st.Legacy)
	}

	if _, err := resealPayloads(ctx, appDB, enc, true, io.Discard); err == nil {
		t.Error("the sweep reported success on a connection row-level security applies to.\n\n" +
			"cleat.assert_tenant_set() is written to RAISE when no tenant is set rather " +
			"than filter to nothing, so this should have failed. Reporting success here " +
			"means an operator who points --db at an application role gets a clean run " +
			"and an unconverted database.")
	}
}
