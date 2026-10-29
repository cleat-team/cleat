package main

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
)

// handleSetTenantSuspended serves POST /api/admin/tenants/{tenant}/suspend and /resume (cleat#3552, B3a).
//
// Suspending a tenant stops new work for it (see cleatctl suspend-tenant for exactly what it does and does
// not stop); it deletes nothing and resume undoes it. It is here because dropping a tenant requires the
// tenant to be suspended first, and a service that creates tenants must be able to take that step for the
// ones it made without holding a key that can suspend anyone's.
//
// Who may call:
//   - an admin-scoped operator key: any tenant;
//   - any other operator key (a provision-scoped one): SUSPEND ONLY, and only a tenant its principal
//     created, the check the confirm route makes. It cannot resume: the flag is one bit and records who set
//     it, so a provisioner that could clear it could lift a suspension an operator made (cleat#3582 review);
//   - a tenant key never. A tenant key reaches /api/admin/* when --enable-admin-api is on, and a tenant
//     must not be able to suspend itself or another tenant.
//
// A tenant that does not exist, one the caller did not create and one made offline all answer 404, the
// same body, so the answer confirms nothing about a tenant the caller has no claim to.
func (s *apiServer) handleSetTenantSuspended(w http.ResponseWriter, r *http.Request, tenant string, suspended bool) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	creator, ok := s.provisionActor(w, r)
	if !ok {
		return
	}
	tid, err := uuid.Parse(tenant)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "tenant must be a UUID")
		return
	}

	// provisionActor has already established that this is an operator credential.
	op, _ := auth.OperatorFromContext(r.Context())
	switch {
	case op.IsAdmin():
		err = s.provisioner.SetTenantSuspended(r.Context(), tid, suspended)
	case suspended:
		err = s.provisioner.SuspendTenantByCreator(r.Context(), tid, creator)
	default:
		// The scope gate does not admit resume to a non-admin key, so this is unreachable through the
		// chain. It is here so that a change to the gate cannot turn into a provisioner lifting an
		// operator's suspension: the handler refuses on its own.
		s.writeError(w, http.StatusForbidden, "resuming a tenant requires an admin operator key")
		return
	}
	switch {
	case errors.Is(err, auth.ErrTenantNotFound):
		s.writeError(w, http.StatusNotFound, "not found")
		return
	case err != nil:
		slog.Error("admin: suspend tenant failed", "tenant_id", tid, "suspended", suspended, "error", err)
		s.writeError(w, http.StatusInternalServerError, "changing the tenant's suspension failed")
		return
	}
	// Said only once the tenant is known to exist, so the audit row lands on ITS chain.
	auth.NoteAuditTarget(r.Context(), tid)
	slog.Warn("admin: set tenant suspended", "tenant_id", tid, "suspended", suspended, "admin", op.IsAdmin(), "operator", operatorFromContext(r))
	s.writeJSON(w, http.StatusOK, map[string]any{"tenant_id": tid.String(), "suspended": suspended})
}
