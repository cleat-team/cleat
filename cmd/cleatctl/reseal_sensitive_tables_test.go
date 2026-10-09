package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3241: cleat#2312 extended --encrypt-sensitive-payloads to six tables
// resealpayloads.go's own sweep never learned about. These tests are the
// same property reseal_binds_legacy_ciphertext_test.go checks for
// event_history, run against each of the six -- a value sealed in an OLDER
// form opens only for its own tenant afterward, AND its plaintext survived.
// Either half alone is satisfied by a broken sweep.

const sixTablesTenant = "88888888-8888-8888-8888-888888888888"

// seedSixTablesFixture creates the tenant, def and one workflow_instances row
// that workflow_signals, workflow_promises and workflow_update_requests all
// foreign-key to (migrations/postgres/001_schema.sql's three
// *_workflow_id_fkey constraints). workflow_schedules and idempotency_keys
// carry no such constraint and are seeded independently by their own test.
func seedSixTablesFixture(t *testing.T, db *sql.DB, ctx context.Context, tenant, workflowID string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, tenant, "reseal6-"+tenant[:8]); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id)
		VALUES ($1, 1, '\x00', $2) ON CONFLICT DO NOTHING`, "reseal6-def", tenant); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_instances (id, def_name, def_version, status, tenant_id)
		VALUES ($1, 'reseal6-def', 1, 'ready', $2)`, workflowID, tenant); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
}

// assertNowDerivedAndBoundToOwnTenant is the shared post-sweep check every
// case below runs: the stored value opens under its own tenant in the
// NEWEST form, with the original plaintext, and no longer opens for a
// foreign tenant. Mirrors the checks in
// TestResealBindsLegacyCiphertextAndPreservesThePlaintext, generalised over
// whichever column/table produced `stored`.
func assertNowDerivedAndBoundToOwnTenant(t *testing.T, enc *engine.PayloadEncryption,
	label, tenant, foreignTenant, stored, wantPlain string) {
	t.Helper()
	body := stored
	quoted := strings.HasPrefix(body, `"`) && strings.HasSuffix(body, `"`)
	if quoted {
		body = body[1 : len(body)-1]
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		t.Errorf("%s: stored value is not base64 after the sweep: %v", label, err)
		return
	}
	plain, form, err := enc.OpenAndClassify(tenant, raw)
	if err != nil {
		t.Errorf("%s: does not open at all after the sweep: %v", label, err)
		return
	}
	if form != engine.PayloadFormDerived {
		t.Errorf("%s: is %s after the sweep, want derived", label, form)
	}
	if string(plain) != wantPlain {
		t.Errorf("%s: plaintext changed: got %q, want %q", label, plain, wantPlain)
	}
	if _, err := enc.Decrypt(foreignTenant, raw); err == nil {
		t.Errorf("%s: still opens for a foreign tenant after the sweep", label)
	}
}

