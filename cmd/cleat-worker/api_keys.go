package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
)

// A tenant's own API keys: POST/GET /api/keys and DELETE /api/keys/{key_id}. cleat#3552, B2c.
//
// This is the TENANT plane. The caller is authenticated by a tenant key and acts on its own tenant's
// keys only; an operator credential cannot reach it (it is confined to /api/admin/*), which is the
// design's rule that rotation and revocation belong to the tenant and not to whoever created it.
// Rotate = mint, deploy, revoke the old one.

// tenantKeyManager is what the routes need from the store; an interface so the handlers are testable
// without a database.
type tenantKeyManager interface {
	MintKey(ctx context.Context, tenantID uuid.UUID, description string, expiresAt *time.Time) (auth.TenantKey, string, error)
	ListKeys(ctx context.Context, tenantID uuid.UUID) ([]auth.TenantKey, error)
	RevokeKey(ctx context.Context, tenantID, keyID uuid.UUID, force bool) (auth.RevokeResult, error)
}

const (
	keyBodyLimit      = 16 << 10
	maxKeyDescription = 255
)

type mintKeyRequest struct {
	Description string `json:"description"`
	ExpiresAt   string `json:"expires_at"`
}

type tenantKeyJSON struct {
	KeyID       string  `json:"key_id"`
	Description string  `json:"description"`
	CreatedAt   string  `json:"created_at"`
	ExpiresAt   *string `json:"expires_at"`
	DisabledAt  *string `json:"disabled_at"`
	Provisional bool    `json:"provisional"`
	Status      string  `json:"status"`
	// Secret is set in the response to a mint and nowhere else: the database holds its hash.
	Secret string `json:"secret,omitempty"`
}

func keyJSON(k auth.TenantKey, secret string, now time.Time) tenantKeyJSON {
	ts := func(t *time.Time) *string {
		if t == nil {
			return nil
		}
		s := t.UTC().Format(time.RFC3339)
		return &s
	}
	return tenantKeyJSON{
		KeyID: k.KeyID.String(), Description: k.Description, CreatedAt: k.CreatedAt.UTC().Format(time.RFC3339),
		ExpiresAt: ts(k.ExpiresAt), DisabledAt: ts(k.DisabledAt), Provisional: k.Provisional,
		Status: k.Status(now), Secret: secret,
	}
}

// keyCaller returns the tenant whose keys are being managed, or writes the refusal.
//
// AUTHENTICATION MUST BE ON. With --require-auth=false the tenant in the context is not proved by
// anything (the header: resolver reads it from a request header), and these routes would let anyone who
// can reach the port mint a working key for any tenant. So they refuse, rather than trust a tenant that
// nothing authenticated.
func (s *apiServer) keyCaller(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	if s.keys == nil {
		s.writeError(w, http.StatusNotImplemented, "API key management is not available on this worker")
		return uuid.Nil, false
	}
	if !s.requireAuth {
		s.writeError(w, http.StatusForbidden, "API key management needs authentication: start the worker with --require-auth")
		return uuid.Nil, false
	}
	tid, ok := auth.TenantIDFromContext(r.Context())
	if !ok {
		s.writeError(w, http.StatusUnauthorized, "authentication required: provide a tenant API key")
		return uuid.Nil, false
	}
	return tid, true
}

// handleKeys serves the collection: GET lists, POST mints.
func (s *apiServer) handleKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListKeys(w, r)
	case http.MethodPost:
		s.handleMintKey(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *apiServer) handleListKeys(w http.ResponseWriter, r *http.Request) {
	tid, ok := s.keyCaller(w, r)
	if !ok {
		return
	}
	keys, err := s.keys.ListKeys(r.Context(), tid)
	if err != nil {
		slog.Error("api keys: list failed", "tenant_id", tid, "error", err)
		s.writeError(w, http.StatusInternalServerError, "listing the API keys failed")
		return
	}
	now := time.Now()
	out := make([]tenantKeyJSON, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyJSON(k, "", now))
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *apiServer) handleMintKey(w http.ResponseWriter, r *http.Request) {
	tid, ok := s.keyCaller(w, r)
	if !ok {
		return
	}
	raw, ok := s.readBody(w, r, bodyLimit{keyBodyLimit, "fixed for the API key endpoint and not moved by --max-body-size"})
	if !ok {
		return
	}
	var body mintKeyRequest
	// An empty body is a mint with no description and no expiry; anything present must be one object.
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			s.writeError(w, http.StatusBadRequest, "invalid request body: unexpected data after the JSON object")
			return
		}
	}
	if len(body.Description) > maxKeyDescription {
		s.writeError(w, http.StatusBadRequest, "description is longer than 255 bytes")
		return
	}
	var expires *time.Time
	if body.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "expires_at must be an RFC 3339 timestamp, for example 2027-01-02T15:04:05Z")
			return
		}
		if !t.After(time.Now()) {
			s.writeError(w, http.StatusBadRequest, "expires_at must be in the future")
			return
		}
		expires = &t
	}
	key, secret, err := s.keys.MintKey(r.Context(), tid, body.Description, expires)
	switch {
	case errors.Is(err, auth.ErrKeyLimit):
		s.writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "this tenant already holds the maximum number of live API keys (" + strconv.Itoa(auth.MaxLiveTenantKeys) + "): revoke one first",
			"reason": "key_limit",
		})
		return
	case errors.Is(err, auth.ErrTenantNotFound):
		s.writeError(w, http.StatusNotFound, "tenant not found")
		return
	case err != nil:
		slog.Error("api keys: mint failed", "tenant_id", tid, "error", err)
		s.writeError(w, http.StatusInternalServerError, "minting the API key failed")
		return
	}
	slog.Info("api keys: minted", "tenant_id", tid, "key_id", key.KeyID, "expires", expires != nil)
	// A response that carries a secret is never cached.
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusCreated, map[string]any{"key": keyJSON(key, secret, time.Now())})
}

// handleKeyByID serves DELETE /api/keys/{key_id}.
func (s *apiServer) handleKeyByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tid, ok := s.keyCaller(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	keyID, err := uuid.Parse(id)
	if err != nil || keyID == uuid.Nil || strings.Contains(id, "/") {
		s.writeError(w, http.StatusBadRequest, "key_id must be the UUID of one of this tenant's keys")
		return
	}
	force := false
	if v := r.URL.Query().Get("force"); v != "" {
		force, err = strconv.ParseBool(v)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "force must be true or false")
			return
		}
	}
	res, err := s.keys.RevokeKey(r.Context(), tid, keyID, force)
	switch {
	case errors.Is(err, auth.ErrKeyNotFound):
		// The same answer for a key id that does not exist and for another tenant's.
		s.writeError(w, http.StatusNotFound, "not found")
		return
	case errors.Is(err, auth.ErrLastKey):
		s.writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "that is this tenant's last live API key: mint its replacement first, or pass ?force=true to revoke it anyway and lock the tenant out",
			"reason": "last_key",
		})
		return
	case err != nil:
		slog.Error("api keys: revoke failed", "tenant_id", tid, "key_id", keyID, "error", err)
		s.writeError(w, http.StatusInternalServerError, "revoking the API key failed")
		return
	}
	slog.Info("api keys: revoked", "tenant_id", tid, "key_id", keyID, "already_revoked", res.AlreadyRevoked, "force", force)
	s.writeJSON(w, http.StatusOK, map[string]any{"key_id": keyID.String(), "revoked": true, "already_revoked": res.AlreadyRevoked})
}
