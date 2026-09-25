package crash

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// releaseLogLine is what cmd/cleat-worker's releaseForAnotherWorker logs. The
// test waits for it rather than for a status, because "ready" is also what a
// run looks like before anybody has claimed it.
const releaseLogLine = "this worker cannot serve this workflow"

// writeSealingKey writes a fresh random 32-byte AES-256 key, base64-encoded,
// in the format --encryption-key-file expects.
func writeSealingKey(t *testing.T) string {
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

// TestAWorkerThatCannotDecryptAReplayReleasesTheRun pins cleat#2311 to a REAL
// worker process.
//
// A run's history is sealed under key A. A worker holding key B (the shape of
// a mis-ordered rotation, or a mis-deploy) claims it and cannot read step 0.
// Before the fix the store swallowed that: it substituted "[DECRYPTION_FAILED]"
// for the field and carried on, so the run either ended FAILED permanently
// (checksums on, the default -- the chain no longer matched) or ended DONE with
// the sentinel in its result (--disable-checksum-verification). A worker that
// DOES hold key A could have finished it either way.
//
// The unreadable history is a fact about THIS worker's key ring, the same
// class as a plugin it lacks (cleat#1710), so the run is released with a
// backoff and a worker with the right key completes it.
//
// Both flag settings are exercised because they fail differently on the old
// code -- one is a terminal failure, the other a terminal SUCCESS on garbage --
// and the second is the one nothing else would ever notice.
func TestAWorkerThatCannotDecryptAReplayReleasesTheRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
	}{
		{"checksums_on", nil},
		{"checksums_off", []string{"--disable-checksum-verification"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := ownerDB(t)
			defer db.Close()

			suffix := uniqueSuffix()
			taskQueue := "queue-decrypt-release-" + suffix
			wfID := "decrypt-release-wf-" + suffix

			deployFixture(t, db, taskQueue)
			bin := buildWorker(t)

			svc := newChargeService(t)
			releaseCharge := svc.holdOperation("Charge")
			defer releaseCharge() // see payload_encryption_rotation_test.go on why this is deferred

			// Worker A seals Reserve under key A and is killed with Charge in
			// flight, so the run's history holds one key-A event.
			keyA := writeSealingKey(t)
			first := startWorker(t, bin, taskQueue, svc.srv.URL,
				"--encrypt-sensitive-payloads", "--encryption-key-file", keyA)
			startWorkflow(t, db, wfID, "order-"+suffix, taskQueue)
			svc.awaitHeldCall(t, first, startBudget)
			first.kill()
			releaseCharge()

			// Worker B holds a different key. A short release backoff so that
			// worker C, started below, is not made to wait out the default.
			keyB := writeSealingKey(t)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()
			_ = ln.Close()
			bFlags := append([]string{
				"--encrypt-sensitive-payloads", "--encryption-key-file", keyB,
				"--unservable-release-backoff", "2s",
				"--api-addr", addr, "--require-auth=false", "--enable-admin-api",
			}, tc.flags...)
			wrong := startWorker(t, bin, taskQueue, svc.srv.URL, bFlags...)

			awaitReleaseNotTerminal(t, db, wfID, wrong)

			// The run must still be alive and unfinished, and worker B must
			// not have stamped anything onto it.
			status, errMsg, result := workflowOutcome(t, db, wfID)
			if status == "failed" || status == "done" || status == "completed" {
				t.Fatalf("after the wrong-key worker: status=%q error=%q result=%q -- the run ended "+
					"on a worker that could not read it", status, errMsg, result)
			}

			// A stuck run must be visible on /metrics, and the store-level
			// decryption counter must move on this path too.
			requireMetricAtLeast(t, addr, "cleat_workflow_releases_total", `check="history_decrypt"`, wrong)
			requireMetricAtLeast(t, addr, "cleat_decryption_errors_total", "", wrong)

			// The admin operations that write to a history must refuse one
			// this worker cannot read, before writing. The run is made
			// re-replayable first, so that a missing guard WOULD write.
			requireAdminRefusals(t, db, addr, wfID, wrong)

			// A stuck run must not flood the log: several releases have
			// happened by now (one every 2s), and each of the two lines below
			// used to be written every time -- about 22,000 a day at the
			// default backoff. The release line is rate limited, and the
			// store's own per-field WARN stays silent on the replay read
			// because the error it returns is what gets reported.
			if n := strings.Count(wrong.output(), releaseLogLine); n != 1 {
				t.Errorf("the release WARN was logged %d times in one window, want exactly 1\n--- worker log ---\n%s", n, wrong.output())
			}
			if n := strings.Count(wrong.output(), "decrypt failed"); n != 0 {
				t.Errorf("the store logged %d per-field \"decrypt failed\" WARNs on the replay read, want 0\n--- worker log ---\n%s", n, wrong.output())
			}
			wrong.kill()
			if _, err := db.Exec(`UPDATE workflow_instances SET status = 'ready', assigned_to = NULL, next_wake_at = now() WHERE id = $1`, wfID); err != nil {
				t.Fatalf("returning the run to ready: %v", err)
			}

			// Worker C holds key A and finishes it.
			right := startWorker(t, bin, taskQueue, svc.srv.URL,
				"--encrypt-sensitive-payloads", "--encryption-key-file", keyA)
			status, errMsg = awaitTerminal(t, db, wfID, completeBudget+30*time.Second)
			if status != "done" && status != "completed" {
				t.Fatalf("workflow ended %q (%s) under the worker that holds the key\n--- worker log ---\n%s",
					status, errMsg, right.output())
			}
			_, _, result = workflowOutcome(t, db, wfID)
			if strings.Contains(result, "DECRYPTION_FAILED") {
				t.Fatalf("result carries the decryption sentinel: %s", result)
			}
			if reserve, _, _ := svc.allCounts(); reserve != 1 {
				t.Errorf("Reserve=%d, want 1 -- it was durable under key A before the crash, and "+
					"worker C reading it (rather than re-running it) is what proves it decrypted", reserve)
			}
		})
	}
}

