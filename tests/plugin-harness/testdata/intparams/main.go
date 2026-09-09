// Package intparams is the fixture for how an int entry-point parameter is
// bound from the input JSON.
//
// It reports what it received rather than asserting anything, so one fixture
// serves every case: a negative, a quoted number, a float, a large value. The
// test decides which of those should bind and which should be refused.
package intparams

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// ReportInt echoes the int it was given alongside a string, so a run that
// bound nothing is distinguishable from one that did not run.
//
// Entry point: report_int
func ReportInt(h cleat.HostCalls, amountCents int, note string) (string, error) {
	h.Log(fmt.Sprintf("intparams: amountCents=%d note=%s", amountCents, note))
	return fmt.Sprintf(`{"amountCents":%d,"note":%q}`, amountCents, note), nil
}
