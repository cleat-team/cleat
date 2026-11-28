package engine

import (
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/tenantdrop"
)

// cleat#3552, B3b-1b. one_definition_of_settled_test.go makes every statement in THIS package spell the
// settled set exactly as settledStatusList does, because a run counted as settled in one place and
// unsettled in another is how a parent waited forever on a child (cleat#1974). The drop plan counts
// unsettled runs too, and its statements live in internal/tenantdrop, outside that test's reach: so the
// property is pinned from this side, where settledStatusList is visible.
//
// The set the plan gates a drop on must be the engine's set. If the engine gains a terminal status and
// the plan does not, a run in it would count as unsettled and the drop would be refused for good; if the
// plan gained one the engine lacks, a drop would go ahead over a run the engine still treats as live.
func TestTheDropPlanSpellsSettledLikeTheEngine(t *testing.T) {
	want := "status NOT IN (" + settledStatusList + ")"
	for name, stmt := range map[string]string{
		"tenantdrop.UnsettledPostgres": tenantdrop.UnsettledPostgres,
		"tenantdrop.UnsettledMSSQL":    tenantdrop.UnsettledMSSQL,
	} {
		if !strings.Contains(stmt, want) {
			t.Errorf("%s does not contain %q\n  %s", name, want, stmt)
		}
	}
}
