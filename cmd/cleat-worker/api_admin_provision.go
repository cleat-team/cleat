package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// POST /api/admin/tenants and POST /api/admin/tenants/{id}/confirm. cleat#3552, B2b-2.
//
// The handshake, and why a create does not simply return a key, is the comment at the top of
// auth/tenant_provision.go. This file is the HTTP half: who may call it, what is accepted, what
// is answered, and what is audited.

// tenantProvisioner is what the routes need from the store. An interface so the handlers can be
// tested without a database, and so a worker that cannot provision (MySQL, sharded) is a nil
// field rather than a branch scattered through the handlers.
type tenantProvisioner interface {
	ProvisionTenant(ctx context.Context, req auth.ProvisionRequest) (auth.ProvisionResult, error)
	ConfirmProvisionedTenant(ctx context.Context, tenantID uuid.UUID, creator string) (auth.ConfirmResult, error)
}

// provisionBodyLimit bounds the request. The largest legitimate body is a few short strings.
const provisionBodyLimit = 64 << 10

type provisionTenantRequest struct {
	TenantID    string `json:"tenant_id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	OrgID       string `json:"org_id"`
	OrgName     string `json:"org_name"`
	Key         struct {
		Description string `json:"description"`
	} `json:"key"`
}

type provisionTenantJSON struct {
	TenantID    string `json:"tenant_id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	OrgID       string `json:"org_id"`
}

type provisionKeyJSON struct {
	KeyID string `json:"key_id"`
	// Secret appears in this response and nowhere else, ever: the database holds its hash.
	Secret          string `json:"secret"`
	Provisional     bool   `json:"provisional"`
	ValidForSeconds int64  `json:"valid_for_seconds"`
	Confirm         string `json:"confirm"`
}

type provisionTenantResponse struct {
	Outcome string              `json:"outcome"`
	Tenant  provisionTenantJSON `json:"tenant"`
	Key     *provisionKeyJSON   `json:"key,omitempty"`
}

// provisionActor identifies who is provisioning, or writes the refusal. The creator recorded on a
// tenant, and the only principal that may replay or confirm its creation, is the operator key's
// principal, or the key itself for a key with none.
//
// An OPERATOR credential is required, and that check is not redundant with the scope gate: a
// TENANT key reaches /api/admin/* too when --enable-admin-api is on (adminAPIOnly's doc comment),
// and it must not be able to create tenants.
func (s *apiServer) provisionActor(w http.ResponseWriter, r *http.Request) (creator string, ok bool) {
	op, isOperator := auth.OperatorFromContext(r.Context())
	if !isOperator {
		s.writeError(w, http.StatusForbidden, "provisioning a tenant requires an operator credential")
		return "", false
	}
	if s.provisioner == nil {
		s.writeError(w, http.StatusNotImplemented,
			"tenant provisioning is not available on this worker: it needs PostgreSQL or SQL Server and a worker that is not sharded")
		return "", false
	}
	if op.Principal != "" {
		return op.Principal, true
	}
	return op.String(), true
}

func (s *apiServer) handleProvisionTenant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	creator, ok := s.provisionActor(w, r)
	if !ok {
		return
	}

	// Bounded by readBody, which answers an oversized body with the 413 that names the limit
	// (TestEveryBoundedBodyGoesThroughTheHelper forbids bounding it here), and then decoded strictly.
	raw, ok := s.readBody(w, r, bodyLimit{provisionBodyLimit, "fixed for the tenant provisioning endpoint and not moved by --max-body-size"})
	if !ok {
		return
	}
	var body provisionTenantRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	// One object and then the end. Decode reads a single value and stops, so without this a second
	// object after the first is accepted and ignored.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		s.writeError(w, http.StatusBadRequest, "invalid request body: unexpected data after the JSON object")
		return
	}
	req := auth.ProvisionRequest{
		Name: body.Name, DisplayName: body.DisplayName, OrgName: body.OrgName,
		KeyDescription: body.Key.Description, Creator: creator,
	}
	if body.TenantID != "" {
		id, err := uuid.Parse(body.TenantID)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "tenant_id must be a UUID")
			return
		}
		if id == uuid.Nil {
			s.writeError(w, http.StatusBadRequest, "tenant_id may not be the nil UUID: it is the default tenant")
			return
		}
		req.TenantID = id // canonical from here on, whatever spelling arrived
	}
	if body.OrgID != "" {
		id, err := uuid.Parse(body.OrgID)
		if err != nil || id == uuid.Nil {
			s.writeError(w, http.StatusBadRequest, "org_id must be a non-nil UUID")
			return
		}
		req.OrgID = id
	}
	if (req.OrgID == uuid.Nil) == (strings.TrimSpace(req.OrgName) == "") {
		s.writeError(w, http.StatusBadRequest, "give exactly one of org_id and org_name")
		return
	}
	req.OrgName = strings.TrimSpace(req.OrgName)

	res, err := s.provisioner.ProvisionTenant(r.Context(), req)
	if err != nil {
		s.writeProvisionError(w, err)
		return
	}
	// Said only once the tenant exists, so the audit row lands on ITS chain with the principal as
	// the subject. A refusal above names no tenant and stays on the default chain (cleat#3559).
	auth.NoteAuditTarget(r.Context(), res.Tenant.ID)
	slog.Warn("admin: provision tenant", "tenant_id", res.Tenant.ID, "outcome", string(res.Outcome),
		"creator", creator, "key_issued", res.Key != nil, "operator", operatorFromContext(r))

	out := provisionTenantResponse{
		Outcome: string(res.Outcome),
		Tenant: provisionTenantJSON{
			TenantID: res.Tenant.ID.String(), Name: res.Tenant.Name,
			DisplayName: res.Tenant.DisplayName, OrgID: res.Tenant.OrgID.String(),
		},
	}
	if res.Key != nil {
		out.Key = &provisionKeyJSON{
			KeyID: res.Key.KeyID.String(), Secret: res.Key.Secret, Provisional: true,
			ValidForSeconds: int64(res.Key.ValidFor.Seconds()),
			Confirm:         "POST /api/admin/tenants/" + res.Tenant.ID.String() + "/confirm",
		}
	}
	status := http.StatusOK
	if res.Outcome == auth.ProvisionCreated {
		status = http.StatusCreated
	}
	// A response that carries a secret is never cached.
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, status, out)
}

