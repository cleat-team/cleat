// Package updatedispatch is the fixture for the update-dispatch row of
// TestEachRewiredMethodWiresItsImport.
//
// Its host calls are h.RegisterUpdateHandler(...) and h.AwaitSignals(...) --
// and NOT h.DispatchUpdates(), which is the point. AwaitSignals is a dispatch
// point: the SDK calls DispatchUpdates before suspending, which calls the
// pollUpdate and completeUpdate CLOSURE FIELDS. A workflow written this way
// never names PollUpdate or CompleteUpdate, so nothing in its own source says
// it needs those imports.
//
// This is the shape a real workflow takes -- register a handler, then wait in
// slices so there is a dispatch point to service it -- which is why it is the
// shape worth pinning.
package updatedispatch

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

var applied int

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	h.RegisterUpdateHandler("bump",
		func(payload string) (string, error) {
			applied++
			return fmt.Sprintf(`{"applied":%d}`, applied), nil
		}, nil)

	for i := 0; i < 20; i++ {
		h.AwaitSignals([]string{"never"}, 1000)
	}
	return fmt.Sprintf(`{"slices":20,"applied":%d}`, applied), nil
}