// awaitReleaseNotTerminal waits for w to log that it released the run. It
// fails, with the worker's log, if the run reaches a terminal status first --
// which is what the pre-fix worker did, and why the log line is the signal
// rather than the status: a status poll alone reads "ready" both before a claim
// and after a release.
func awaitReleaseNotTerminal(t *testing.T, db *sql.DB, id string, w *worker) {
	t.Helper()
	deadline := time.Now().Add(startBudget)
	for time.Now().Before(deadline) {
		if strings.Contains(w.output(), releaseLogLine) {
			return
		}
		status, errMsg, result := workflowOutcome(t, db, id)
		if status != "ready" && status != "running" {
			t.Fatalf("the run ended %q (error=%q result=%q) on a worker that could not decrypt its history; "+
				"it should have been released\n--- worker log ---\n%s", status, errMsg, result, w.output())
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the wrong-key worker never logged %q within %v\n--- worker log ---\n%s",
		releaseLogLine, startBudget, w.output())
}

func workflowOutcome(t *testing.T, db *sql.DB, id string) (status, errMsg, result string) {
	t.Helper()
	var msg, res sql.NullString
	if err := db.QueryRow(`SELECT status, error_msg, result::text FROM workflow_instances WHERE id = $1`, id).
		Scan(&status, &msg, &res); err != nil {
		t.Fatalf("reading %s: %v", id, err)
	}
	return status, msg.String, res.String
}

// requireMetricAtLeast fails unless the worker's /metrics carries a sample of
// name (with the label fragment, if given) whose value is at least 1.
func requireMetricAtLeast(t *testing.T, addr, name, label string, w *worker) {
	t.Helper()
	var last string
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\{?([^ ]*)\}? ([0-9.e+]+)$`)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			last = string(body)
			for _, m := range re.FindAllStringSubmatch(last, -1) {
				if (label == "" || strings.Contains(m[1], label)) && m[2] != "0" {
					return
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	var lines []string
	for _, l := range strings.Split(last, "\n") {
		if strings.Contains(l, name) {
			lines = append(lines, l)
		}
	}
	t.Fatalf("/metrics never showed %s %s at 1 or more; matching lines: %q\n--- worker log ---\n%s", name, label, lines, w.output())
}

// requireAdminRefusals calls re-replay and resolve-step on a run this worker
// cannot decrypt and requires a 409 that says nothing was changed, with the
// event count and status untouched.
func requireAdminRefusals(t *testing.T, db *sql.DB, addr, id string, w *worker) {
	t.Helper()
	// Force-complete and force-fail come FIRST, on a run that is not settled
	// (a settled run is refused by them for a different reason, which would
	// make this a test of the wrong thing). The run is parked as 'ready' with
	// its next wake an hour out, so the worker's own release loop does not
	// claim it while the calls are made.
	parked := false
	for i := 0; i < 80 && !parked; i++ {
		res, err := db.Exec(`UPDATE workflow_instances SET next_wake_at = now() + interval '1 hour' WHERE id = $1 AND status = 'ready'`, id)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := res.RowsAffected()
		parked = n == 1
		if !parked {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !parked {
		t.Fatalf("could not park the run\n--- worker log ---\n%s", w.output())
	}
	var generation int64
	if err := db.QueryRow(`SELECT generation FROM workflow_instances WHERE id = $1`, id).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	before := eventCount(t, db, id)
	if before == 0 {
		t.Fatalf("PRECONDITION FAILED: the run has no events, so nothing below measures a refused write")
	}
	post := func(path, confirm, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+path, strings.NewReader(body))
		req.Header.Set("X-Confirm", confirm)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v\n--- worker log ---\n%s", path, err, w.output())
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	refused := func(name, path, confirm, body string) {
		t.Helper()
		code, resp := post(path, confirm, body)
		if code != http.StatusConflict || !strings.Contains(resp, "Nothing was changed") {
			t.Errorf("%s on a history this worker cannot decrypt: %d %s, want 409 saying nothing was changed", name, code, resp)
		}
		if strings.Contains(resp, "message authentication") {
			t.Errorf("%s leaked driver text to the caller: %s", name, resp)
		}
	}
	g := strconv.FormatInt(generation, 10)
	refused("force-complete", "/api/admin/instances/"+id+"/force-complete", "force-complete", `{"generation":`+g+`,"result":"{}"}`)
	refused("force-fail", "/api/admin/instances/"+id+"/force-fail", "force-fail", `{"generation":`+g+`,"error_message":"x","error_code":"operator"}`)
	if got := eventCount(t, db, id); got != before {
		t.Errorf("event count went %d -> %d across refused force calls", before, got)
	}
	if st, _ := runRow(t, db, id); st != "ready" {
		t.Errorf("status is %q after refused force calls, want it untouched (ready): the status change must roll back with the audit event", st)
	}

	// Then re-replay and resolve-step, which need a re-replayable run.
	if _, err := db.Exec(`UPDATE workflow_instances SET status = 'failed' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, path, confirm, body string }{
		{"re-replay", "/api/admin/instances/" + id + "/re-replay", "re-replay", `{"generation":` + g + `}`},
		{"resolve-step", "/api/admin/instances/" + id + "/steps/0/resolve", "resolve-step", `{"response":"{}"}`},
	} {
		refused(c.name, c.path, c.confirm, c.body)
	}
	if got := eventCount(t, db, id); got != before {
		t.Errorf("event count went %d -> %d across refused admin calls; an admin_action sealed under the wrong key is exactly what this must prevent", before, got)
	}
	if st, _ := runRow(t, db, id); st != "failed" {
		t.Errorf("status is %q after refused admin calls, want it untouched (failed)", st)
	}
}

