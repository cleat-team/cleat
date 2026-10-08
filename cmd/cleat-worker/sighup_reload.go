package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"

	"github.com/cleat-team/cleat/engine"
)

// reloadKeyRingsOnSIGHUP re-reads every key source cleat#2298 covers --
// CLEAT_SECRET_MASTER_KEY[_FILE] and its three siblings for the shared
// tenant-secrets/deployment-secrets ring, --encryption-key-file[-previous]
// for the payload ring -- and, if everything validates, swaps both with no
// restart. This is the body of the SIGHUP handler main.go installs; split
// out so the logic can be exercised without a signal or a running process.
//
// S1: "swap both rings or neither, with a success/failure metric." The two
// rings have no common transaction to share (one lives entirely in this
// process's memory, the other's safety check needs a database round trip
// under the #2160 gate), so "neither" is achieved by ORDER rather than by a
// shared commit:
//
//  1. The PAYLOAD ring is validated and swapped FIRST. It is pure in-memory
//     work (ReloadableKeyRing.Reload's own M3 check), nothing external reads
//     "has the payload ring moved yet" as a signal to act on, and a prior
//     ring is trivially available to swap back to if the next step fails.
//
//  2. The SECRETS ring is validated and swapped second, under the shared
//     gate, which runs M4 (every stored tenant AND deployment secret must
//     open under the candidate) before the swap and republishes
//     admin.workers.secret_key_versions as part of the same call.
//
//  3. If step 2 fails for any reason, step 1's swap is REVERTED -- back to
//     the exact ring that was live before this call, which Reload's own M3
//     check can never refuse (reverting introduces no version/byte
//     conflict with itself). This closes the window to one failed gate
//     call's worth of time, during which nothing outside this process
//     could have observed or acted on the payload ring having moved.
//
//  4. Step 2 itself has the same shape one level in: secretsRing.Reload
//     runs INSIDE RegisterUnderKeyGate's check callback, before that call's
//     own commit (postgres/mssql) or lock release (mysql). A late
//     infrastructure failure in that narrow window -- after the check
//     callback returns nil, before RegisterUnderKeyGate itself returns --
//     makes the overall call fail with the ring already moved. Found and
//     falsified live by cleat-review (killing the gate transaction's
//     connection right after the callback's Reload succeeds); the fix is
//     the same move as step 3, one level in: track whether the swap
//     actually happened, and if RegisterUnderKeyGate still errors, revert
//     the secrets ring too -- so it ends up matching whatever registration
//     actually survived, exactly as a refused check would have left it.
//
// Logged, not returned: this runs off a signal with nothing to report an
// error to except the log and w.Metrics.
//
// A LIMITATION, MEASURED RATHER THAN ASSUMED: under the current
// --encryption-key-file[-previous] design, this can NEVER actually change
// the payload key's BYTES, only confirm them unchanged. loadPayloadKeyRing
// assigns version 2 to whatever --encryption-key-file holds and 1 to
// --encryption-key-file-previous, UNCONDITIONALLY, on every call -- so a
// SIGHUP that re-reads an operator-edited current-key file produces a
// candidate ring whose version 2 carries DIFFERENT bytes than the ring's
// own version 2 a moment ago, which is exactly refuseReusedVersion's job to
// refuse (reloadable_key_ring.go). Verified directly: reloading
// {2: A}-shaped ring with a {2: B}-shaped candidate, no previous key
// present either way, is refused every time.
//
// The contrast with the secrets side is the reason: SecretKeyRingFromEnv's
// M1 rereads the VERSION NUMBER from a file too (CLEAT_SECRET_MASTER_KEY_VERSION_FILE
// and its _PREVIOUS sibling), so an operator rotating secrets introduces a
// version NUMBER the live ring never held before -- a clean add, not a
// reuse. The payload side has no equivalent: --encryption-key-file-version
// does not exist, so every SIGHUP sees the SAME two labels (2 and 1) no
// matter what an operator changes on disk, and content changes under a
// fixed label are exactly what M3 exists to catch.
//
// So, for now: a SIGHUP with the payload key file(s) UNCHANGED is a clean,
// harmless no-op (this is the common case -- an operator rotating secrets
// sends one SIGHUP to every worker, and workers with no payload key change
// pending should not be disrupted by it). A SIGHUP after rewriting the
// CURRENT key's bytes is SAFELY REFUSED -- logged, failure metric
// incremented, old key kept, nothing corrupted -- rather than either
// silently rotating (which would orphan every row sealed under the old
// bytes without converting them first) or crashing. Genuine payload key
// rotation still requires the restart-based two-phase procedure
// (docs/how-to/rotate-payload-encryption-key.md) until a follow-up gives
// the payload ring the same file-sourced version numbers the secrets ring
// already has (tracked separately as cleat#3203).
func (w *Worker) reloadKeyRingsOnSIGHUP(ctx context.Context) {
	logger := w.logger
	workerID := w.id

	record := func(success bool) {
		if w.Metrics != nil {
			w.Metrics.RecordSecretKeyReload(ctx, success)
		}
	}

	// Step 1: the payload ring, if payload encryption is configured on this
	// worker at all. loadPayloadEncryption's doc comment is the reason this
	// is a nil check rather than an error: a worker that never had
	// --encryption-key-file set has no ring to reload, by design, and
	// enabling the feature from nothing is a restart, not a SIGHUP, the
	// same way every other boot-time-only flag on this worker is.
	var revertPayloadTo *engine.KeyRing
	payloadMoved := false
	if w.payloadRing == nil {
		logger.InfoContext(ctx, "SIGHUP: payload encryption is not configured on this worker, nothing to reload for it", "worker_id", workerID)
	} else {
		candidatePayload, err := loadPayloadKeyRing(w.payloadKeyFile, w.payloadKeyFilePrevious)
		if err != nil {
			logger.ErrorContext(ctx, "SIGHUP: failed to read the payload encryption key, keeping the live one", "worker_id", workerID, "error", err)
			record(false)
			return
		}
		revertPayloadTo = w.payloadRing.Load()
		result, err := w.payloadRing.Reload(candidatePayload)
		if err != nil {
			logger.ErrorContext(ctx, "SIGHUP: payload encryption key reload refused, keeping the live one", "worker_id", workerID, "error", err)
			record(false)
			return
		}
		payloadMoved = true
		if len(result.Dropped) > 0 {
			logger.WarnContext(ctx, "SIGHUP: payload encryption key reload dropped a key version -- any value still sealed under it will fail to open on this worker",
				"worker_id", workerID, "dropped_versions", result.Dropped)
		}
	}

	// Step 2: the secrets ring, under the #2160 gate.
	candidateSecrets, err := engine.SecretKeyRingFromEnv(os.Getenv)
	if err != nil {
		logger.ErrorContext(ctx, "SIGHUP: the secret master key configuration is unusable, keeping the live ring", "worker_id", workerID, "error", err)
		revertPayloadRing(ctx, logger, workerID, w.payloadRing, revertPayloadTo, payloadMoved)
		record(false)
		return
	}

	reg := engine.WorkerRegistration{
		WorkerID:          workerID,
		Hostname:          hostnameOrEmpty(),
		Address:           podAddress(hostnameOrEmpty(), *workerServiceName),
		PID:               os.Getpid(),
		Concurrency:       w.concurrency,
		ConnectionBudget:  w.clusterConnectionBudget,
		SecretKeyVersions: candidateSecrets.Versions(),
	}
	// revertSecretsTo/secretsMoved cover a gap cleat-review found and
	// falsified live: RegisterUnderKeyGate runs this callback, including the
	// secretsRing.Reload below, BEFORE its own commit (postgres/mssql) or
	// lock-release (mysql) -- so a late infrastructure failure in that
	// narrow window (the backend connection dying between check-success and
	// commit, say) makes RegisterUnderKeyGate return an error AFTER the ring
	// has already moved. Without tracking that, this code read "checkErr !=
	// nil" as "nothing moved" and reverted only the payload ring, leaving
	// the secrets ring on the candidate while logging "keeping the live
	// ring" -- a real both-or-neither violation, not a hypothetical one.
	var revertSecretsTo *engine.KeyRing
	secretsMoved := false
	var droppedSecrets []int
	checkErr := w.workerRegistry.RegisterUnderKeyGate(ctx, reg, func(cctx context.Context) error {
		if err := checkCandidateOpensEverySecret(cctx, w.secrets, w.deploymentSecrets, candidateSecrets); err != nil {
			return err
		}
		revertSecretsTo = w.secretsRing.Load()
		result, err := w.secretsRing.Reload(candidateSecrets)
		if err != nil {
			return err
		}
		secretsMoved = true
		droppedSecrets = result.Dropped
		if w.afterSecretsRingMovedForTest != nil {
			w.afterSecretsRingMovedForTest()
		}
		return nil
	})
	if checkErr != nil {
		if secretsMoved {
			// The check succeeded and the ring swapped; RegisterUnderKeyGate
			// still failed afterward, which on postgres/mssql can only be the
			// commit and on mysql only the lock release -- the registration
			// that would have advertised the candidate's versions did NOT
			// land, so the ring must go back to match what the registry
			// actually still says, exactly as a refused check would have left it.
			logger.ErrorContext(ctx, "SIGHUP: the secret key reload's check succeeded and the ring already moved, but registering it failed afterward (a late infrastructure failure, not a refused check) -- reverting the ring to match the registration that did not commit",
				"worker_id", workerID, "error", checkErr)
			if revertRing(ctx, logger, workerID, "secrets", w.secretsRing, revertSecretsTo) {
				logger.InfoContext(ctx, "SIGHUP: reverted the secrets ring after its own registration failed to commit", "worker_id", workerID)
			}
		} else {
			logger.ErrorContext(ctx, "SIGHUP: secret key reload refused, keeping the live ring and the previously published key versions",
				"worker_id", workerID, "error", checkErr)
		}
		revertPayloadRing(ctx, logger, workerID, w.payloadRing, revertPayloadTo, payloadMoved)
		record(false)
		return
	}
	if len(droppedSecrets) > 0 {
		logger.WarnContext(ctx, "SIGHUP: secret key reload dropped a key version -- any secret still sealed under it will fail to resolve on this worker",
			"worker_id", workerID, "dropped_versions", droppedSecrets)
	}

	logger.InfoContext(ctx, "SIGHUP: key reload complete", "worker_id", workerID, "secret_key_versions", candidateSecrets.Versions())
	record(true)
}

