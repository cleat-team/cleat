package main

// cleat#2298 PR 2: reloadKeyRingsOnSIGHUP is the SIGHUP handler's body. These
// tests exercise it directly -- no signal, no subprocess -- so the wiring
// (M4's candidate-opens-everything check under the #2160 gate, the
// republished secret_key_versions, the success/failure metric, and the
// payload ring's safe-refusal-not-corruption behaviour) is covered at the
// level a real syscall.Kill test (tests/crash) cannot reach cheaply: a
// subprocess test can only observe the log line and that the worker kept
// serving, not that admin.workers.secret_key_versions is byte-for-byte what
// it should be, or that CheckKeyRingCandidate ran against BOTH stores.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// syncLogBuf is a concurrency-safe log sink: reloadKeyRingsOnSIGHUP is what a
// real SIGHUP handler goroutine calls, so a test asserting on its log output
// writes from the same goroutine it reads from here, but slog's own handler
// contract makes no promise about that staying true, and a bare
// strings.Builder is not safe for concurrent use regardless.
type syncLogBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type sighupEnv struct {
	db  *sql.DB
	reg *engine.WorkerRegistry
	w   *Worker
	id  string
	log *syncLogBuf
}

func newSighupEnv(t *testing.T, initial *engine.KeyRing) *sighupEnv {
	t.Helper()
	db := testutil.SuiteTestDB(t, "cleat_worker")
	reg := &engine.WorkerRegistry{DB: db, Dialect: engine.DialectPostgres}
	id := fmt.Sprintf("sighup-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = reg.Deregister(context.Background(), id) })
	secretsRing := engine.NewReloadableKeyRing(initial)
	logBuf := &syncLogBuf{}
	w := &Worker{
		id:                id,
		ctx:               context.Background(),
		logger:            slog.New(slog.NewTextHandler(logBuf, nil)),
		Metrics:           newTestPrometheus(),
		workerRegistry:    reg,
		secretsRing:       secretsRing,
		secrets:           engine.NewSecretStoreWithReloadableRing(db, "postgres", secretsRing),
		deploymentSecrets: engine.NewDeploymentSecretStoreWithReloadableRing(db, "postgres", secretsRing),
	}
	return &sighupEnv{db: db, reg: reg, w: w, id: id, log: logBuf}
}

func (e *sighupEnv) publishedKeys(t *testing.T) (value string, present bool) {
	t.Helper()
	var v sql.NullString
	err := e.db.QueryRow(`SELECT secret_key_versions FROM admin.workers WHERE worker_id = $1`, e.id).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("read the published key set: %v", err)
	}
	return v.String, true
}

func sighupRing(t *testing.T, kvs ...engine.VersionedKey) *engine.KeyRing {
	t.Helper()
	if len(kvs) == 0 {
		return nil
	}
	r, err := engine.NewKeyRing(kvs[0], kvs[1:]...)
	if err != nil {
		t.Fatalf("build ring: %v", err)
	}
	return r
}

