// Package scopefixture is the fixture for cleat#984: whether a Go guest that
// calls the Scoper methods actually emits the scope host calls.
//
// IMPROVEMENT-PLAN 3.223 settled the gap with a fixture of exactly this shape --
// a workflow whose body is h.SetScope(obj, key) -- and read the produced binary,
// where cleat_set_scope did not appear at all. This is the converse artifact.
//
// It reports what it observed rather than asserting, so the test decides what
// each value should be.
package scopefixture

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// ExerciseScope sets a scope, replaces it, reads it back and clears it,
// reporting each observation.
//
// Entry point: exercise_scope
func ExerciseScope(h cleat.HostCalls) (string, error) {
	first := h.SetScope("cart", "c1")
	second := h.SetScope("cart", "c2")
	objType, instKey := h.GetScope()
	cleared := h.ClearScope()
	afterType, afterKey := h.GetScope()

	h.Log("scopefixture: exercised")

	return fmt.Sprintf(
		`{"firstPrev":%q,"secondPrev":%q,"objType":%q,"instKey":%q,"cleared":%q,"afterType":%q,"afterKey":%q}`,
		first, second, objType, instKey, cleared, afterType, afterKey), nil
}