// revertPayloadRing undoes step 1's swap after step 2 refuses, so the two
// rings end up both-or-neither moved rather than half-moved. A no-op unless
// the payload ring actually moved in this call.
func revertPayloadRing(ctx context.Context, logger *slog.Logger, workerID string, payloadRing *engine.ReloadableKeyRing, revertTo *engine.KeyRing, moved bool) {
	if !moved {
		return
	}
	if revertRing(ctx, logger, workerID, "payload", payloadRing, revertTo) {
		logger.InfoContext(ctx, "SIGHUP: reverted the payload ring after the secrets half refused, so neither moved", "worker_id", workerID)
	}
}

// revertRing swaps ring back to revertTo and reports whether that succeeded.
// Shared by revertPayloadRing above and the secrets-side revert in
// reloadKeyRingsOnSIGHUP, both of which only ever call it with a ring that
// was genuinely live a moment ago -- so Reload's own M3 check can never
// refuse the revert itself.
func revertRing(ctx context.Context, logger *slog.Logger, workerID, label string, ring *engine.ReloadableKeyRing, revertTo *engine.KeyRing) bool {
	if _, err := ring.Reload(revertTo); err != nil {
		// Should never happen: revertTo was live a moment ago, so reverting to
		// it introduces no version/byte conflict with itself. If this somehow
		// fires, this ring is left on the candidate while its counterpart
		// did not move (or already reverted) -- a genuine "both or neither"
		// violation worth paging on, which is why this is its own loud error
		// rather than folded into the caller's own log line.
		logger.ErrorContext(ctx, "SIGHUP: reverting the "+label+" ring failed -- "+
			"it may now disagree with its counterpart about which key reload this was",
			"worker_id", workerID, "error", err)
		return false
	}
	return true
}

