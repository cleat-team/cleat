// This file is FIXTURE, not code. It lives under testdata/ so the Go tool
// does not build it.
//
// It carries a correct call -- ok is used, not discarded -- so the scanner
// must report ZERO sites here. Without this control, a matcher broad enough
// to catch every 2-value assignment near a TenantIDFromContext/Request call
// would flag the FIX itself, which is indistinguishable from working unless
// something asserts it does not. cleat#2183.
package fixture

import (
	"net/http"

	"github.com/cleat-team/cleat/auth"
)

func handlerDoneCorrectly(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		http.Error(w, "tenant required", http.StatusUnauthorized)
		return
	}
	_ = tid
}
