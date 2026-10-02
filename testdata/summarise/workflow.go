// Package summarise is the WORKFLOW tool's child in cleat#1983's acceptance
// test: the agent's second tool is a cleat workflow rather than a service, and
// this is it.
//
// IT MAKES ONE DURABLE CALL, and that call is the whole point of the fixture.
// A workflow tool is started with ChildWorkflow and awaited; the acceptance
// claims that a completed tool call is not repeated when the worker dies
// later in the loop. Asserting that needs the child's execution to leave
// something outside the engine to count -- a child that only returned a string
// would be re-run by a replaying parent and look identical from the parent's
// side either way, because AwaitChild hands back the recorded result.
//
// So the child tells a service it ran, and the test asks the service how many
// times it was told. Two means the parent re-started a child it had already
// finished.
package summarise

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// Summarise records that it ran, then answers.
func Summarise(h cleat.HostCalls, input string) (string, error) {
	req, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		return "", fmt.Errorf("summarise: marshal: %w", err)
	}
	if _, err := h.DurableCall("tools", "summarise", string(req)); err != nil {
		return "", err
	}
	return `{"summary":"tokyo is warm"}`, nil
}
