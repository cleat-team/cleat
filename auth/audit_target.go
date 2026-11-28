package auth

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// AuditTarget is where an operator's admin handler says WHICH TENANT it acted on, so the audit log
// can put the record on that tenant's chain. cleat#3559.
//
// It is a slot rather than a value computed from the request because the request does not say. The
// first version parsed /api/admin/tenants/<id>/ out of the path, and cleat-review found two ways
// that was wrong, both measured:
//
//   - The id-only routes (/api/admin/instances/<id>/...) act on whichever tenant the header: tenant
//     resolver put in the context. The path names none, so the row moved from that tenant's chain
//     to the default one -- the chain stopped following the tenant that was acted on.
//   - A path with a random UUID in it named a tenant that does not exist, and audit_events has no
//     foreign key, so the row started a chain for it.
//
// The handler is the only place that knows both which tenant it opened a store for AND that the
// target exists in it, so it records the answer there (NoteAuditTarget, after the workflow is found)
// and the audit middleware reads it back. A request that never confirms a target -- a 404, a
// refusal, health, drain -- leaves the slot empty and is recorded on the default tenant's chain,
// attributed to the operator by the subject.
type AuditTarget struct {
	mu  sync.Mutex
	id  uuid.UUID
	set bool

	// metadata is the JSON document a handler asked to be stored on the row (cleat#3578: the reason an
	// operator gave). Empty means "none", which stores the "{}" every row stored before this existed.
	metadata string
}

type auditTargetKey struct{}

// WithAuditTarget returns a context carrying a new, empty slot, and the slot to read after the
// request has been served. The audit middleware calls it for an operator request.
func WithAuditTarget(ctx context.Context) (context.Context, *AuditTarget) {
	slot := &AuditTarget{}
	return context.WithValue(ctx, auditTargetKey{}, slot), slot
}

// NoteAuditTarget records the tenant the request acted on. It is a no-op when nothing is
// listening (every request but an operator's under the audit log), and the nil UUID is ignored: it
// is the default tenant's id, which is where an unrecorded request goes anyway. The first tenant
// noted wins, so a handler that touches two cannot make the row land on the later one by accident.
func NoteAuditTarget(ctx context.Context, tenant uuid.UUID) {
	slot, ok := ctx.Value(auditTargetKey{}).(*AuditTarget)
	if !ok || tenant == uuid.Nil {
		return
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if !slot.set {
		slot.id, slot.set = tenant, true
	}
}

// Tenant returns the tenant a handler noted, if it noted one.
func (a *AuditTarget) Tenant() (uuid.UUID, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.id, a.set
}

// NoteAuditMetadata records a JSON document for the audit row of this request, stored in the row's
// metadata column and covered by the chain hash like the rest of the row. It is a no-op when nothing is
// listening (every request but an operator's under the audit log). The FIRST document noted wins, as the
// first tenant does, so a handler cannot have a later step overwrite what an earlier one recorded.
//
// The caller owns what goes in. The document must be valid JSON, and must never contain a secret: this
// is the audit log, not a place to put a credential, and nothing here looks. cleat#3578 puts an
// operator's stated reason here and validates it before it gets this far.
func NoteAuditMetadata(ctx context.Context, doc string) {
	slot, ok := ctx.Value(auditTargetKey{}).(*AuditTarget)
	if !ok || doc == "" {
		return
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.metadata == "" {
		slot.metadata = doc
	}
}

// Metadata returns the document a handler noted, or "" when it noted none.
func (a *AuditTarget) Metadata() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.metadata
}
