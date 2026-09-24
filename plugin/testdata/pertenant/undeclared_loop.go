// This file is FIXTURE, not code. It lives under testdata/ so the Go tool does
// not build it, and it exists to be scanned.
//
// It carries a plugin.AllTenantIDs call that is deliberately absent from
// perTenantLoopLedger, so the scanner has something it must REPORT rather
// than only something it must not complain about -- same reasoning as
// testdata/crosstenant/undeclared_sweep.go, for the loop shape.
package fixture

import (
	"context"

	"github.com/cleat-team/cleat/plugin"
)

func loopNobodyDeclared(ctx context.Context, db plugin.PluginDB, dialect plugin.Dialect) ([]string, error) {
	return plugin.AllTenantIDs(ctx, db, dialect)
}
