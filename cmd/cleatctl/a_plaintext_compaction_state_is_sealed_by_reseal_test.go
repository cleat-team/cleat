package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3774: releases through 0.5.0 wrote workflow_payloads.compaction_state in
// the clear with --encrypt-sensitive-payloads on. The fix seals it from then on;
// these tests are about the rows already on disk.
//
// They cannot be nulled -- compaction deletes the events the state summarises from
// event_history in the transaction that writes it, so for a run that is not
// finished the state is the only copy of that history. They are sealed in place,
// by the command that already runs with the key and already walks this table.

const compactionResealTenant = "99999999-9999-9999-9999-999999999999"

const plaintextCompactionState = `{"version": 1, "events": [{"t": 30, "svc": "billing", "resp": "card 4111111111111111"}], "compacted_step": 1}`

func seedCompactedRun(t *testing.T, db *sql.DB, ctx context.Context, wfID, state string) {
	t.Helper()
	seedSixTablesFixture(t, db, ctx, compactionResealTenant, wfID)
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_payloads SET compaction_state = $1::jsonb, compaction_step = 1 WHERE id = $2`, state, wfID); err != nil {
		t.Fatalf("seed compaction_state: %v", err)
	}
}

func markEncryptionEverEnabled(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO payload_encryption_ever_enabled (singleton, enabled_at) VALUES (true, now()) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("mark encryption enabled: %v", err)
	}
}

func readCompactionState(t *testing.T, db *sql.DB, ctx context.Context, wfID string) string {
	t.Helper()
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT compaction_state::text FROM workflow_payloads WHERE id = $1`, wfID).Scan(&raw); err != nil {
		t.Fatalf("read compaction_state: %v", err)
	}
	return raw
}

