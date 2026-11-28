// Package agentguest is the fixture for cleat#1983: a workflow whose ONLY
// route to five host calls is cleat/agentworkflow.
//
// The workflow's own source names exactly one host call, h.NowMs(), and that
// is deliberate -- the same design as testdata/dagguest, for the same reason.
// Without it, "no imports at all" could not be told apart from a build that
// failed for some unrelated cause: one import is present in a correct build,
// and it is the one this file wrote itself. What must ALSO be present is the
// five that agentworkflow needs and this file never mentions.
//
// THE DIRECTIVE IS READ ON ITS OWN in wasm's test for this fixture. Until cleat#3803 the build's hand list of SDK
// helpers accepted only receivers in a package named "cleat", and this package is named agentworkflow, so the
// directive was the only route to these five imports; the build now also reads agentworkflow.Run's body, which
// wires them as well.
package agentguest

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/cleat/agentworkflow"
)

//cleat:entry
func HandleAgentGuest(h cleat.HostCalls, input string) (string, error) {
	t0 := h.NowMs()

	out, err := agentworkflow.Run(h, input)
	if err != nil {
		return "", fmt.Errorf("agent: %w", err)
	}
	return fmt.Sprintf(`{"t0":%d,"out":%s}`, t0, out), nil
}