// Turning --encrypt-sensitive-payloads on for a deployment with runs in flight
// is an ordinary upgrade. The history those runs already wrote is plaintext, the
// worker that resumes them holds a key, and it must finish them: reading a
// plaintext row is not a decryption failure. The strict replay load once
// refused them ("illegal base64 data") and released such a run forever, on every
// worker, where develop read them fine.
func TestEnablingEncryptionMidRunDoesNotStrandTheRun(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()

	suffix := uniqueSuffix()
	taskQueue := "queue-encrypt-midrun-" + suffix
	wfID := "encrypt-midrun-wf-" + suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	releaseCharge := svc.holdOperation("Charge")
	defer releaseCharge()

	// Worker A has no encryption at all: Reserve is durable in plaintext.
	first := startWorker(t, bin, taskQueue, svc.srv.URL)
	startWorkflow(t, db, wfID, "order-"+suffix, taskQueue)
	svc.awaitHeldCall(t, first, startBudget)
	if !eventTextContains(t, db, wfID, "order-"+suffix) {
		t.Fatalf("PRECONDITION FAILED: the history is not plaintext, so this measures nothing about a plaintext history")
	}
	first.kill()
	releaseCharge()

	// Worker C is the same deployment with encryption switched on.
	second := startWorker(t, bin, taskQueue, svc.srv.URL,
		"--encrypt-sensitive-payloads", "--encryption-key-file", writeSealingKey(t))
	status, errMsg := awaitTerminal(t, db, wfID, completeBudget)
	if status != "done" && status != "completed" {
		t.Fatalf("workflow ended %q (%s) on the worker that turned encryption on\n--- worker log ---\n%s",
			status, errMsg, second.output())
	}
	if strings.Contains(second.output(), releaseLogLine) {
		t.Errorf("the worker released the run instead of finishing it\n--- worker log ---\n%s", second.output())
	}
	if reserve, _, _ := svc.allCounts(); reserve != 1 {
		t.Errorf("Reserve=%d, want 1: it was durable before the switch and must be read, not re-run", reserve)
	}
}
