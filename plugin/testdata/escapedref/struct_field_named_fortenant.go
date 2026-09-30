// FIXTURE, not code. Under testdata/ so the Go tool does not build it; it
// exists to be scanned. cleat#2740 review, R2 -- a DECLINED fixture: this
// must NOT be reported by any scanner in the family.
//
// A struct field and a composite-literal key, both spelled "ForTenant",
// neither touching plugin.ForTenant at all. Before gateBareIdentOnPackage,
// isPluginForTenantCall's `case *ast.Ident: return e.Name == "ForTenant"`
// matched both -- measured live, this exact shape inside a
// plugins/notifications function reported 2 escapes. package fixture
// (not "plugin"), same as every sibling fixture except
// barecontext/bare_unqualified_call.go -- the gate is keyed on exactly
// this distinction.
package fixture

func buildsAStructThatHappensToShareAName() {
	type reviewRecord struct {
		ForTenant int
	}
	_ = reviewRecord{ForTenant: 1}
}
