package crash

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeEncryptionKeyFile writes a fresh random 32-byte AES-256 key,
// base64-encoded, and returns its path -- the format
// cmd/cleat-worker/setup.go's loadPayloadEncryption expects.
func writeEncryptionKeyFile(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "key.b64")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

// writeShardsFile writes a one-shard --shards-file config pointing at
// connStr, in the format cmd/cleat-worker/setup.go's loadShardConfigs
// expects. A local struct, not engine.ShardConfig: this package deliberately
// does not import engine (see harness_test.go's comment on defaultTenant),
// so this is the same trade harness_test.go already makes.
func writeShardsFile(t *testing.T, connStr string) string {
	t.Helper()
	type shardConfig struct {
		Name    string `json:"name"`
		ConnStr string `json:"conn_str"`
	}
	data, err := json.Marshal([]shardConfig{{Name: "shard-0", ConnStr: connStr}})
	if err != nil {
		t.Fatalf("marshal shards config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "shards.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write shards file: %v", err)
	}
	return path
}

// startShardedWorker launches the worker in --shards-file mode against a
// single shard. It cannot reuse startWorkerOn: that helper always passes
// --db, and cmd/cleat-worker/main.go's sharded branch (main.go:811) never
// reads --db at all -- it builds its runtime pool from the shard config's
// own conn_str instead, which is the entire reason the shadowed-variable bug
// (cleat#2305) had no test that could see it: nothing in this package
// started a worker this way before this file.
func startShardedWorker(t *testing.T, bin, shardsFile, taskQueue, svcURL string, extraFlags ...string) *worker {
	t.Helper()
	w := &worker{log: &strings.Builder{}}
	args := []string{
		"--shards-file", shardsFile,
		"--migrate-db", ensureCrashDatabase(t),
		"--task-queue", taskQueue,
		"--bench-svc-url", svcURL,
		"--poll", "200ms",
		"--concurrency", "1",
		"--migrate-on-start",
	}
	args = append(args, extraFlags...)
	//nolint:gosec // bin is built by this test from this repo.
	cmd := exec.Command(bin, args...)
	cmd.Dir = repoRoot(t)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = &syncWriter{w: w.log, mu: &w.mu}
	cmd.Stderr = &syncWriter{w: w.log, mu: &w.mu}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the sharded worker: %v", err)
	}
	w.cmd = cmd
	t.Cleanup(func() { w.kill() })
	return w
}

