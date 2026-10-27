package migration_test

import (
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

// cleat#3463. The refusal is the first text an operator upgrading from 0.4.0
// reads, and it once told them to "move your data across with an export and
// import" while docs/operations/upgrading.md said there is no such tool. Nothing
// failed, because the tests around it pin the marker, the version and the docs
// path, and none of them read the remedy.
//
// This pins the remedy to the route the doc gives, from both sides: the message
// carries each step and offers no export or import, and the doc still says
// there is no such tool, so rewording either side alone fails here.
func TestTheForeignChainRefusalNamesTheRouteTheUpgradingDocNames(t *testing.T) {
	msg := (&migration.ForeignChainError{
		Where: "public.schema_migrations",
		Mismatches: []migration.ChainMismatch{
			{Version: "1", Recorded: "no identity", Shipped: "001_schema.sql (abc)"},
		},
	}).Error()

	for _, want := range []string{
		migration.ForeignChainMarker,
		"Nothing was changed",
		"create a fresh database",
		"`cleat-worker --migrate-only`",
		"beside the old version until the old one has drained",
		"there is no in-place conversion or export tool",
		"docs/operations/upgrading.md",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, msg)
		}
	}
	for _, banned := range []string{"export and import", "move your data across"} {
		if strings.Contains(msg, banned) {
			t.Errorf("the refusal offers %q, a route the upgrading doc says does not exist:\n%s", banned, msg)
		}
	}

	doc, err := os.ReadFile("../docs/operations/upgrading.md")
	if err != nil {
		t.Fatalf("read the upgrading doc the refusal points at: %v", err)
	}
	if !strings.Contains(string(doc), "There is no export/import tool") {
		t.Errorf("docs/operations/upgrading.md no longer says there is no export/import tool, " +
			"so the refusal's remedy and the doc may have drifted apart again; reconcile both")
	}
}