// TestResealSensitiveTablesConvertsEachOfTheSix seeds a legacy (nil-AAD)
// ciphertext into one column of every table cleat#2312 added, runs the
// cleat#3241 sweep once, and checks both halves on every table: bound to its
// own tenant now, plaintext unchanged. A second run must find nothing left.
func TestResealSensitiveTablesConvertsEachOfTheSix(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	ctx := context.Background()

	const tenant = sixTablesTenant
	const foreign = resealTenant // reuse reseal_binds_legacy_ciphertext_test.go's second tenant

	key := resealKey(t)
	enc, err := engine.NewPayloadEncryption(key)
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	const wfID = "reseal6-wf-1"
	seedSixTablesFixture(t, db, ctx, tenant, wfID)

	// workflow_instances: input is jsonb (quoted wire form), error_msg is text
	// (bare). Seed one of each so the quoting-preservation path is exercised
	// on this table too, not only on event_history's payload column.
	instInputPlain := `{"order_id":"4111111111111111"}`
	instErrPlain := "boom: card 4111111111111111"
	legacyInput := `"` + sealNilAAD(t, key, instInputPlain) + `"`
	legacyErr := sealNilAAD(t, key, instErrPlain)
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_instances SET input = $1::jsonb, error_msg = $2 WHERE id = $3`,
		legacyInput, legacyErr, wfID); err != nil {
		t.Fatalf("seed workflow_instances columns: %v", err)
	}

	// workflow_signals: PK is a bigint id with no app-supplied value -- let
	// the sequence assign it, and read it back for the post-sweep lookup.
	signalPlain := "signal payload with 4111111111111111"
	legacySignal := `"` + sealNilAAD(t, key, signalPlain) + `"`
	var signalID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id)
		 VALUES ($1, 'reseal-sig', $2::jsonb, $3) RETURNING id`,
		wfID, legacySignal, tenant).Scan(&signalID); err != nil {
		t.Fatalf("seed workflow_signals: %v", err)
	}

	// workflow_promises: PK is (workflow_id, promise_id), both text.
	promisePlain := "promise result 4111111111111111"
	legacyPromise := `"` + sealNilAAD(t, key, promisePlain) + `"`
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, result, tenant_id)
		 VALUES ($1, 'reseal-promise-1', 'p', $2::jsonb, $3)`,
		wfID, legacyPromise, tenant); err != nil {
		t.Fatalf("seed workflow_promises: %v", err)
	}

	// workflow_update_requests: PK is (workflow_id, request_id), both text.
	updatePlain := `{"amount":"4111111111111111"}`
	legacyUpdate := `"` + sealNilAAD(t, key, updatePlain) + `"`
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflow_update_requests (workflow_id, update_name, payload, request_id, tenant_id)
		 VALUES ($1, 'reseal-update', $2::jsonb, 'req-1', $3)`,
		wfID, legacyUpdate, tenant); err != nil {
		t.Fatalf("seed workflow_update_requests: %v", err)
	}

	// workflow_schedules: PK is (tenant_id, name) -- tenant_id doubles as the
	// AAD source, exercising tenantFromPK's reuse path rather than the
	// separate-select path every other table above takes.
	schedulePlain := `{"customer":"4111111111111111"}`
	legacySchedule := `"` + sealNilAAD(t, key, schedulePlain) + `"`
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflow_schedules (name, def_name, cron_expression, input, tenant_id)
		 VALUES ('reseal-sched-1', 'reseal6-def', '* * * * *', $1::jsonb, $2)`,
		legacySchedule, tenant); err != nil {
		t.Fatalf("seed workflow_schedules: %v", err)
	}

	// idempotency_keys: PK is (key_hash, tenant_id), key_hash a bytea --
	// exercising the ::text round trip on a type whose cast is not
	// human-readable (standard \x-prefixed hex).
	idemPlain := "idempotency error with 4111111111111111"
	legacyIdem := sealNilAAD(t, key, idemPlain)
	idemKeyHash := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key_hash, workflow_id, error_msg, tenant_id)
		 VALUES ($1, $2, $3, $4)`,
		idemKeyHash, wfID, legacyIdem, tenant); err != nil {
		t.Fatalf("seed idempotency_keys: %v", err)
	}

	// PRECONDITION, once for the whole fixture: every legacy value above
	// really does open for a FOREIGN tenant before the sweep runs. Without
	// this, the per-table checks after the sweep prove nothing about what
	// changed -- same discipline as
	// TestResealBindsLegacyCiphertextAndPreservesThePlaintext.
	for _, c := range []struct {
		label, legacy string
	}{
		{"workflow_instances.input", legacyInput},
		{"workflow_instances.error_msg", legacyErr},
		{"workflow_signals.payload", legacySignal},
		{"workflow_promises.result", legacyPromise},
		{"workflow_update_requests.payload", legacyUpdate},
		{"workflow_schedules.input", legacySchedule},
		{"idempotency_keys.error_msg", legacyIdem},
	} {
		body := c.legacy
		if strings.HasPrefix(body, `"`) {
			body = body[1 : len(body)-1]
		}
		raw, derr := base64.StdEncoding.DecodeString(body)
		if derr != nil {
			t.Fatalf("UNMEASURED: %s fixture is not base64: %v", c.label, derr)
		}
		if _, err := enc.Decrypt(foreign, raw); err != nil {
			t.Fatalf("UNMEASURED: %s's seeded legacy value does not open for a foreign "+
				"tenant (%v), so it is not the unbound shape this sweep exists to convert",
				c.label, err)
		}
	}

	results, total, err := resealSensitiveTables(ctx, db, enc, false, io.Discard)
	if err != nil {
		t.Fatalf("resealSensitiveTables: %v", err)
	}
	// Seven legacy values: two on workflow_instances, one each on the other
	// five.
	if total.Older != 7 || total.Rewrote != 7 {
		t.Errorf("total: Older=%d Rewrote=%d, want 7 and 7", total.Older, total.Rewrote)
	}
	if total.Unreadable != 0 {
		t.Errorf("total: Unreadable=%d, want 0", total.Unreadable)
	}
	if len(results) != len(sensitiveTableSpecs) {
		t.Errorf("results has %d entries, want %d (one per table)", len(results), len(sensitiveTableSpecs))
	}

	var gotInstInput, gotInstErr string
	if err := db.QueryRowContext(ctx,
		`SELECT input::text, error_msg FROM workflow_instances WHERE id = $1`, wfID).
		Scan(&gotInstInput, &gotInstErr); err != nil {
		t.Fatalf("read back workflow_instances: %v", err)
	}
	assertNowDerivedAndBoundToOwnTenant(t, enc, "workflow_instances.input", tenant, foreign, gotInstInput, instInputPlain)
	assertNowDerivedAndBoundToOwnTenant(t, enc, "workflow_instances.error_msg", tenant, foreign, gotInstErr, instErrPlain)
	if !strings.HasPrefix(gotInstInput, `"`) || !strings.HasSuffix(gotInstInput, `"`) {
		t.Errorf("workflow_instances.input lost its JSON string quoting: %q", gotInstInput)
	}

	var gotSignal string
	if err := db.QueryRowContext(ctx,
		`SELECT payload::text FROM workflow_signals WHERE id = $1`, signalID).Scan(&gotSignal); err != nil {
		t.Fatalf("read back workflow_signals by its real bigint id: %v", err)
	}
	assertNowDerivedAndBoundToOwnTenant(t, enc, "workflow_signals.payload", tenant, foreign, gotSignal, signalPlain)

	var gotPromise string
	if err := db.QueryRowContext(ctx,
		`SELECT result::text FROM workflow_promises WHERE workflow_id = $1 AND promise_id = 'reseal-promise-1'`,
		wfID).Scan(&gotPromise); err != nil {
		t.Fatalf("read back workflow_promises: %v", err)
	}
	assertNowDerivedAndBoundToOwnTenant(t, enc, "workflow_promises.result", tenant, foreign, gotPromise, promisePlain)

	var gotUpdate string
	if err := db.QueryRowContext(ctx,
		`SELECT payload::text FROM workflow_update_requests WHERE workflow_id = $1 AND request_id = 'req-1'`,
		wfID).Scan(&gotUpdate); err != nil {
		t.Fatalf("read back workflow_update_requests: %v", err)
	}
	assertNowDerivedAndBoundToOwnTenant(t, enc, "workflow_update_requests.payload", tenant, foreign, gotUpdate, updatePlain)

	var gotSchedule string
	if err := db.QueryRowContext(ctx,
		`SELECT input::text FROM workflow_schedules WHERE tenant_id = $1 AND name = 'reseal-sched-1'`,
		tenant).Scan(&gotSchedule); err != nil {
		t.Fatalf("read back workflow_schedules: %v", err)
	}
	assertNowDerivedAndBoundToOwnTenant(t, enc, "workflow_schedules.input", tenant, foreign, gotSchedule, schedulePlain)

	var gotIdem string
	if err := db.QueryRowContext(ctx,
		`SELECT error_msg FROM idempotency_keys WHERE key_hash = $1 AND tenant_id = $2`,
		idemKeyHash, tenant).Scan(&gotIdem); err != nil {
		t.Fatalf("read back idempotency_keys by its real bytea key_hash: %v", err)
	}
	assertNowDerivedAndBoundToOwnTenant(t, enc, "idempotency_keys.error_msg", tenant, foreign, gotIdem, idemPlain)

	// Idempotent: a second sweep finds nothing left, across all six tables.
	_, total2, err := resealSensitiveTables(ctx, db, enc, false, io.Discard)
	if err != nil {
		t.Fatalf("second resealSensitiveTables: %v", err)
	}
	if total2.Older != 0 || total2.Rewrote != 0 {
		t.Errorf("second sweep still found work: Older=%d Rewrote=%d", total2.Older, total2.Rewrote)
	}
}

