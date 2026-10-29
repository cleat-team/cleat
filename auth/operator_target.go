package auth

import (
	"strings"

	"github.com/google/uuid"
)

// OperatorTargetTenant reports which tenant an operator's admin request is ABOUT, when its path
// names one: /api/admin/tenants/<id>/... The audit log uses it to put the record on that tenant's
// chain (cleat#3559).
//
// It reads the path the way handleAdminRoutesForTenant does (cmd/cleat-worker/api_admin.go) -- the
// first segment after the prefix, through uuid.Parse -- so the chain a row lands on and the tenant
// the handler acted on cannot be two different tenants. The result is the parsed value, so
// callers get the canonical form whatever spelling arrived (braces, urn:, bare hex).
//
// ok is false for every route that names no tenant (health, drain, the retention sweep, the id-only
// instance routes) and for a segment that is not a UUID, which the handler answers with 400. The
// path is compared exactly, not folded to lower case: the mux is case-sensitive, so a path that
// differs in case reaches no handler and has no target.
func OperatorTargetTenant(path string) (uuid.UUID, bool) {
	rest, found := strings.CutPrefix(path, OperatorRoutePrefix+"tenants/")
	if !found {
		return uuid.Nil, false
	}
	segment, _, hasTail := strings.Cut(rest, "/")
	if !hasTail || segment == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(segment)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}
