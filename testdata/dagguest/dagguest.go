// Package dagguest is the fixture for cleat#1617: a workflow whose ONLY route
// to a host call is cleat/dagrun.
//
// The workflow's own source names exactly one host call, h.NowMs(), and that
// is deliberate. Without it, "no imports at all" could not be told apart from
// a build that failed for some unrelated reason -- one import is present in a
// correct build, and it is the one this file wrote itself. What must ALSO be
// present is the pair dagrun needs and this file never mentions:
// cleat_child_workflow_with_options and cleat_await_any_child.
//
// Before cleat#1617 they were absent. dagrun declares them with a
// //cleat:require directive, and collectRequirements read only the workflow's
// own package, so the directive -- correct, and written by someone who
// expected it to work -- sat in the one place that could not act on it. The
// module built, deployed, and died on its first task with "the HostCalls
// runtime was not initialized".
package dagguest

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/cleat/dagrun"
)

//cleat:entry
func HandleDagGuest(h cleat.HostCalls, input string) (string, error) {
	t0 := h.NowMs()

	d := dagrun.NewDAG()
	d.AddTask("a", nil, func(ctx *dagrun.TaskContext) (string, error) {
		return "a-done", nil
	})
	d.AddTask("b", []string{"a"}, func(ctx *dagrun.TaskContext) (string, error) {
		return "b-done", nil
	})

	if err := d.Execute(h, input); err != nil {
		return "", fmt.Errorf("dag execute: %w", err)
	}
	out, _ := d.Output("b")
	return fmt.Sprintf(`{"t0":%d,"b":%q}`, t0, out), nil
}