// eventTextContains reports whether any request/response column of id's
// event_history rows carries marker in plain text.
//
// request and response are stored as exactly one layer of base64 whether or
// not encryption is on -- base64(plaintext) unencrypted, base64(ciphertext)
// encrypted (engine/event_storage_encoding.go's encodeEventForStorage: "not
// as base64 of base64"). A raw substring check against the column text can
// never match either way, since the marker itself is never base64-aligned in
// a way that survives encoding -- this was caught by this file's own
// known-positive control failing the first time it ran, against an
// unencrypted row.
func eventTextContains(t *testing.T, db *sql.DB, id, marker string) bool {
	t.Helper()
	rows, err := db.Query(`
		SELECT COALESCE(request, ''), COALESCE(response, '')
		FROM event_history WHERE workflow_id = $1`, id)
	if err != nil {
		t.Fatalf("querying event_history for %s: %v", id, err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var req, resp string
		if err := rows.Scan(&req, &resp); err != nil {
			t.Fatalf("scanning event_history row for %s: %v", id, err)
		}
		if base64ContainsMarker(req, marker) || base64ContainsMarker(resp, marker) {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating event_history for %s: %v", id, err)
	}
	return found
}

// base64ContainsMarker decodes one layer of base64 and looks for marker in
// the result. A column that fails to decode (should not happen for
// request/response, but this is a test, not the production read path) is
// treated as not containing the marker rather than failing the test --
// eventTextContains' callers only care about a plain-text leak, and an
// undecodable column cannot be one.
func base64ContainsMarker(encoded, marker string) bool {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	return strings.Contains(string(decoded), marker)
}

// TestPayloadEncryptionKeyRotationWiresIntoARealWorker pins cleat#1992's
// --encryption-key-file-previous flag to a REAL cleat-worker process, not
// just the loader function
// cmd/cleat-worker/payload_encryption_flags_test.go unit-tests in isolation.
// cleat-review found the gap directly: passing "" for both encryption flags
// at all three of main.go's call sites left every existing test green,
// because nothing exercised the compiled binary's own flag parsing and
// wiring end to end (#2308).
//
// worker A runs under --encryption-key-file <keyA> alone and records Reserve
// durably before being killed with Charge held in flight -- the same
// crash-window technique TestCrashMidDurableCallReplaysOnlyTheUnrecordedStep
// uses. worker B then starts under --encryption-key-file <keyB>
// --encryption-key-file-previous <keyA> -- the rolled configuration
// docs/how-to/rotate-payload-encryption-key.md describes -- and must decrypt
// Reserve's keyA-sealed event during replay to resume the workflow at all.
//
// If the previous-key flag were silently dropped (the bug this test would
// have caught before the fix: the call sites only invoked
// loadPayloadEncryption inside `if *encryptionKeyFile != ""`, so a
// previous-only argument never reached it), worker B would build a ring
// holding only keyB. Decrypting Reserve's event_history row would then fail
// AEAD verification, and -- per cleat-review's separate measurement, with
// checksums on (the default, and this test's configuration) -- the workflow
// ends FAILED rather than completing.
func TestPayloadEncryptionKeyRotationWiresIntoARealWorker(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()

	suffix := uniqueSuffix()
	taskQueue := "queue-payload-rotation-" + suffix
	wfID := "payload-rotation-wf-" + suffix
	marker := "order-rotation-marker-" + suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)

	svc := newChargeService(t)
	releaseCharge := svc.holdOperation("Charge")
	// release funcs are sync.Once-wrapped (idempotent), so deferring them
	// immediately is safe even though each is also called explicitly below on
	// the success path: a t.Fatalf between here and that explicit call would
	// otherwise skip it and leave a held connection open, which
	// httptest.Server.Close() (run by t.Cleanup) waits on forever -- exactly
	// what hung this test's first version for the length of its go test
	// timeout instead of failing fast.
	defer releaseCharge()

	keyA := writeEncryptionKeyFile(t)
	first := startWorker(t, bin, taskQueue, svc.srv.URL,
		"--encrypt-sensitive-payloads", "--encryption-key-file", keyA)
	startWorkflow(t, db, wfID, marker, taskQueue)

	// Wait until Charge has actually reached the service. Reserve's count
	// increments synchronously in the handler before Charge's does (see
	// chargeService.handle), so by the time Charge is held, Reserve is done
	// and durable.
	svc.awaitHeldCall(t, first, startBudget)
	if r, c, _ := svc.allCounts(); r != 1 || c != 1 {
		t.Fatalf("before the crash: Reserve=%d Charge=%d, want 1/1 -- "+
			"the workflow did not reach the held call cleanly", r, c)
	}

	// Reserve is durable now, sealed under keyA. Checked HERE, while the
	// workflow is still "running": finalize_workflow_status deletes a
	// terminal workflow's event_history (003_procedures.sql), so this check
	// is only meaningful before the workflow completes -- see
	// TestPayloadEncryptionWiringKnownPositive's own history with this.
	if eventTextContains(t, db, wfID, marker) {
		t.Fatalf("event_history for %s carries %q in plain text with "+
			"--encryption-key-file set -- the flag never reached the store", wfID, marker)
	}

	// The crash: worker A dies holding an HTTP response it never received.
	first.kill()
	releaseCharge()

	// Held BEFORE worker B starts, on the op after the one that will be
	// retried: holdOperation only holds an op's FIRST invocation, and
	// Charge's first invocation is the one just released above, so it must be
	// Ship's turn to pause the workflow this time -- which happens only once
	// worker B's retried Charge has succeeded and been recorded under keyB.
	releaseShip := svc.holdOperation("Ship")
	defer releaseShip() // see the comment on the earlier defer releaseCharge()

	keyB := writeEncryptionKeyFile(t)
	second := startWorker(t, bin, taskQueue, svc.srv.URL,
		"--encrypt-sensitive-payloads", "--encryption-key-file", keyB,
		"--encryption-key-file-previous", keyA)

	// Reaching this point at all requires worker B to have decrypted
	// Reserve's keyA-sealed event during replay -- a ring holding only keyB
	// (the silently-dropped-previous-flag bug this test exists to catch)
	// fails that decrypt, and per cleat-review's measurement (checksums on,
	// the default here) the workflow ends FAILED rather than reaching Ship.
	svc.awaitHeldCall(t, second, startBudget)

	// Charge's retry is durable now, sealed under keyB -- confirms the
	// CURRENT key half of the rotation reached the store too, not only the
	// previous-key read.
	if eventTextContains(t, db, wfID, marker) {
		t.Fatalf("event_history for %s carries %q in plain text after "+
			"Charge's retry under the rolled worker -- the current key never "+
			"reached the store", wfID, marker)
	}
	releaseShip()

	status, errMsg := awaitTerminal(t, db, wfID, completeBudget)
	if status != "done" && status != "completed" {
		t.Fatalf("workflow ended %q (%s) after rolling onto the new key\n--- worker log ---\n%s",
			status, errMsg, second.output())
	}

	reserve, charge, ship := svc.allCounts()
	t.Logf("after rotation: Reserve=%d Charge=%d Ship=%d", reserve, charge, ship)
	if reserve != 1 {
		t.Errorf("Reserve=%d, want 1 -- it was durable before the crash and "+
			"should not have replayed, which independently confirms worker B "+
			"decrypted its keyA-sealed event rather than re-running it", reserve)
	}
	if charge != 2 {
		t.Errorf("Charge=%d, want 2 -- it was interrupted in flight and should "+
			"have been retried exactly once (at-least-once)", charge)
	}
	if ship != 1 {
		t.Errorf("Ship=%d, want 1 -- it should have run cleanly, once, after Charge's retry", ship)
	}
}

// TestPayloadEncryptionWiringKnownPositive is the negative control
// TestPayloadEncryptionKeyRotationWiresIntoARealWorker's eventTextContains
// assertions need: CLAUDE.md's falsification discipline requires a check
// that "no plaintext found" is not ambiguous between "genuinely encrypted"
// and "the query looks in the wrong place". A worker started with neither
// encryption flag must leave the marker in plain text in event_history --
// if it does not, eventTextContains cannot be trusted anywhere in this file.
func TestPayloadEncryptionWiringKnownPositive(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()

	suffix := uniqueSuffix()
	taskQueue := "queue-payload-rotation-control-" + suffix
	wfID := "payload-rotation-control-wf-" + suffix
	marker := "order-rotation-control-marker-" + suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	release := svc.holdOperation("Charge")
	defer release() // see TestPayloadEncryptionKeyRotationWiresIntoARealWorker's comment on this pattern

	w := startWorker(t, bin, taskQueue, svc.srv.URL) // no encryption flags at all
	startWorkflow(t, db, wfID, marker, taskQueue)

	// Checked while the workflow is still "running", not after it completes:
	// finalize_workflow_status deletes a terminal workflow's event_history
	// (003_procedures.sql), so a check made after awaitTerminal would find no
	// rows at all and pass regardless of encryption -- caught by this test
	// itself failing the first time it was run, against exactly that mistake.
	svc.awaitHeldCall(t, w, startBudget)
	if !eventTextContains(t, db, wfID, marker) {
		dumpEventHistory(t, db, wfID)
		t.Fatalf("event_history for %s does NOT carry %q in plain text with no "+
			"encryption flags set -- eventTextContains cannot detect plain text, "+
			"so its absence in the rotation test proves nothing", wfID, marker)
	}
	release()

	status, errMsg := awaitTerminal(t, db, wfID, completeBudget)
	if status != "done" && status != "completed" {
		t.Fatalf("workflow ended %q (%s)\n--- worker log ---\n%s", status, errMsg, w.output())
	}
}

func dumpEventHistory(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	rows, err := db.Query(`
		SELECT step, event_type, COALESCE(service, ''), COALESCE(operation, ''),
			COALESCE(request, ''), COALESCE(response, ''), COALESCE(payload::text, '')
		FROM event_history WHERE workflow_id = $1 ORDER BY step`, id)
	if err != nil {
		t.Logf("dumpEventHistory: query: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var step int
		var eventType, service, operation, request, response, payload string
		if err := rows.Scan(&step, &eventType, &service, &operation, &request, &response, &payload); err != nil {
			t.Logf("dumpEventHistory: scan: %v", err)
			return
		}
		t.Logf("step=%d type=%s service=%s op=%s request=%q response=%q payload=%q",
			step, eventType, service, operation, request, response, payload)
	}
}

// TestEncryptionKeyFilePreviousWithoutCurrentIsRefusedByARealWorker pins
// cleat-review's should-fix on #2308: --encryption-key-file-previous without
// --encryption-key-file used to be silently ignored at the CLI, because all
// three of main.go's call sites only invoked loadPayloadEncryption inside
// `if *encryptionKeyFile != ""` -- the function's own refusal
// (cmd/cleat-worker/payload_encryption_flags_test.go's
// TestLoadPayloadEncryption_PreviousWithoutCurrentIsRefused) never had a
// chance to run for a real invocation. This starts the compiled binary the
// way an operator would and confirms it now exits non-zero rather than
// starting quietly with encryption off.
//
// Bounded with exec.CommandContext, not exec.Command (cleat-review,
// #2308): on a regression the worker does not exit at all -- it starts
// cleanly with encryption off and sits polling for work -- so
// CombinedOutput blocks until something kills the process. Without a
// deadline of its own, that "something" is go test's whole-suite timeout,
// which turns one silent regression into every other test in this package
// failing to report anything either.
func TestEncryptionKeyFilePreviousWithoutCurrentIsRefusedByARealWorker(t *testing.T) {
	suffix := uniqueSuffix()
	taskQueue := "queue-payload-rotation-refuse-" + suffix
	keyA := writeEncryptionKeyFile(t)
	bin := buildWorker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	//nolint:gosec // bin is built by this test from this repo.
	cmd := exec.CommandContext(ctx, bin,
		"--db", appDSN(t),
		"--migrate-db", ensureCrashDatabase(t),
		"--task-queue", taskQueue,
		"--poll", "200ms",
		"--concurrency", "1",
		"--migrate-on-start",
		"--encryption-key-file-previous", keyA,
	)
	cmd.Dir = repoRoot(t)
	// Own process group, so a context-deadline kill (which signals cmd.Process
	// alone) cannot leave a grandchild running -- matching worker.kill()'s
	// SysProcAttr elsewhere in this package.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("worker did not exit within 15s of being given "+
			"--encryption-key-file-previous with no --encryption-key-file -- "+
			"want a fast non-zero refusal, not a worker that starts and runs\n"+
			"--- output ---\n%s", out)
	}
	if err == nil {
		t.Fatalf("worker exited 0 with --encryption-key-file-previous and no "+
			"--encryption-key-file -- want a non-zero refusal\n--- output ---\n%s", out)
	}
	if !strings.Contains(string(out), "requires --encryption-key-file") {
		t.Errorf("worker refused, but its output does not name the missing flag:\n%s", out)
	}
}