func compactionResealFixture(t *testing.T) (*sql.DB, context.Context, *engine.PayloadEncryption) {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	enc, err := engine.NewPayloadEncryption(resealKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	return db, context.Background(), enc
}

func TestResealSealsAPlaintextCompactionStateInPlace(t *testing.T) {
	db, ctx, enc := compactionResealFixture(t)
	markEncryptionEverEnabled(t, db, ctx)
	const wf = "compaction-reseal-1"
	seedCompactedRun(t, db, ctx, wf, plaintextCompactionState)

	_, total, err := resealSensitiveTables(ctx, db, enc, false, io.Discard)
	if err != nil {
		t.Fatalf("resealSensitiveTables: %v", err)
	}
	if total.Unreadable != 0 {
		t.Errorf("sweep reported %d unreadable values", total.Unreadable)
	}

	stored := readCompactionState(t, db, ctx, wf)
	if strings.Contains(stored, "4111111111111111") {
		t.Fatalf("the sweep left the plaintext in compaction_state: %s", stored)
	}
	// Opens, bound to its own tenant, to the same JSON; and not for another.
	if !strings.HasPrefix(stored, `"`) {
		t.Fatalf("compaction_state is not a sealed JSON string literal after the sweep: %.80s", stored)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Trim(stored, `"`))
	if err != nil {
		t.Fatalf("stored value is not base64: %v", err)
	}
	plain, form, err := enc.OpenAndClassify(compactionResealTenant, raw)
	if err != nil {
		t.Fatalf("sealed compaction_state does not open: %v", err)
	}
	if form != engine.PayloadFormDerived {
		t.Errorf("sealed form is %s, want derived", form)
	}
	var got, want any
	if err := json.Unmarshal(plain, &got); err != nil {
		t.Fatalf("the sealed value is not JSON: %v", err)
	}
	_ = json.Unmarshal([]byte(plaintextCompactionState), &want)
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if string(gj) != string(wj) {
		t.Errorf("sealed content changed:\n got  %s\n want %s", gj, wj)
	}
	if _, err := enc.Decrypt(resealTenant, raw); err == nil {
		t.Error("the sealed state still opens for a foreign tenant")
	}

	// And it converges: a second sweep finds nothing left to do.
	_, total2, err := resealSensitiveTables(ctx, db, enc, false, io.Discard)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if total2.Older != 0 || total2.Rewrote != 0 {
		t.Errorf("second sweep still found work: Older=%d Rewrote=%d", total2.Older, total2.Rewrote)
	}
}

func TestResealDryRunReportsAPlaintextCompactionStateAndWritesNothing(t *testing.T) {
	db, ctx, enc := compactionResealFixture(t)
	markEncryptionEverEnabled(t, db, ctx)
	const wf = "compaction-reseal-dry"
	seedCompactedRun(t, db, ctx, wf, plaintextCompactionState)
	before := readCompactionState(t, db, ctx, wf)

	_, total, err := resealSensitiveTables(ctx, db, enc, true, io.Discard)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if total.Older != 1 || total.Rewrote != 0 {
		t.Errorf("dry run reported Older=%d Rewrote=%d, want 1 and 0: it must see the work and do none of it", total.Older, total.Rewrote)
	}
	if after := readCompactionState(t, db, ctx, wf); after != before {
		t.Errorf("a dry run changed compaction_state:\n before %s\n after  %s", before, after)
	}
}

// On a database no worker has ever run with the flag on, a plaintext state is just
// how that deployment stores it, and sealing it would leave rows its keyless
// workers cannot read. Known-positive for the marker: the same row IS sealed once
// the marker is set (the first test).
func TestResealLeavesAPlaintextCompactionStateAloneWhereEncryptionWasNeverOn(t *testing.T) {
	db, ctx, enc := compactionResealFixture(t)
	// The marker is permanent by design and CleanupPostgresTestData does not touch
	// it, so another test's marker is still here. Remove it for this test, and put
	// back whatever was there.
	var was bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM payload_encryption_ever_enabled)`).Scan(&was); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM payload_encryption_ever_enabled`); err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	if was {
		t.Cleanup(func() { markEncryptionEverEnabled(t, db, context.Background()) })
	}
	const wf = "compaction-reseal-never"
	seedCompactedRun(t, db, ctx, wf, plaintextCompactionState)
	before := readCompactionState(t, db, ctx, wf)

	if _, _, err := resealSensitiveTables(ctx, db, enc, false, io.Discard); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if after := readCompactionState(t, db, ctx, wf); after != before {
		t.Errorf("the sweep sealed a plaintext state on a database encryption was never on for:\n before %s\n after  %s", before, after)
	}
}

// A state that is already sealed is current: nothing to do, and the bytes do not move.
func TestResealLeavesASealedCompactionStateAlone(t *testing.T) {
	db, ctx, enc := compactionResealFixture(t)
	markEncryptionEverEnabled(t, db, ctx)
	const wf = "compaction-reseal-sealed"
	seedCompactedRun(t, db, ctx, wf, plaintextCompactionState)
	if _, _, err := resealSensitiveTables(ctx, db, enc, false, io.Discard); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	before := readCompactionState(t, db, ctx, wf)

	_, total, err := resealSensitiveTables(ctx, db, enc, false, io.Discard)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if after := readCompactionState(t, db, ctx, wf); after != before {
		t.Error("a sweep over an already sealed compaction_state rewrote it")
	}
	if total.Current < 1 {
		t.Errorf("Current=%d, want the sealed state counted as already current", total.Current)
	}
}

