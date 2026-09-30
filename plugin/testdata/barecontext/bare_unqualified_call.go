// FIXTURE, not code. Under testdata/ so the Go tool does not build it; it
// exists to be scanned. cleat#2740 (E).
//
// A bare, unqualified ForTenant call -- the shape a call from WITHIN
// package plugin itself would use. Before this fixture,
// isPluginForTenantCall's `case *ast.Ident` arm (matching the unqualified
// spelling) had no fixture and no live caller: deleting that arm failed
// nothing, so a regression to it would have been silent.
//
// DELIBERATELY "package plugin", unlike every sibling fixture's "package
// fixture" (cleat#2740 review, R2). gateBareIdentOnPackage only honours the
// bare-Ident branch when the scanned file's own package is "plugin" -- the
// whole point of this fixture is to be the one case where that condition
// is true, so it has to actually say so.
package plugin

import "context"

func calledFromWithinThePackage(ctx context.Context, id string) context.Context {
	return ForTenant(ctx, id)
}