// TestPayloadEncryptionShardedWorkerDoesNotWritePlaintext pins cleat#2305's
// second half, found by cleat-review while re-reviewing #2308: restoring the
// shadowing var cleat#2305 removed keeps every OTHER test in this file
// green, because none of them start a worker with --shards-file.
// cmd/cleat-worker/main.go's sharded branch (main.go:811-855) builds each
// shard's store factory from the sharded-path's own `payloadEncryption`
// variable, which the shadow left permanently nil even with
// --encryption-key-file set and loaded correctly -- so
// f.WithEncryption(nil, true) ran, encryption was silently off, and
// event_history was written in plain text with the run still ending done.
// That was develop's behaviour before the fix in this PR; this test is what
// should have caught it, and did not exist to.
//
// Also exercises one rotation handover (worker A under key A crashes,
// worker B under key B with A as previous resumes) on the sharded path --
// the same sequence TestPayloadEncryptionKeyRotationWiresIntoARealWorker
// proves on the non-sharded one. The two paths build their
// PayloadEncryption independently (main.go:843 sharded vs. main.go:1190 and
// :1229 non-sharded), so proving one says nothing about the other -- which
// is exactly how cleat#2305's sharded half survived this file's first
// version unnoticed.
func TestPayloadEncryptionShardedWorkerDoesNotWritePlaintext(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()

	suffix := uniqueSuffix()
	taskQueue := "queue-payload-rotation-sharded-" + suffix
	wfID := "payload-rotation-sharded-wf-" + suffix
	marker := "order-rotation-sharded-marker-" + suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	shardsFile := writeShardsFile(t, appDSN(t))

	svc := newChargeService(t)
	releaseCharge := svc.holdOperation("Charge")
	defer releaseCharge() // see TestPayloadEncryptionKeyRotationWiresIntoARealWorker's comment on this pattern

	keyA := writeEncryptionKeyFile(t)
	first := startShardedWorker(t, bin, shardsFile, taskQueue, svc.srv.URL,
		"--encrypt-sensitive-payloads", "--encryption-key-file", keyA)
	startWorkflow(t, db, wfID, marker, taskQueue)

	svc.awaitHeldCall(t, first, startBudget)
	if r, c, _ := svc.allCounts(); r != 1 || c != 1 {
		t.Fatalf("before the crash: Reserve=%d Charge=%d, want 1/1 -- the workflow "+
			"did not reach the held call cleanly\n--- worker log ---\n%s", r, c, first.output())
	}

	// Checked HERE, while the workflow is still "running" -- see
	// TestPayloadEncryptionWiringKnownPositive's comment on why after
	// awaitTerminal would prove nothing.
	if eventTextContains(t, db, wfID, marker) {
		dumpEventHistory(t, db, wfID)
		t.Fatalf("event_history for %s carries %q in plain text on a SHARDED "+
			"worker with --encryption-key-file set -- cleat#2305: the sharded "+
			"path's own store factory never received the loaded key\n"+
			"--- worker log ---\n%s", wfID, marker, first.output())
	}

	first.kill()
	releaseCharge()

	releaseShip := svc.holdOperation("Ship")
	defer releaseShip()

	keyB := writeEncryptionKeyFile(t)
	second := startShardedWorker(t, bin, shardsFile, taskQueue, svc.srv.URL,
		"--encrypt-sensitive-payloads", "--encryption-key-file", keyB,
		"--encryption-key-file-previous", keyA)

	// Reaching this requires worker B's sharded store to have decrypted
	// Reserve's keyA-sealed event during replay.
	svc.awaitHeldCall(t, second, startBudget)

	if eventTextContains(t, db, wfID, marker) {
		dumpEventHistory(t, db, wfID)
		t.Fatalf("event_history for %s carries %q in plain text after Charge's "+
			"retry on the rolled sharded worker\n--- worker log ---\n%s",
			wfID, marker, second.output())
	}
	releaseShip()

	status, errMsg := awaitTerminal(t, db, wfID, completeBudget)
	if status != "done" && status != "completed" {
		t.Fatalf("workflow ended %q (%s) on the sharded rotation handover\n--- worker log ---\n%s",
			status, errMsg, second.output())
	}

	reserve, charge, ship := svc.allCounts()
	t.Logf("sharded, after rotation: Reserve=%d Charge=%d Ship=%d", reserve, charge, ship)
	if reserve != 1 {
		t.Errorf("Reserve=%d, want 1", reserve)
	}
	if charge != 2 {
		t.Errorf("Charge=%d, want 2", charge)
	}
	if ship != 1 {
		t.Errorf("Ship=%d, want 1", ship)
	}
}