// TestResealSensitiveTablesDryRunWritesNothing is the --dry-run contract,
// checked against this sweep the same way resealpayloads.go's own dry-run
// path is: it must report the work without performing it.
func TestResealSensitiveTablesDryRunWritesNothing(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	ctx := context.Background()

	const tenant = sixTablesTenant
	key := resealKey(t)
	enc, err := engine.NewPayloadEncryption(key)
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	const wfID = "reseal6-dryrun-wf"
	seedSixTablesFixture(t, db, ctx, tenant, wfID)

	plain := "dry run plaintext 4111111111111111"
	legacy := sealNilAAD(t, key, plain)
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_instances SET error_msg = $1 WHERE id = $2`, legacy, wfID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, total, err := resealSensitiveTables(ctx, db, enc, true, io.Discard)
	if err != nil {
		t.Fatalf("resealSensitiveTables (dry-run): %v", err)
	}
	if total.Older != 1 {
		t.Errorf("Older=%d, want 1", total.Older)
	}

	var stillLegacy string
	if err := db.QueryRowContext(ctx,
		`SELECT error_msg FROM workflow_instances WHERE id = $1`, wfID).Scan(&stillLegacy); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stillLegacy != legacy {
		t.Errorf("--dry-run wrote a change: got %q, want the untouched legacy value %q", stillLegacy, legacy)
	}
}
