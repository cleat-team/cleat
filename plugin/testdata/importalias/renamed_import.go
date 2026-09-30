// FIXTURE, not code. Under testdata/ so the Go tool does not build it; it
// exists to be scanned. cleat#2740 (F).
//
// Imports cleat/plugin under a non-default alias and calls ForTenant through
// it. Every ledger scanner in plugin_test hardcodes the literal package
// identifier "plugin", so this shape must be refused by
// assertDefaultPluginImportName rather than silently mis-scanned -- before
// this fixture, a renamed import made the plugin-for-tenant scanner blind to
// this call AND made the secrets-for-tenant scanner wrongly claim it (its
// exclusion only recognises `plugin.ForTenant`, the default-alias spelling).
package fixture

import (
	"context"

	pl "github.com/cleat-team/cleat/plugin"
)

func callsThroughARenamedImport(ctx context.Context, id string) context.Context {
	return pl.ForTenant(ctx, id)
}
