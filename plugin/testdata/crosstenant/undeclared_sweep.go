// This file is FIXTURE, not code. It lives under testdata/ so the Go tool does
// not build it, and it exists to be scanned.
//
// It carries a cross-tenant bypass that is deliberately absent from the ledger,
// and one whose reason is deliberately not a string literal. A guard that
// passes on a clean tree is satisfied by every broken version of itself; these
// are the cases already known to be wrong, so the scanner has something it must
// REPORT rather than only something it must not complain about.
package fixture

import (
	"context"
	"fmt"

	"github.com/cleat-team/cleat/plugin"
)

func sweepNobodyDeclared(ctx context.Context) context.Context {
	return plugin.AcrossAllTenants(ctx, "fixture: this site is deliberately undeclared")
}

func reasonAssembledAtRuntime(ctx context.Context, n int) context.Context {
	return plugin.AcrossAllTenants(ctx, fmt.Sprintf("fixture: %d tenants", n))
}