func sighupKey(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

// A candidate that opens every stored deployment secret (v1 kept as
// previous) is accepted: the secrets ring moves, admin.workers republishes
// the new version set, and the success metric fires.
func TestReloadKeyRingsOnSIGHUP_SecretsRotationSucceedsAndRepublishesVersions(t *testing.T) {
	v1 := engine.VersionedKey{Version: 1, Key: sighupKey(0x11)}
	v2 := engine.VersionedKey{Version: 2, Key: sighupKey(0x22)}
	e := newSighupEnv(t, sighupRing(t, v1))
	ctx := context.Background()

	const rotationSecretName = "cleat-2298-sighup-rotation.key"
	if err := e.w.deploymentSecrets.PutDeploymentSecret(ctx, rotationSecretName, "value"); err != nil {
		t.Fatalf("seed a deployment secret under v1: %v", err)
	}
	// SuiteTestDB is a SHARED per-suite database: a row left behind here is
	// still there for the next test, including ones that configure no
	// secret ring at all -- where CheckKeyRingCandidate(ctx, nil) would
	// then report it Unopenable and refuse a reload that was never asking
	// about secrets in the first place.
	t.Cleanup(func() { _, _ = e.db.Exec(`DELETE FROM deployment_secrets WHERE name = $1`, rotationSecretName) })
	if err := e.reg.Register(ctx, engine.WorkerRegistration{WorkerID: e.id, Hostname: "h", PID: 1, SecretKeyVersions: []int{1}}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLEAT_SECRET_MASTER_KEY", mustB64(v2.Key))
	t.Setenv("CLEAT_SECRET_MASTER_KEY_VERSION", "2")
	t.Setenv("CLEAT_SECRET_MASTER_KEY_PREVIOUS", mustB64(v1.Key))
	t.Setenv("CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION", "1")

	e.w.reloadKeyRingsOnSIGHUP(ctx)

	if got := e.w.secretsRing.Load().Current().Version; got != 2 {
		t.Errorf("secretsRing.Load().Current().Version = %d, want 2 -- the reload should have swapped", got)
	}
	if got, ok := e.publishedKeys(t); !ok || got != "1,2" {
		t.Errorf("published secret_key_versions = %q (present=%v), want %q", got, ok, "1,2")
	}
}

// A candidate that DROPS a version still holding live data is refused:
// nothing swaps, nothing is republished, and the refusal is the failure
// metric's job to count (checked via the ring and the registry row, which
// is what an operator-facing symptom would actually be).
func TestReloadKeyRingsOnSIGHUP_SecretsRefusedWhenACandidateCannotOpenAStoredSecret(t *testing.T) {
	v1 := engine.VersionedKey{Version: 1, Key: sighupKey(0x11)}
	v2 := engine.VersionedKey{Version: 2, Key: sighupKey(0x22)}
	e := newSighupEnv(t, sighupRing(t, v1))
	ctx := context.Background()

	const refusalSecretName = "cleat-2298-sighup-refusal.key"
	if err := e.w.deploymentSecrets.PutDeploymentSecret(ctx, refusalSecretName, "value"); err != nil {
		t.Fatalf("seed a deployment secret under v1: %v", err)
	}
	t.Cleanup(func() { _, _ = e.db.Exec(`DELETE FROM deployment_secrets WHERE name = $1`, refusalSecretName) })
	if err := e.reg.Register(ctx, engine.WorkerRegistration{WorkerID: e.id, Hostname: "h", PID: 1, SecretKeyVersions: []int{1}}); err != nil {
		t.Fatal(err)
	}

	// v2 ONLY -- v1 dropped, while a secret is still sealed under it. M4 must
	// refuse this: a drop is fine once nothing is sealed under the dropped
	// version (reseal-secrets' job), never before.
	t.Setenv("CLEAT_SECRET_MASTER_KEY", mustB64(v2.Key))
	t.Setenv("CLEAT_SECRET_MASTER_KEY_VERSION", "2")

	e.w.reloadKeyRingsOnSIGHUP(ctx)

	if got := e.w.secretsRing.Load().Current().Version; got != 1 {
		t.Errorf("secretsRing.Load().Current().Version = %d, want 1 (unchanged) -- a refused reload must not swap", got)
	}
	if got, ok := e.publishedKeys(t); !ok || got != "1" {
		t.Errorf("published secret_key_versions = %q (present=%v), want %q (unchanged -- the attempted "+
			"registration must have been withdrawn, not left committed with versions this worker cannot open)", got, ok, "1")
	}
}

// The payload half's safety property: a SIGHUP that finds the key file(s)
// UNCHANGED is a clean no-op. This is the common case -- an operator
// rotating secrets sends one SIGHUP fleet-wide, and most workers' payload
// files are not part of that change at all.
func TestReloadKeyRingsOnSIGHUP_PayloadUnchangedFileIsACleanNoOp(t *testing.T) {
	e := newSighupEnv(t, nil) // no secret ring configured -- isolates this to the payload half
	ctx := context.Background()
	if err := e.reg.Register(ctx, engine.WorkerRegistration{WorkerID: e.id, Hostname: "h", PID: 1}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	curPath := filepath.Join(dir, "current.b64")
	key := sighupKey(0x33)
	if err := os.WriteFile(curPath, []byte(mustB64(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	ring, err := loadPayloadKeyRing(curPath, "", payloadKeyVersions{})
	if err != nil {
		t.Fatalf("load initial payload ring: %v", err)
	}
	e.w.payloadRing = engine.NewReloadableKeyRing(ring)
	e.w.payloadKeyFile = curPath

	before := e.w.payloadRing.Load()
	e.w.reloadKeyRingsOnSIGHUP(ctx)
	after := e.w.payloadRing.Load()

	if before.Current().Version != after.Current().Version {
		t.Errorf("payload ring's current version changed on an unmodified file: %d -> %d", before.Current().Version, after.Current().Version)
	}
	if string(before.Current().Key) != string(after.Current().Key) {
		t.Error("payload ring's current key bytes changed on an unmodified file")
	}
	// Distinguishes "Reload ran and found nothing to change" from "the
	// payload branch never ran at all" -- both leave the ring looking
	// identical, which is exactly why the ring's state alone cannot tell
	// them apart. Only one of them logs this line.
	if !strings.Contains(e.log.String(), "SIGHUP: key reload complete") {
		t.Errorf("expected a successful-reload log line; got:\n%s", e.log.String())
	}
}

// The payload half's other safety property, UNCHANGED by cleat#3203:
// rewriting the CURRENT key's bytes under the SAME version label (here, the
// default -- no --encryption-key-file-version flags set at all) is SAFELY
// REFUSED, not silently applied and not a crash. The old key keeps serving.
// This is exactly the shape loadPayloadKeyRing's hardcoded version scheme
// used to ALWAYS present, before cleat#3203 gave it a way out -- see
// TestReloadKeyRingsOnSIGHUP_PayloadRotationViaNewVersionNumbersSucceeds
// below for the now-possible escape via a version an operator actually
// changes.
func TestReloadKeyRingsOnSIGHUP_PayloadChangedCurrentKeyIsSafelyRefused(t *testing.T) {
	e := newSighupEnv(t, nil)
	ctx := context.Background()
	if err := e.reg.Register(ctx, engine.WorkerRegistration{WorkerID: e.id, Hostname: "h", PID: 1}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	curPath := filepath.Join(dir, "current.b64")
	oldKey := sighupKey(0x33)
	if err := os.WriteFile(curPath, []byte(mustB64(oldKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	ring, err := loadPayloadKeyRing(curPath, "", payloadKeyVersions{})
	if err != nil {
		t.Fatalf("load initial payload ring: %v", err)
	}
	e.w.payloadRing = engine.NewReloadableKeyRing(ring)
	e.w.payloadKeyFile = curPath

	// Rewrite the SAME path with DIFFERENT bytes -- the operator mistake (or
	// a misguided rotation attempt) M3 exists to catch.
	newKey := sighupKey(0x44)
	if err := os.WriteFile(curPath, []byte(mustB64(newKey)), 0o600); err != nil {
		t.Fatal(err)
	}

	e.w.reloadKeyRingsOnSIGHUP(ctx)

	live := e.w.payloadRing.Load()
	if string(live.Current().Key) != string(oldKey) {
		t.Error("the payload ring's live key changed after a refused reload -- a version was reused " +
			"with different bytes and this must have been refused, keeping the old key")
	}
	// Distinguishes "Reload actually ran and refused" from "the payload
	// branch never ran" -- same reasoning as the no-op test's log
	// assertion above, for the opposite outcome.
	if !strings.Contains(e.log.String(), "SIGHUP: payload encryption key reload refused") {
		t.Errorf("expected the specific refusal log line; got:\n%s", e.log.String())
	}
}

// cleat#3203's whole point, proven rather than argued: SIGHUP-driven
// payload key rotation, genuinely impossible before this issue (every
// content change under the hardcoded version labels hit M3,
// unconditionally -- the test above still pins that baseline), now
// succeeds when the operator gives the new key an unused version number.
//
// Two SIGHUPs, each only ever ADDING a version the live ring never held:
//
//  1. Introduce the new key B as PREVIOUS under a brand-new version (11).
//     Current stays A at its existing version (10), unchanged -- so M3
//     sees no conflict on either label, and the candidate ring now holds
//     both keys. Nothing new is sealed under B yet.
//  2. Swap roles: B (already at 11, unchanged bytes) becomes current, A
//     (already at 10, unchanged bytes) becomes previous. THIS STEP NEEDS
//     NO NEW VERSION NUMBER, which is worth confirming rather than
//     assuming: engine.ReloadableKeyRing.Reload's M3 check
//     (refuseReusedVersion) compares version-to-bytes pairs only, and
//     never looks at which slot (current vs. previous) a version occupies
//     -- so moving an already-held, unchanged version between slots can
//     never trip it. cleat#3203's own issue text describes this step as
//     needing "another new version number"; measured here, it does not.
//
// After step 2, a freshly-sealed payload uses B -- the rotation that was
// categorically impossible (TestReloadKeyRingsOnSIGHUP_PayloadChangedCurrentKeyIsSafelyRefused,
// above) has now genuinely happened, with no restart.
func TestReloadKeyRingsOnSIGHUP_PayloadRotationViaNewVersionNumbersSucceeds(t *testing.T) {
	e := newSighupEnv(t, nil)
	ctx := context.Background()
	if err := e.reg.Register(ctx, engine.WorkerRegistration{WorkerID: e.id, Hostname: "h", PID: 1}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	curPath := filepath.Join(dir, "current.b64")
	prevPath := filepath.Join(dir, "previous.b64")
	keyA := sighupKey(0x55)
	keyB := sighupKey(0x66)

	// Boot: current = A @ version 10, no previous.
	if err := os.WriteFile(curPath, []byte(mustB64(keyA)), 0o600); err != nil {
		t.Fatal(err)
	}
	e.w.payloadKeyVersions = payloadKeyVersions{current: "10"}
	ring, err := loadPayloadKeyRing(curPath, "", e.w.payloadKeyVersions)
	if err != nil {
		t.Fatalf("load initial payload ring: %v", err)
	}
	if got := ring.Current().Version; got != 10 {
		t.Fatalf("initial ring's current version = %d, want 10 -- the version flag did not take", got)
	}
	e.w.payloadRing = engine.NewReloadableKeyRing(ring)
	e.w.payloadKeyFile = curPath

	// Step 1: introduce B as previous @ version 11 (new). Current (A @ 10)
	// is untouched.
	if err := os.WriteFile(prevPath, []byte(mustB64(keyB)), 0o600); err != nil {
		t.Fatal(err)
	}
	e.w.payloadKeyFilePrevious = prevPath
	e.w.payloadKeyVersions.previous = "11"

	e.w.reloadKeyRingsOnSIGHUP(ctx)

	afterStep1 := e.w.payloadRing.Load()
	if got := afterStep1.Current().Version; got != 10 {
		t.Fatalf("after step 1, current version = %d, want 10 (unchanged -- B is only previous so far)", got)
	}
	if string(afterStep1.Current().Key) != string(keyA) {
		t.Fatal("after step 1, the current key's bytes changed -- step 1 should only ADD B as previous")
	}
	if bKey, ok := afterStep1.Key(11); !ok || string(bKey.Key) != string(keyB) {
		t.Fatalf("after step 1, version 11 should resolve to B's bytes (ok=%v)", ok)
	}
	if !strings.Contains(e.log.String(), "SIGHUP: key reload complete") {
		t.Fatalf("step 1 should have succeeded; log:\n%s", e.log.String())
	}

	// Step 2: swap roles. Current := B's file @ 11 (same version, same
	// bytes, now current). Previous := A's file @ 10 (same version, same
	// bytes, now previous). No file content changes -- only which file is
	// "current" and which version label each flag now names.
	e.w.payloadKeyFile = prevPath        // B's file
	e.w.payloadKeyFilePrevious = curPath // A's file
	e.w.payloadKeyVersions = payloadKeyVersions{current: "11", previous: "10"}

	e.w.reloadKeyRingsOnSIGHUP(ctx)

	afterStep2 := e.w.payloadRing.Load()
	if got := afterStep2.Current().Version; got != 11 {
		t.Fatalf("after step 2, current version = %d, want 11 -- the promotion (role swap) should have succeeded with no new version needed", got)
	}
	if string(afterStep2.Current().Key) != string(keyB) {
		t.Fatal("after step 2, the current key should be B's bytes -- the rotation this issue exists to make possible")
	}
	if aKey, ok := afterStep2.Key(10); !ok || string(aKey.Key) != string(keyA) {
		t.Fatalf("after step 2, version 10 should still resolve to A's bytes, now as previous (ok=%v)", ok)
	}

	// The functional proof, not just the ring's bookkeeping: a freshly
	// sealed payload must actually use B now.
	candidatePE, err := engine.NewPayloadEncryptionWithReloadableRing(e.w.payloadRing)
	if err != nil {
		t.Fatalf("build a PayloadEncryption around the rotated ring: %v", err)
	}
	const tenant = "33333333-3333-3333-3333-333333333333"
	sealed, err := candidatePE.Encrypt(tenant, []byte("sealed-after-rotation"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, form, err := candidatePE.OpenAndClassify(tenant, sealed)
	if err != nil {
		t.Fatalf("OpenAndClassify: %v", err)
	}
	if string(got) != "sealed-after-rotation" {
		t.Errorf("opened %q, want %q", got, "sealed-after-rotation")
	}
	if form != engine.PayloadFormDerived {
		t.Errorf("form = %v, want PayloadFormDerived -- a fresh seal should use the CURRENT key (B), not fall back to the previous one", form)
	}

	if strings.Contains(e.log.String(), "SIGHUP: payload encryption key reload refused") {
		t.Error("step 2 (the promotion) must not have been refused -- it reuses only already-held, unchanged version/byte pairs")
	}
}

// S1's "both or neither": the payload ring moves FIRST (step 1, see
// reloadKeyRingsOnSIGHUP's doc comment), then the secrets gate refuses
// (step 2) -- so the payload ring's step-1 swap must be REVERTED, leaving
// it on the EXACT ring object that was live before this call, not merely
// one with equivalent content. Pointer identity, not content equality, is
// the assertion: loadPayloadKeyRing allocates a fresh *engine.KeyRing on
// every call even when the file content is byte-identical, so a reverted
// ring and a never-reverted-because-never-moved ring would read identically
// by content either way -- only the object identity tells them apart.
func TestReloadKeyRingsOnSIGHUP_SecretsFailureRevertsAnAlreadyMovedPayloadRing(t *testing.T) {
	v1 := engine.VersionedKey{Version: 1, Key: sighupKey(0x11)}
	v2 := engine.VersionedKey{Version: 2, Key: sighupKey(0x22)}
	e := newSighupEnv(t, sighupRing(t, v1))
	ctx := context.Background()

	const secretName = "cleat-2298-sighup-both-or-neither.key"
	if err := e.w.deploymentSecrets.PutDeploymentSecret(ctx, secretName, "value"); err != nil {
		t.Fatalf("seed a deployment secret under v1: %v", err)
	}
	t.Cleanup(func() { _, _ = e.db.Exec(`DELETE FROM deployment_secrets WHERE name = $1`, secretName) })
	if err := e.reg.Register(ctx, engine.WorkerRegistration{WorkerID: e.id, Hostname: "h", PID: 1, SecretKeyVersions: []int{1}}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	curPath := filepath.Join(dir, "current.b64")
	payloadKey := sighupKey(0x55)
	if err := os.WriteFile(curPath, []byte(mustB64(payloadKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	payloadRing, err := loadPayloadKeyRing(curPath, "", payloadKeyVersions{})
	if err != nil {
		t.Fatalf("load initial payload ring: %v", err)
	}
	e.w.payloadRing = engine.NewReloadableKeyRing(payloadRing)
	e.w.payloadKeyFile = curPath
	originalPayloadRing := e.w.payloadRing.Load()

	// Secrets: v2 only -- v1 dropped while the seeded secret is still sealed
	// under it. M4 must refuse. The payload file is left UNCHANGED, so its
	// own step-1 reload succeeds trivially (new object, same bytes) before
	// step 2 ever runs.
	t.Setenv("CLEAT_SECRET_MASTER_KEY", mustB64(v2.Key))
	t.Setenv("CLEAT_SECRET_MASTER_KEY_VERSION", "2")

	e.w.reloadKeyRingsOnSIGHUP(ctx)

	if e.w.payloadRing.Load() != originalPayloadRing {
		t.Error("the payload ring was not reverted to the exact pre-call object after the secrets half " +
			"refused -- S1 requires both rings to move or neither, and this leaves the payload ring " +
			"on a different (if content-identical) object than the one live before this call")
	}
	if !strings.Contains(e.log.String(), "SIGHUP: reverted the payload ring after the secrets half refused") {
		t.Errorf("expected the revert log line; got:\n%s", e.log.String())
	}
}

// A counterpart to the both-or-neither test above, one level in: cleat-review
// found and falsified live that secretsRing.Reload (sighup_reload.go) runs
// *inside* RegisterUnderKeyGate's check callback, before that call's own
// commit -- so an unrelated, late infrastructure failure in that narrow
// window leaves the secrets ring already moved while RegisterUnderKeyGate
// still reports an error. Before the fix this was read identically to a
// refused check: "keeping the live ring" logged (false), and only the
// payload ring reverted -- the secrets ring stayed on the candidate with
// nothing in the logs or the failure metric saying so. This reproduces that
// exact window via afterSecretsRingMovedForTest and asserts the ring is
// reverted and the correct (distinguishing) log line is used.
func TestReloadKeyRingsOnSIGHUP_LateRegistrationFailureRevertsAnAlreadyMovedSecretsRing(t *testing.T) {
	v1 := engine.VersionedKey{Version: 1, Key: sighupKey(0x11)}
	v2 := engine.VersionedKey{Version: 2, Key: sighupKey(0x22)}
	e := newSighupEnv(t, sighupRing(t, v1))
	ctx := context.Background()

	const secretName = "cleat-2298-sighup-late-failure.key"
	if err := e.w.deploymentSecrets.PutDeploymentSecret(ctx, secretName, "value"); err != nil {
		t.Fatalf("seed a deployment secret under v1: %v", err)
	}
	t.Cleanup(func() { _, _ = e.db.Exec(`DELETE FROM deployment_secrets WHERE name = $1`, secretName) })
	if err := e.reg.Register(ctx, engine.WorkerRegistration{WorkerID: e.id, Hostname: "h", PID: 1, SecretKeyVersions: []int{1}}); err != nil {
		t.Fatal(err)
	}

	// v2 current, v1 kept as previous -- a candidate M4 accepts, so the check
	// succeeds and secretsRing.Reload actually swaps. This HAS to be a
	// genuine success up to that point, or the window this test targets
	// (a failure strictly AFTER the swap) is never reached.
	t.Setenv("CLEAT_SECRET_MASTER_KEY", mustB64(v2.Key))
	t.Setenv("CLEAT_SECRET_MASTER_KEY_VERSION", "2")
	t.Setenv("CLEAT_SECRET_MASTER_KEY_PREVIOUS", mustB64(v1.Key))
	t.Setenv("CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION", "1")

	// keyGatePGKey is engine/secret_key_gate.go:90's advisory-lock key,
	// unexported there. By the time afterSecretsRingMovedForTest runs,
	// acquireKeyGate has already taken this lock in SHARED mode on the gate
	// transaction's own backend (it runs before the check callback, which
	// is before this hook) -- so finding and killing that backend here is
	// deterministic, not a race against RegisterUnderKeyGate's own timing.
	const keyGatePGKey int64 = 7215842093104563
	e.w.afterSecretsRingMovedForTest = func() {
		var pid int
		if err := e.db.QueryRow(
			`SELECT pid FROM pg_locks WHERE locktype = 'advisory' `+
				`AND ((classid::bigint << 32) | objid::bigint) = $1 AND granted = true`,
			keyGatePGKey,
		).Scan(&pid); err != nil {
			t.Fatalf("find the gate transaction's backend pid: %v", err)
		}
		var terminated bool
		if err := e.db.QueryRow(`SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil {
			t.Fatalf("terminate the gate transaction's backend: %v", err)
		}
		if !terminated {
			t.Fatalf("pg_terminate_backend(%d) reported false -- the gate's backend was not found or already gone", pid)
		}
	}

	e.w.reloadKeyRingsOnSIGHUP(ctx)

	if got := e.w.secretsRing.Load().Current().Version; got != 1 {
		t.Errorf("secretsRing.Load().Current().Version = %d, want 1 -- a registration that failed AFTER "+
			"the ring already moved must revert the ring, not leave it on the candidate", got)
	}
	if got, ok := e.publishedKeys(t); !ok || got != "1" {
		t.Errorf("published secret_key_versions = %q (present=%v), want %q (unchanged -- the registration "+
			"that would have advertised the candidate's versions never committed)", got, ok, "1")
	}
	if !strings.Contains(e.log.String(), "the ring already moved, but registering it failed afterward") {
		t.Errorf("expected the log line distinguishing a late infrastructure failure from a refused check; got:\n%s", e.log.String())
	}
	if !strings.Contains(e.log.String(), "reverted the secrets ring after its own registration failed to commit") {
		t.Errorf("expected the revert-succeeded log line; got:\n%s", e.log.String())
	}
	if strings.Contains(e.log.String(), "keeping the live ring and the previously published key versions") {
		t.Error("the refusal line (accurate only when the check itself failed, before any swap) must not " +
			"appear when the ring had already moved before the failure")
	}
}

func mustB64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
