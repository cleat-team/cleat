// This file is FIXTURE, not code. It lives under testdata/ so the Go tool
// does not build it, and it exists to be scanned.
//
// It carries a discarded-ok call to auth.TenantIDFromContext that is
// deliberately absent from tenantOkDiscardLedger, so the scanner has
// something it must REPORT rather than only something it must not complain
// about -- same reasoning as testdata/secretsfortenant/undeclared.go, for
// the tenant-ok-discard shape. cleat#2183.
package fixture

import (
	"net/http"

	"github.com/cleat-team/cleat/auth"
	"github.com/google/uuid"
)

func handlerNobodyDeclared(w http.ResponseWriter, r *http.Request) {
	tid, _ := auth.TenantIDFromContext(r.Context())
	if tid == uuid.Nil {
		http.Error(w, "tenant required", http.StatusUnauthorized)
	}
}
