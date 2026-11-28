package main

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/cleat-team/cleat/auth"
	"github.com/google/uuid"
)

// tombstoneTimeFormat is the drop reply's timestamp format, so the two replies describe one instant the same way.
const tombstoneTimeFormat = "2006-01-02T15:04:05.000000Z07:00"

// handleDeleteTombstone serves DELETE /api/admin/tenants/{tenant}/tombstone (cleat#3552, B3b-2b): it removes a
// dropped tenant's tombstone, which is the one act that lets that id be provisioned again. Until it runs,
// `POST /api/admin/tenants` for the id answers 409 tenant_id_dropped.
//
// ADMIN-ONLY, and a provision-scoped key can never reach it. That is the point of making reuse a separate
// act and not a flag on create: a service that can create tenants must not be able to recreate an id that was
// dropped, because what the worker, its caches and the logs still hold under that id would be inherited by
// whoever gets it. Two layers say so and neither is trusted to be the only one: provisionMayReach admits no
// DELETE with a tail (auth/operator_scope.go), so the key is refused with 403 before this runs, and this
// handler asks op.IsAdmin() itself, so a gate that someone later widens does not widen this.
//
// The order of the checks is the drop's:
//
//  1. an operator credential, on a worker that can provision (the same answer the other admin routes give);
//  2. an admin-scoped key;
//  3. a UUID, which is not the nil one;
//  4. the X-Confirm header is that same id, typed back. A header and not a query parameter, because query
//     strings are logged. It is checked before anything is read, so a malformed request reveals nothing.
//
// IT DISCARDS THE CHAIN ANCHOR. The tombstone is the only record of where the dropped tenant's own audit chain
// ended; the chain itself went with the tenant. So the reply carries EVERYTHING the row held, and the worker logs
// the same fields at WARN, and the operator is expected to archive the reply. The audit row for this request
// records that it happened (the operator, the path naming the tenant, the status, the time) and not the
// contents: an audit row has no field for them. Option A of three, decided at coordinator level on cleat#3552;
// the alternatives (a retained deletion table, or a detail field on the audit row) were not built.
//
// A REPEAT IS 404, NOT 200. Deleting a tombstone is a deliberate one-off, not a saga step, and once the id has
// been reused it may belong to a live tenant, so an "already deleted" 200 would say something untrue about it.
//
// Like the drop it calls no NoteAuditTarget: the tenant is gone, so the row goes on the default tenant's chain,
// attributed to the operator, with the dropped tenant's id in the path.
func (s *apiServer) handleDeleteTombstone(w http.ResponseWriter, r *http.Request, tenant string) {
	if r.Method != http.MethodDelete {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	actor, ok := s.provisionActor(w, r)
	if !ok {
		return
	}
	op, _ := auth.OperatorFromContext(r.Context())
	if !op.IsAdmin() {
		s.writeError(w, http.StatusForbidden, "deleting a tombstone requires an admin-scoped operator key")
		return
	}
	tid, err := uuid.Parse(tenant)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "tenant must be a UUID")
		return
	}
	if tid == uuid.Nil {
		s.writeError(w, http.StatusBadRequest, "the default tenant has no tombstone")
		return
	}
	if r.Header.Get("X-Confirm") != tid.String() {
		s.writeError(w, http.StatusBadRequest, "X-Confirm header must be the id of the tenant whose tombstone is being deleted")
		return
	}

	t, err := s.provisioner.DeleteTombstone(r.Context(), tid)
	switch {
	case errors.Is(err, auth.ErrTombstoneNotFound):
		s.writeError(w, http.StatusNotFound, "there is no tombstone for that tenant id")
		return
	case err != nil:
		// The detail goes to the log: a database error can name a table, a constraint or a value.
		slog.Error("admin: delete tombstone failed", "tenant_id", tid, "error", err)
		s.writeError(w, http.StatusInternalServerError, "deleting the tombstone failed")
		return
	}

	// The same fields as the reply. They are observability data and not secrets (cleat D49-1a): ids, a name, a
	// chain position and a hash. The log is the second place they survive, after the reply, because this is the
	// last time they exist.
	attrs := []any{"tenant_id", tid, "name", t.Name, "org_id", t.OrgID, "created_by", t.CreatedBy,
		"dropped_by", t.DroppedBy, "dropped_at", t.DroppedAt.UTC().Format(tombstoneTimeFormat),
		"deleted_by", actor, "operator", operatorFromContext(r)}
	if t.AuditChainSeq != nil && t.AuditChainHash != nil {
		attrs = append(attrs, "audit_chain_seq", *t.AuditChainSeq, "audit_chain_hash", *t.AuditChainHash)
	}
	slog.Warn("admin: deleted a tenant tombstone; the id can be provisioned again and the audit chain anchor is gone", attrs...)

	s.writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id":         tid.String(),
		"tombstone_deleted": true,
		"tombstone":         tombstoneJSON(t),
	})
}

// tombstoneJSON is everything a tombstone row holds, under the names the drop's reply uses where it has them.
func tombstoneJSON(t auth.Tombstone) map[string]any {
	body := map[string]any{
		"tenant_id":   t.TenantID.String(),
		"name":        t.Name,
		"org_id":      t.OrgID.String(),
		"created_by":  nil,
		"dropped_by":  t.DroppedBy,
		"dropped_at":  t.DroppedAt.UTC().Format(tombstoneTimeFormat),
		"audit_chain": nil,
	}
	if t.CreatedBy != "" {
		body["created_by"] = t.CreatedBy
	}
	if t.AuditChainSeq != nil && t.AuditChainHash != nil {
		body["audit_chain"] = map[string]any{"seq": *t.AuditChainSeq, "hash": *t.AuditChainHash}
	}
	return body
}