// nonSensitivePayloadColumns names every workflow_payloads column the reseal
// sweep does NOT cover, each with why. A column is either sealed (in the
// sensitiveTableSpecs entry) or named here; a column that is neither fails
// TestEveryWorkflowPayloadsColumnIsSealedOrNamedPlain, which is how cleat#3774
// would have been caught -- compaction_state was added to this table with no one
// asked whether it holds data the flag seals everywhere else.
var nonSensitivePayloadColumns = map[string]string{
	"id":              "the run's id",
	"tenant_id":       "the owning tenant",
	"compacted_at":    "a timestamp",
	"compaction_step": "an integer step number",
	"plugin_vers":     "plugin names and versions the run started under",
	"allowed_signals": "signal NAMES the workflow declared, plain JSON via encodeAllowedSignals (see sensitiveTableSpecs)",
	"def_name":        "the workflow definition's name",
	"def_version":     "the workflow definition's version",
	"trace_id":        "a W3C trace id",
	"completed_at":    "a timestamp",
}

func TestEveryWorkflowPayloadsColumnIsSealedOrNamedPlain(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	rows, err := db.Query(`SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'workflow_payloads' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()

	sealed := map[string]bool{}
	for _, spec := range sensitiveTableSpecs {
		if spec.name == "workflow_payloads" {
			for _, c := range spec.cols {
				sealed[c] = true
			}
		}
	}
	if len(sealed) == 0 {
		t.Fatal("sensitiveTableSpecs has no workflow_payloads entry")
	}

	var all []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		all = append(all, c)
	}
	if len(all) == 0 {
		t.Fatal("read no workflow_payloads columns: the guard would pass vacuously")
	}
	for _, c := range all {
		_, plain := nonSensitivePayloadColumns[c]
		switch {
		case sealed[c] && plain:
			t.Errorf("workflow_payloads.%s is both sealed and listed as non-sensitive", c)
		case !sealed[c] && !plain:
			t.Errorf("workflow_payloads.%s is neither covered by the reseal sweep nor named in nonSensitivePayloadColumns: "+
				"decide which, and if it can hold what --encrypt-sensitive-payloads seals, seal it on write, "+
				"unseal it on read and add it to sensitiveTableSpecs (cleat#3774)", c)
		}
	}
	// A name in either list that is not a column is stale.
	have := map[string]bool{}
	for _, c := range all {
		have[c] = true
	}
	var stale []string
	for c := range sealed {
		if !have[c] {
			stale = append(stale, "sealed:"+c)
		}
	}
	for c := range nonSensitivePayloadColumns {
		if !have[c] {
			stale = append(stale, "plain:"+c)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("names that are not workflow_payloads columns: %v", stale)
	}
}

// THE WINDOW (cleat#3774, found in review). The sweep reads every row, then writes
// each replacement keyed on the primary key. A worker that compacts a run in
// between writes a NEW state and a new compaction_step; a write that does not check
// the value it read lays a sealed copy of the OLD state over it, and the run
// replays without the events compaction deleted. resealBeforeWrite is the hook
// that lets the test be the worker, inside that window.
func TestResealDoesNotOverwriteAStateAWorkerWroteAfterItWasRead(t *testing.T) {
	db, ctx, enc := compactionResealFixture(t)
	markEncryptionEverEnabled(t, db, ctx)
	const wf = "compaction-reseal-race"
	seedCompactedRun(t, db, ctx, wf, plaintextCompactionState)

	// What a compaction after the read writes: a NEWER state, already sealed (a
	// worker on this release seals), and a newer step.
	newPlain := `{"version": 1, "events": [{"t": 30, "svc": "billing", "resp": "NEW-STATE-MARKER"}], "compacted_step": 7}`
	newSealed, err := sealLegacyPlaintextJSON(enc, compactionResealTenant, newPlain)
	if err != nil {
		t.Fatalf("seal the worker's new state: %v", err)
	}
	fired := 0
	resealBeforeWrite = func(spec tableSpec) {
		if spec.name != "workflow_payloads" {
			return
		}
		fired++
		if _, err := db.ExecContext(ctx,
			`UPDATE workflow_payloads SET compaction_state = $1::jsonb, compaction_step = 7 WHERE id = $2`, newSealed, wf); err != nil {
			t.Errorf("the simulated worker's write: %v", err)
		}
	}
	t.Cleanup(func() { resealBeforeWrite = nil })

	_, total, err := resealSensitiveTables(ctx, db, enc, false, io.Discard)
	if err != nil {
		t.Fatalf("a value that changed since it was read must not abort the sweep: %v", err)
	}
	resealBeforeWrite = nil
	if fired != 1 {
		t.Fatalf("the hook fired %d times for workflow_payloads, want 1: the test did not force the interleave", fired)
	}

	var step int
	if err := db.QueryRowContext(ctx, `SELECT compaction_step FROM workflow_payloads WHERE id = $1`, wf).Scan(&step); err != nil {
		t.Fatal(err)
	}
	if step != 7 {
		t.Fatalf("compaction_step = %d, want 7", step)
	}
	if got := readCompactionState(t, db, ctx, wf); got != newSealed {
		t.Errorf("the sweep overwrote the worker's newer state with a copy of the one it had read:\n got  %.120s\n want %.120s", got, newSealed)
	}
	if total.Raced != 1 || total.Rewrote != 0 {
		t.Errorf("Raced=%d Rewrote=%d, want 1 and 0: the value was changed by someone else, so none was rewritten and it is reported", total.Raced, total.Rewrote)
	}

	// The sweep is repeatable: nothing is left that needs it (the new state was
	// already sealed), and the result is clean.
	_, again, err := resealSensitiveTables(ctx, db, enc, false, io.Discard)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again.Raced != 0 || again.Older != 0 || again.Unreadable != 0 {
		t.Errorf("second sweep: Raced=%d Older=%d Unreadable=%d, want all 0", again.Raced, again.Older, again.Unreadable)
	}
}

// And the other half of "run it again": when the worker wrote a PLAINTEXT state in
// the window (a worker still on 0.5.0), the next sweep seals THAT one.
func TestResealSealsThePlaintextStateAWorkerWroteInTheWindowOnTheNextRun(t *testing.T) {
	db, ctx, enc := compactionResealFixture(t)
	markEncryptionEverEnabled(t, db, ctx)
	const wf = "compaction-reseal-race-plain"
	seedCompactedRun(t, db, ctx, wf, plaintextCompactionState)

	newPlain := `{"version": 1, "events": [{"t": 30, "svc": "billing", "resp": "NEWER-PLAIN-MARKER"}], "compacted_step": 7}`
	resealBeforeWrite = func(spec tableSpec) {
		if spec.name != "workflow_payloads" {
			return
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE workflow_payloads SET compaction_state = $1::jsonb, compaction_step = 7 WHERE id = $2`, newPlain, wf); err != nil {
			t.Errorf("the simulated worker's write: %v", err)
		}
	}
	t.Cleanup(func() { resealBeforeWrite = nil })

	_, first, err := resealSensitiveTables(ctx, db, enc, false, io.Discard)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	resealBeforeWrite = nil
	if first.Raced != 1 {
		t.Fatalf("first sweep Raced=%d, want 1", first.Raced)
	}
	if !strings.Contains(readCompactionState(t, db, ctx, wf), "NEWER-PLAIN-MARKER") {
		t.Fatal("the worker's newer plaintext state is gone after the first sweep")
	}

	if _, _, err := resealSensitiveTables(ctx, db, enc, false, io.Discard); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	stored := readCompactionState(t, db, ctx, wf)
	if strings.Contains(stored, "NEWER-PLAIN-MARKER") {
		t.Fatalf("the second sweep left the newer state in the clear: %s", stored)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Trim(stored, `"`))
	if err != nil {
		t.Fatalf("not base64 after the second sweep: %v", err)
	}
	plain, _, err := enc.OpenAndClassify(compactionResealTenant, raw)
	if err != nil || !strings.Contains(string(plain), "NEWER-PLAIN-MARKER") {
		t.Errorf("the sealed state does not open to the newer content: %v %s", err, plain)
	}
}
