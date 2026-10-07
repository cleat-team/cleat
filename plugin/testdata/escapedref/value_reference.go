// FIXTURE, not code. Under testdata/ so the Go tool does not build it; it
// exists to be scanned. cleat#2740 (G) -- the issue's own example.
//
// Assigns plugin.ForTenant to a variable and calls it indirectly. A ledger
// keyed on CallExpr.Fun only ever sees the CALL, whose Fun here is the bare
// identifier "mark" -- not "plugin.ForTenant" -- so this call site is
// invisible to every scanner in the family. What must be caught is the
// ASSIGNMENT: `mark := plugin.ForTenant` is a reference to the tracked
// function that is not itself a call, and that is where the ledger's
// tracking escapes.
package fixture

import (
	"context"

	"github.com/cleat-team/cleat/plugin"
)

func indirectlyThroughAVariable(ctx context.Context, id string) context.Context {
	mark := plugin.ForTenant
	return mark(ctx, id)
}
