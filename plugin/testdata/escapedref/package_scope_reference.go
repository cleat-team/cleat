// FIXTURE, not code. Under testdata/ so the Go tool does not build it; it
// exists to be scanned. cleat#2740 review, R1.
//
// The same escape as value_reference.go's, one scope out: a tracked
// function assigned to a var declared OUTSIDE any function, rather than
// inside one. trackedReferenceEscapes only ever walked a FuncDecl's body,
// so this was invisible -- measured live, this exact shape inside a
// plugins/notifications function reported 1 escape; at package scope, 0.
package fixture

import (
	"github.com/cleat-team/cleat/plugin"
)

var reviewMark = plugin.ForTenant
