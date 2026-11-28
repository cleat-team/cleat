// Package childnames is the fixture for cleat#3500: one workflow that starts
// children by every kind of name argument the build has to classify.
//
//	literal          "child-literal"             resolvable, always was
//	named const      childConst                  resolvable since cleat#3500
//	const expression childPrefix + "-expr"       resolvable since cleat#3500
//	raw string       `child-raw`                 resolvable since cleat#3500
//	variable         name                        NOT resolvable: runtime value
//	call             pick(input)                 NOT resolvable: runtime value
//
// The first four must land in UsageInfo.Children; the last two must be
// reported in UnresolvedChildren with their position, and nowhere else.
package childnames

import "github.com/cleat-team/cleat/cleat"

const (
	childConst  = "child-const"
	childPrefix = "child-prefix"
)

func pick(input string) string { return "child-" + input }

//cleat:entry
func HandleChildNames(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.ChildWorkflow("child-literal", input); err != nil {
		return "", err
	}
	if _, err := h.ChildWorkflow(childConst, input); err != nil {
		return "", err
	}
	if _, err := h.ChildWorkflowWithOptions(childPrefix+"-expr", input, cleat.ChildWorkflowOptions{}); err != nil {
		return "", err
	}
	if _, err := h.ChildWorkflow(`child-raw`, input); err != nil {
		return "", err
	}
	name := pick(input)
	if _, err := h.ChildWorkflow(name, input); err != nil {
		return "", err
	}
	if _, err := h.ChildWorkflowTyped(pick(input), input); err != nil {
		return "", err
	}
	return "ok", nil
}