// checkCandidateOpensEverySecret is cleat#2298's M4: both the tenant-secrets
// store and the deployment-secrets store must open everything they hold
// under candidate, checked before any swap. Run inside
// WorkerRegistry.RegisterUnderKeyGate's check callback, under the #2160
// gate, alongside the registration that advertises candidate's versions --
// the same pairing checkSecretsUsable already has with registerWithKeyCheck
// at boot, extended here to a ring that is not live yet and to the
// deployment-secrets store, which has no boot-time equivalent of its own.
func checkCandidateOpensEverySecret(ctx context.Context, secretStore *engine.SecretStore, deploymentSecretStore *engine.DeploymentSecretStore, candidate *engine.KeyRing) error {
	chk, err := secretStore.CheckKeyRingCandidate(ctx, candidate)
	if err != nil {
		return fmt.Errorf("checking whether the candidate key ring opens every tenant secret: %w", err)
	}
	if msg := unopenableMessage(chk.Unopenable, chk.Configured); msg != "" {
		return fmt.Errorf("%d tenant secret(s) would become unopenable: %s", sumCounts(chk.Unopenable), msg)
	}
	depChk, err := deploymentSecretStore.CheckKeyRingCandidate(ctx, candidate)
	if err != nil {
		return fmt.Errorf("checking whether the candidate key ring opens every deployment secret: %w", err)
	}
	if msg := unopenableMessage(depChk.Unopenable, depChk.Configured); msg != "" {
		return fmt.Errorf("%d deployment secret(s) would become unopenable: %s", sumCounts(depChk.Unopenable), msg)
	}
	return nil
}

// unopenableMessage describes a non-empty Unopenable map, or returns "" when
// there is nothing to report -- the two checks above only need to know
// whether to refuse, and the NAMES of the stuck versions are what an
// operator needs to fix it, same shape as checkSecretsUsable's own refusal.
func unopenableMessage(unopenable map[int]int, configured []int) string {
	if len(unopenable) == 0 {
		return ""
	}
	versions := make([]int, 0, len(unopenable))
	for v := range unopenable {
		versions = append(versions, v)
	}
	sort.Ints(versions)
	parts := make([]string, 0, len(versions))
	for _, v := range versions {
		parts = append(parts, fmt.Sprintf("%d under key_version %d", unopenable[v], v))
	}
	return fmt.Sprintf("%v, and the candidate ring only carries versions %v", parts, configured)
}

// sumCounts adds up every value in an Unopenable-shaped map.
func sumCounts(m map[int]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
