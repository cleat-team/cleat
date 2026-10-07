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
//  2. The SECRETS ring is validated and swapped second, under the shared
//     gate, which runs M4 (every stored tenant AND deployment secret must
//     open under the candidate) before the swap and republishes
//     admin.workers.secret_key_versions as part of the same call.
//  3. If step 2 fails for any reason, step 1's swap is REVERTED -- back to
//     the exact ring that was live before this call, which Reload's own M3
//     check can never refuse (reverting introduces no version/byte
//     conflict with itself). This closes the window to one failed gate
//     call's worth of time, during which nothing outside this process
//     could have observed or acted on the payload ring having moved.
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
	var droppedSecrets []int
	checkErr := w.workerRegistry.RegisterUnderKeyGate(ctx, reg, func(cctx context.Context) error {
		if err := checkCandidateOpensEverySecret(cctx, w.secrets, w.deploymentSecrets, candidateSecrets); err != nil {
			return err
		}
		result, err := w.secretsRing.Reload(candidateSecrets)
		if err != nil {
			return err
		}
		droppedSecrets = result.Dropped
		return nil
	})
	if checkErr != nil {
		logger.ErrorContext(ctx, "SIGHUP: secret key reload refused, keeping the live ring and the previously published key versions",
			"worker_id", workerID, "error", checkErr)
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
	if _, err := payloadRing.Reload(revertTo); err != nil {
		// Should never happen: revertTo was live a moment ago, so reverting to
		// it introduces no version/byte conflict with itself. If this somehow
		// fires, the payload ring is left on the candidate while the secrets
		// ring stayed on the old one -- a genuine "both or neither" violation
		// worth paging on, which is why this is its own loud error rather than
		// folded into the refusal log line above.
		logger.ErrorContext(ctx, "SIGHUP: reverting the payload ring after a secrets-side refusal itself failed -- "+
			"the payload ring may now disagree with the secrets ring about which key reload this was",
			"worker_id", workerID, "error", err)
		return
	}
	logger.InfoContext(ctx, "SIGHUP: reverted the payload ring after the secrets half refused, so neither moved", "worker_id", workerID)
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
