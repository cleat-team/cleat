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
// WHY THE SDK HELPER TABLE CANNOT COVER THIS, so that nobody "simplifies" the
// fixture away by adding a row to sdkHelperImports: SDKDurableHelper accepts
// only receivers whose package is named "cleat", and this package is named
// agentworkflow. A row for agentworkflow.Run would be written and never
// consulted. The directive is the route, exactly as it is for cleat/dagrun.
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
