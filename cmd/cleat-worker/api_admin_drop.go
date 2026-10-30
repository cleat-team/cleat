package main

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
)

// handleDropTenant serves DELETE /api/admin/tenants/{tenant} (cleat#3552, B3b-2): it removes a tenant and
// everything it owns, and leaves a tombstone. This is the destructive step of the tenant-provisioning design.
//
// It is refused unless every one of these holds, and the order is the order they are checked in:
//
//  1. the caller is an operator, and its scope reaches the route (the scope gate, before this runs);
//  2. the tenant id in the path is a UUID and not the default tenant;
//  3. the X-Confirm header is that same id, typed back. A header and not a query parameter, because query
//     strings are logged; the same shape force-complete uses. It is checked before anything is read, so a
//     malformed request reveals nothing about a tenant;
//  4. the tenant exists and the caller has a claim to it: an admin key has one to any tenant, a
//     provision-scoped key only to a tenant its principal created. A tenant that does not exist, one the
//     caller did not create and one made offline all answer the same 404;
//  5. the tenant is suspended (409 not_suspended), so that no new work is arriving;
//  6. the tenant has no unsettled run (409 unsettled_runs, with the count): deleting a run that is
//     executing makes its next heartbeat fail on a fence it no longer has.
//
// 5 and 6 are checked inside the same transaction as the drop, under a lock on the tenant's row, so a resume
// cannot slip between the check and the delete. The drop writes a tombstone atomically with itself (see
// the tombstone migration), which is what makes a repeat answer 200 already_dropped, and what stops the id being
// reused unless an operator deletes the tombstone.
//
// A PROVISION-SCOPED KEY MAY DROP A TENANT ITS PRINCIPAL CREATED. That is an OWNER DECISION THAT IS STILL
// PENDING (cleat#3552): the design comment said a drop would be operator-only, the instruction that followed
// was "the same callers as suspend", and this is the second. The scope gate and the matrix row in
// auth/a_scoped_operator_key_reaches_only_its_routes_test.go say the same, so the three agree; if the answer
// is no, those two lines are the change.
//
// IT DOES NOT CALL NoteAuditTarget, on purpose. The audit log puts an operator's request on the chain of
// the tenant the handler names, and this tenant's chain is deleted with it; naming it would put the record
// of the drop on a chain that no longer exists, or start a new chain for a dead id. With no target the row
// goes on the DEFAULT tenant's chain, attributed to the operator, where it survives. The tombstone records
// where the dropped chain ended.
//
// THE AUDIT ROW IS NOT ATOMIC WITH THE DROP. The audit middleware enqueues it after this handler returns,
// so a crash between the commit and the enqueue loses it. The tombstone is the atomic record.
//
// What the reply does not say is that the worker has finished with the tenant. The egress allowlist is
// cached per tenant and a dropped tenant's entry is honoured until its TTL (30 s by default) runs out,
// after which the lookup finds nothing and denies. Nothing else a worker holds for the tenant is acted on
// once its keys and rows are gone, but this is the one place a dropped tenant keeps working for a moment.
func (s *apiServer) handleDropTenant(w http.ResponseWriter, r *http.Request, tenant string) {
	if r.Method != http.MethodDelete {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	actor, ok := s.provisionActor(w, r)
	if !ok {
		return
	}
	tid, err := uuid.Parse(tenant)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "tenant must be a UUID")
		return
	}
	if tid == uuid.Nil {
		s.writeError(w, http.StatusBadRequest, "the default tenant cannot be dropped")
		return
	}
	if r.Header.Get("X-Confirm") != tid.String() {
		s.writeError(w, http.StatusBadRequest, "X-Confirm header must be the id of the tenant being dropped")
		return
	}

	op, _ := auth.OperatorFromContext(r.Context())
	req := auth.DropRequest{TenantID: tid, DroppedBy: actor, Schema: s.schema}
	if !op.IsAdmin() {
		req.OnlyCreator = actor
	}

	res, err := s.provisioner.DropTenant(r.Context(), req)
	var refused *auth.DropRefusedError
	switch {
	case errors.Is(err, auth.ErrTenantNotFound):
		s.writeError(w, http.StatusNotFound, "not found")
		return
	case errors.As(err, &refused):
		body := map[string]any{"error": refused.Error(), "reason": refused.Reason}
		if refused.Reason == auth.DropRefusedUnsettledRuns {
			body["unsettled_runs"] = refused.Unsettled
		}
		s.writeJSON(w, http.StatusConflict, body)
		return
	case err != nil:
		// The detail goes to the log: a database error can name a table, a constraint or a value.
		slog.Error("admin: drop tenant failed", "tenant_id", tid, "error", err)
		s.writeError(w, http.StatusInternalServerError, "dropping the tenant failed")
		return
	}

	t := res.Tombstone
	slog.Warn("admin: dropped tenant", "tenant_id", tid, "name", t.Name, "dropped_by", t.DroppedBy,
		"already_dropped", res.AlreadyDropped, "admin", op.IsAdmin(), "operator", operatorFromContext(r))
	body := map[string]any{
		"tenant_id":       tid.String(),
		"dropped":         true,
		"already_dropped": res.AlreadyDropped,
		"dropped_at":      t.DroppedAt.UTC().Format("2006-01-02T15:04:05.000000Z07:00"),
		"dropped_by":      t.DroppedBy,
		"audit_chain":     nil,
	}
	if t.AuditChainSeq != nil && t.AuditChainHash != nil {
		body["audit_chain"] = map[string]any{"seq": *t.AuditChainSeq, "hash": *t.AuditChainHash}
	}
	s.writeJSON(w, http.StatusOK, body)
}
