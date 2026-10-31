package main

// cleat#3578: POST /api/admin/tenants/{tenant}/recovery-key. An operator mints a key for a tenant that
// has locked itself out.
//
// A tenant can revoke its own last key (DELETE /api/keys/{key_id}?force=true) and then has no credential
// that reaches /api/keys. The offline route, `cleat-worker --generate-api-key`, needs a shell on a worker,
// which a service acting for many tenants does not have. This is the same act over HTTP, under the rules
// the issue's accepted design sets:
//
//  1. ADMIN-SCOPED OPERATOR KEY ONLY. A provision-scoped key is refused by the scope gate (the route is
//     not on its allow-list) and again here, and a tenant key never reaches it. Whether a provision key
//     may recover a tenant its principal created is the owner's open question on the issue; it is
//     additive later.
//  2. THE TENANT ID IS TYPED BACK in X-Confirm, checked before anything is read.
//  3. A REASON IS REQUIRED: non-empty after trimming, at most maxRecoveryReason characters, no control
//     characters, and nothing shaped like an API key. It is recorded in the audit row on the TARGET
//     tenant's chain (hash-covered), so the tenant can read, in its own log, who recovered it and why.
//     A reason that looks like a key is refused with a 400 that does not echo it: a credential pasted
//     into a free-text field must not end up in the audit chain for good, or in a log.
//  4. THE STORE DECIDES whether the tenant is locked out: it has no live key, read under the tenant's row
//     lock in the transaction that mints (auth.TenantStore.MintRecoveryKey). Not found, a refusal for a
//     live key, and a refusal for a suspended tenant are the answers; nothing is written on any of them.
//  5. THE KEY IS PROVISIONAL (15 minutes) and the reply carries it once with Cache-Control: no-store. The
//     tenant's documented path is POST /api/keys with it to mint a permanent key, then DELETE /api/keys/
//     {key_id} to revoke the recovery one. This route does not rely on the confirm route.
//
// THE SECRET appears in the reply and nowhere else: not in the audit row, not in a log line. The audit
// metadata carries the reason and the new key's ID, which is not a secret.
//
// WHAT THIS DOES NOT STOP, and the docs say it too: an admin can recover a tenant that revoked its keys
// on purpose. That is not new power (the offline route gives a shell-holder the same, and /api/admin
// already reaches every tenant's runs); what the route changes is that it is reachable over HTTP by a
// credential that can be stolen, which is why the scope and the precondition matter.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
)

const (
	recoveryKeyBodyLimit = 4 << 10
	maxRecoveryReason    = 500
)

// recoveryKeyMinter is what the route needs beyond tenantProvisioner. It is asserted, not added to that
// interface, so a worker whose provisioner cannot do this answers 501 rather than every fake of the
// interface having to grow a method.
type recoveryKeyMinter interface {
	MintRecoveryKey(ctx context.Context, tenantID uuid.UUID, description string) (auth.RecoveryKey, error)
}

type recoveryKeyRequest struct {
	Reason string `json:"reason"`
}

// validateRecoveryReason returns the trimmed reason, or the sentence that says what is wrong with it. The
// sentences never repeat the reason: it is the field most likely to hold something it should not.
func validateRecoveryReason(raw string) (string, string) {
	reason := strings.TrimSpace(raw)
	switch {
	case reason == "":
		return "", "reason is required: say why this tenant is being recovered"
	case utf8.RuneCountInString(reason) > maxRecoveryReason:
		return "", "reason is too long: at most 500 characters"
	case !utf8.ValidString(reason):
		return "", "reason must be valid UTF-8"
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return "", "reason must not contain control characters"
		}
	}
	lower := strings.ToLower(reason)
	if strings.Contains(lower, strings.ToLower(auth.OperatorKeyPrefix)) || strings.Contains(lower, "cleat_sk_") {
		return "", "reason must not contain an API key"
	}
	return reason, ""
}

