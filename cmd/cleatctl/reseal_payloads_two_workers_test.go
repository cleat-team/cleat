package main

import (
	"context"
	"encoding/base64"
	"io"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#1992, second half. engine/payload_key_rotation_test.go proves the
// KEY RING mechanics in isolation: one process, two PayloadEncryption values
// built in sequence. reseal_moves_across_keys_test.go proves the sweep end to
// end against a real database, also from one process.
//
// Neither proves what a ROLLING RESTART actually looks like: two WORKERS,
// each holding its own PayloadEncryption built from its own flags at its own
// boot time, live against the SAME database at once, for however long the
// restart takes. That is the scenario this file is named for -- not two
// PayloadEncryption values in one goroutine, but two independently-configured
// ones each writing and reading the same shared event_history table, which is
// the part of a rotation an operator actually lives through.
//
// It answers the question the rotation doc needs an answer to before it can
// tell an operator what is safe mid-rotation: can the worker that has not yet
// picked up the new key still do its job, and can the worker that has already
// rotated read what the old one wrote? The second question already has a
// covered answer (yes, via the previous key). The FIRST does not, anywhere
// else in the tree, and the answer is no -- which is the fact the doc has to
// state as a caveat, not assume.

const twoWorkersTenant = "77777777-7777-7777-7777-777777777777"

// TestPayloadKeyRotationAcrossTwoWorkers is the rolling-restart scenario:
// workerOld has not been restarted yet and knows only the old key. workerNew
// has already been restarted onto the new deploy -- current = new key,
// previous = old key. Both are live against one database, as they would be
// mid-rollout.
func TestPayloadKeyRotationAcrossTwoWorkers(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	ctx := context.Background()

	oldKeyBase64 := resealKey(t)
	newKeyBase64 := resealKey(t)
	oldKey, err := base64.StdEncoding.DecodeString(oldKeyBase64)
	if err != nil {
		t.Fatalf("decode old key: %v", err)
	}
	newKey, err := base64.StdEncoding.DecodeString(newKeyBase64)
	if err != nil {
		t.Fatalf("decode new key: %v", err)
	}

	// workerOld: has not restarted. This is every worker in the fleet before
	// the rollout begins, and whichever ones are slow to pick up the new
	// deploy while it is in progress.
	workerOld, err := engine.NewPayloadEncryption(oldKeyBase64)
	if err != nil {
		t.Fatalf("NewPayloadEncryption(old): %v", err)
	}

	// workerNew: has restarted with --encryption-key-file <new> and
	// --encryption-key-file-previous <old>. This is the shape the worker flag
	// pair produces once it exists (cleat#1992 part 2); built directly from a
	// KeyRing here because the flag itself lives in cmd/cleat-worker, which
	// this test deliberately does not touch -- the ring is the contract
	// between the flag and the engine, and the flag's own job is only to
	// build one.
	ringNew, err := engine.NewKeyRing(
		engine.VersionedKey{Version: 2, Key: newKey},
		engine.VersionedKey{Version: 1, Key: oldKey},
	)
	if err != nil {
		t.Fatalf("build ring: %v", err)
	}
	workerNew, err := engine.NewPayloadEncryptionWithRing(ringNew)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing: %v", err)
	}

	// workerOld writes before the rollout reaches it -- an ordinary step
	// recorded by a worker still on the old deploy.
	const beforePlain = "written by the not-yet-restarted worker"
	sealedBefore, err := workerOld.Encrypt(twoWorkersTenant, []byte(beforePlain))
	if err != nil {
		t.Fatalf("workerOld seal: %v", err)
	}
	seedResealRow(t, db, ctx, twoWorkersTenant, "two-workers-before-wf",
		map[string]string{"error": base64.StdEncoding.EncodeToString(sealedBefore)})

	// workerNew writes after it has restarted -- the same workflow could just
	// as easily be picked up by either worker next, which is exactly the
	// hazard: nothing routes a workflow's steps back to the worker that wrote
	// the previous one.
	const afterPlain = "written by the already-restarted worker"
	sealedAfter, err := workerNew.Encrypt(twoWorkersTenant, []byte(afterPlain))
	if err != nil {
		t.Fatalf("workerNew seal: %v", err)
	}
	seedResealRow(t, db, ctx, twoWorkersTenant, "two-workers-after-wf",
		map[string]string{"error": base64.StdEncoding.EncodeToString(sealedAfter)})

	// GUARANTEE: the already-restarted worker reads what the not-yet-restarted
	// one wrote. This is the property the previous-key half of the ring
	// exists for, and it is why a rollout does not have to be instantaneous
	// to be safe for old data.
	got, form, err := workerNew.OpenAndClassify(twoWorkersTenant, sealedBefore)
	if err != nil {
		t.Fatalf("workerNew could not read workerOld's row: %v", err)
	}
	if string(got) != beforePlain {
		t.Errorf("workerNew read %q, want %q", got, beforePlain)
	}
	if form != engine.PayloadFormPreviousKey {
		t.Errorf("classified %s, want previous-key", form)
	}

	// THE GAP: the not-yet-restarted worker CANNOT read what the
	// already-restarted one wrote. This is not a bug -- workerOld was never
	// given the new key, so it has no way to -- but it is a real operational
	// fact for the length of a rollout, and nothing in this repo enforces a
	// worker-registry gate for payload keys the way reseal-secrets has for
	// tenant secrets (docs/how-to/use-secrets.md, "You do not have to time
	// this by hand"). If workerOld is asked to replay or debug
	// two-workers-after-wf mid-rollout, it fails. The rotation doc states
	// this as a caveat rather than asserting the rollout is safe outright.
	if _, _, err := workerOld.OpenAndClassify(twoWorkersTenant, sealedAfter); err == nil {
		t.Error("the not-yet-restarted worker opened a value sealed under the new key -- " +
			"either the ring leaked the new key backward, or this scenario no longer applies " +
			"and the rotation doc's caveat needs revisiting")
	}

	// The rollout completes: workerOld restarts onto the same ring workerNew
	// already has. Once every worker is on it, the gap above closes -- both
	// rows are readable by both, because there is only one worker
	// configuration left in the fleet.
	workerOldRestarted, err := engine.NewPayloadEncryptionWithRing(ringNew)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing (workerOld restarted): %v", err)
	}
	if _, _, err := workerOldRestarted.OpenAndClassify(twoWorkersTenant, sealedBefore); err != nil {
		t.Errorf("restarted workerOld could not read the pre-rollout row: %v", err)
	}
	if _, _, err := workerOldRestarted.OpenAndClassify(twoWorkersTenant, sealedAfter); err != nil {
		t.Errorf("restarted workerOld could not read the post-rollout row: %v", err)
	}

	// Once every worker holds the new ring, cleatctl reseal-payloads moves the
	// pre-rollout row off the old key entirely, so the old key can be dropped
	// from the ring on the next deploy without losing it. This is the same
	// production sweep `cleatctl reseal-payloads` runs, not a reimplementation
	// of it.
	st, err := resealPayloads(ctx, db, workerNew, false, io.Discard)
	if err != nil {
		t.Fatalf("resealPayloads: %v", err)
	}
	if st.Older < 1 || st.Rewrote < 1 {
		t.Errorf("resealPayloads stats: Older=%d Rewrote=%d, want at least 1 of each "+
			"(the pre-rollout row)", st.Older, st.Rewrote)
	}

	// After the sweep, the pre-rollout row no longer opens under the old key
	// alone -- it moved, so dropping --encryption-key-file-previous on the
	// next deploy loses nothing.
	oldOnlyAfterSweep, err := engine.NewPayloadEncryption(oldKeyBase64)
	if err != nil {
		t.Fatalf("NewPayloadEncryption(old, post-sweep): %v", err)
	}
	var gotErr string
	if err := db.QueryRowContext(ctx,
		`SELECT "error" FROM event_history WHERE workflow_id = 'two-workers-before-wf' AND step = 0`).
		Scan(&gotErr); err != nil {
		t.Fatalf("read back post-sweep row: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(gotErr)
	if err != nil {
		t.Fatalf("post-sweep value is not base64: %v", err)
	}
	if _, err := oldOnlyAfterSweep.Decrypt(twoWorkersTenant, raw); err == nil {
		t.Error("the pre-rollout row still opens under the old key alone after the sweep -- " +
			"it was made readable under two keys instead of moved to one")
	}
}
