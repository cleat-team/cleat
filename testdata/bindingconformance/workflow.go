// Package bindingconformance is the Go fixture for the shared entry-point
// binding table, tests/conformance/entry_point_binding_cases.json.
//
// cleat#1065. One entry point per case SHAPE rather than one per case: the
// table varies the payload, and the payload is not a property of the fixture.
//
// Each entry echoes WHAT IT BOUND rather than only succeeding, so the reader
// can tell "bound the zero value and ran" apart from "ran without binding
// anything" -- two outcomes that look identical from a workflow that returns a
// constant, and the distinction the whole table rests on.
package bindingconformance

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// Item is the composite case. An absent composite is a bind error in Go, which
// is the one row every SDK already agrees on.
type Item struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

// BindString carries a SECOND parameter on purpose.
//
// A lone string parameter is not bound by name at all: Go's generator passes
// the whole payload straight through ("Single string parameter: pass the raw
// argsJSON directly", wasm/exports.go), and AssemblyScript's transform has the
// same fast path. So a one-string entry point given {} binds the two-character
// string "{}" rather than "" -- which is what this fixture measured before the
// tag existed, and it looked like a binding defect.
//
// Python has no such fast path; it binds by name always. That is a real
// cross-SDK divergence and it has its own row in the table.
//
//cleat:entry
func BindString(h cleat.HostCalls, note string, other int) (string, error) {
	return fmt.Sprintf(`{"bound":%q}`, note), nil
}

// BindLoneString is the fast path itself, measured rather than described.
//
//cleat:entry
func BindLoneString(h cleat.HostCalls, note string) (string, error) {
	return fmt.Sprintf(`{"bound":%q}`, note), nil
}

//cleat:entry
func BindInt(h cleat.HostCalls, count int) (string, error) {
	return fmt.Sprintf(`{"bound":%d}`, count), nil
}

//cleat:entry
func BindComposite(h cleat.HostCalls, item Item) (string, error) {
	return fmt.Sprintf(`{"bound":%q}`, item.SKU), nil
}

// BindOptional is Go's optional mechanism: a POINTER parameter (cleat#1645).
//
// It binds nil when the key is absent, which is NOT what Python and
// AssemblyScript do -- their optional is a declaration-site default, so absence
// binds the declared value. Go has no declaration-site default, so the workflow
// decides its own fallback. The table records that divergence rather than
// assuming one shape; see its "absent, declared optional" case.
//
//cleat:entry
func BindOptional(h cleat.HostCalls, note *string) (string, error) {
	if note == nil {
		return `{"bound":null}`, nil
	}
	return fmt.Sprintf(`{"bound":%q}`, *note), nil
}