// handleConfirmProvisionedTenant makes the creator's provisional key permanent. It is sent LAST,
// after the creator has stored the key durably: that ordering is the whole point.
func (s *apiServer) handleConfirmProvisionedTenant(w http.ResponseWriter, r *http.Request, tenant string) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	creator, ok := s.provisionActor(w, r)
	if !ok {
		return
	}
	tid, err := uuid.Parse(tenant)
	if err != nil || tid == uuid.Nil {
		s.writeError(w, http.StatusBadRequest, "tenant must be a UUID")
		return
	}
	res, err := s.provisioner.ConfirmProvisionedTenant(r.Context(), tid, creator)
	switch {
	case errors.Is(err, auth.ErrTenantNotFound):
		// The same answer for an id that does not exist and for one this principal did not create.
		s.writeError(w, http.StatusNotFound, "not found")
		return
	case errors.Is(err, auth.ErrNoProvisionalKey):
		s.writeJSON(w, http.StatusConflict, map[string]any{
			"error":  err.Error(),
			"reason": "no_provisional_key",
		})
		return
	case err != nil:
		slog.Error("admin: confirm tenant failed", "tenant_id", tid, "error", err)
		s.writeError(w, http.StatusInternalServerError, "confirming the tenant failed")
		return
	}
	auth.NoteAuditTarget(r.Context(), tid)
	slog.Warn("admin: confirm tenant", "tenant_id", tid, "confirmed", res.Confirmed,
		"already_confirmed", res.AlreadyConfirmed, "creator", creator, "operator", operatorFromContext(r))
	s.writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": tid.String(), "confirmed": res.Confirmed, "already_confirmed": res.AlreadyConfirmed,
	})
}

func (s *apiServer) writeProvisionError(w http.ResponseWriter, err error) {
	var conflict *auth.ProvisionConflictError
	switch {
	case errors.As(err, &conflict):
		body := map[string]any{"error": conflict.Error(), "reason": conflict.Reason}
		if len(conflict.Fields) > 0 {
			body["fields"] = conflict.Fields
		}
		s.writeJSON(w, http.StatusConflict, body)
	case errors.Is(err, auth.ErrOrgNotFound):
		s.writeError(w, http.StatusNotFound, "org not found")
	default:
		// The detail goes to the log: a database error can name a table, a constraint or a value.
		slog.Error("admin: provision tenant failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "provisioning the tenant failed")
	}
}

// tenantProvisionerFor returns the provisioner a worker should serve, or nil when it cannot.
//
// MySQL is single-tenant by decision (tiers.yaml), so there is nothing to provision into; a
// sharded worker is refused because the rotating claim that serves many tenants is unavailable
// on it, so a tenant created there would never run anything. Nil makes both routes answer 501
// with that explanation, rather than 500 from a statement that cannot succeed.
func tenantProvisionerFor(driver string, store any, resolver *auth.TenantStore) tenantProvisioner {
	if resolver == nil {
		return nil
	}
	if driver != auth.DialectPostgres && driver != auth.DialectMSSQL {
		return nil
	}
	if _, sharded := store.(*engine.ShardedStore); sharded {
		return nil
	}
	return resolver
}
