// This file is FIXTURE, not code. It lives under testdata/ so the Go tool
// does not build it, and it exists to be scanned.
//
// It carries a Secrets.ForTenant call that is deliberately absent from
// secretsForTenantLedger, so the scanner has something it must REPORT rather
// than only something it must not complain about -- same reasoning as
// testdata/pertenant/undeclared_loop.go, for the Secrets.ForTenant shape.
package fixture

import (
	"context"

	"github.com/cleat-team/cleat/plugin"
)

func loopNobodyDeclared(ctx context.Context, secrets plugin.Secrets, tenantID string) (string, error) {
	return secrets.ForTenant(tenantID).Get(ctx, "some-name")
}