func (s *apiServer) handleRecoveryKey(w http.ResponseWriter, r *http.Request, tenant string) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.provisionActor(w, r); !ok {
		return
	}
	op, _ := auth.OperatorFromContext(r.Context())
	if !op.IsAdmin() {
		s.writeError(w, http.StatusForbidden, "minting a recovery key requires an admin-scoped operator key")
		return
	}
	minter, ok := s.provisioner.(recoveryKeyMinter)
	if !ok {
		s.writeError(w, http.StatusNotImplemented, "recovery keys are not available on this worker")
		return
	}
	tid, err := uuid.Parse(tenant)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "tenant must be a UUID")
		return
	}
	if tid == uuid.Nil {
		s.writeError(w, http.StatusBadRequest, "the default tenant has no recovery key")
		return
	}
	if r.Header.Get("X-Confirm") != tid.String() {
		s.writeError(w, http.StatusBadRequest, "X-Confirm header must be the id of the tenant being recovered")
		return
	}

	raw, ok := s.readBody(w, r, bodyLimit{recoveryKeyBodyLimit, "fixed for the recovery key endpoint and not moved by --max-body-size"})
	if !ok {
		return
	}
	var body recoveryKeyRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body: expected {\"reason\": \"...\"}")
		return
	}
	if dec.More() {
		s.writeError(w, http.StatusBadRequest, "invalid request body: one JSON object expected")
		return
	}
	reason, problem := validateRecoveryReason(body.Reason)
	if problem != "" {
		s.writeError(w, http.StatusBadRequest, problem)
		return
	}

	key, err := minter.MintRecoveryKey(r.Context(), tid, "recovery key minted by an operator")
	var refused *auth.RecoveryRefusedError
	switch {
	case errors.Is(err, auth.ErrTenantNotFound):
		s.writeError(w, http.StatusNotFound, "not found")
		return
	case errors.As(err, &refused):
		// The tenant exists (the store found and locked it), so the attempt is recorded on ITS chain with
		// the reason and what refused it. The tenant can see that an operator tried.
		s.noteRecoveryAudit(r, tid, map[string]any{"reason": reason, "refused": refused.Reason})
		resp := map[string]any{"error": refused.Error(), "reason": refused.Reason}
		if refused.Reason == auth.RecoveryRefusedLiveKeys {
			resp["live_keys"] = refused.LiveKeys
		}
		s.writeJSON(w, http.StatusConflict, resp)
		return
	case err != nil:
		// The detail goes to the log: a database error can name a table, a constraint or a value.
		slog.Error("admin: mint recovery key failed", "tenant_id", tid, "error", err)
		s.writeError(w, http.StatusInternalServerError, "minting the recovery key failed")
		return
	}

	s.noteRecoveryAudit(r, tid, map[string]any{"reason": reason, "key_id": key.KeyID.String()})
	// The secret is NOT logged. The reason is, because it was validated not to look like a key.
	slog.Warn("admin: minted a recovery key", "tenant_id", tid, "key_id", key.KeyID, "reason", reason,
		"operator", operatorFromContext(r))
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"tenant_id":         tid.String(),
		"key_id":            key.KeyID.String(),
		"secret":            key.Secret,
		"provisional":       true,
		"expires_at":        time.Now().Add(key.ValidFor).UTC().Format(time.RFC3339),
		"valid_for_seconds": int(key.ValidFor / time.Second),
		"next": "Use this key to POST /api/keys for a permanent key, then DELETE /api/keys/" + key.KeyID.String() +
			" to revoke this one. It lapses unused; the route can then be called again.",
	})
}

// noteRecoveryAudit puts the request on the target tenant's chain with the document. The document is built
// with encoding/json so a reason containing quotes or braces cannot change its shape.
func (s *apiServer) noteRecoveryAudit(r *http.Request, tid uuid.UUID, doc map[string]any) {
	auth.NoteAuditTarget(r.Context(), tid)
	if b, err := json.Marshal(doc); err == nil {
		auth.NoteAuditMetadata(r.Context(), string(b))
	}
}
