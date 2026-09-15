// Package methodreceiverfield is the POSITIVE counterpart to
// testdata/methodglobalh: the same method, reaching the host the supported
// way.
//
// Retrier carries a cleat.HostCalls field, so phase 3 of VerifyThreading
// admits its methods. cleat#1614 tightened phase 0 to stop crediting methods
// that reference the package-level h; this fixture exists so that tightening
// cannot quietly become "no method may reach the host".
package methodreceiverfield

import (
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// Retrier carries its own HostCalls -- the struct_methods pattern.
type Retrier struct {
	H        cleat.HostCalls
	Attempts int
}

// Wait reaches the host through the receiver's field, not through a global.
func (r *Retrier) Wait() {
	r.H.DurableSleep(5 * time.Second)
}

// RetryOrder is the entry point.
func RetryOrder(h cleat.HostCalls, orderID string, attempts int) (string, error) {
	r := &Retrier{H: h, Attempts: attempts}
	r.Wait()
	return orderID, nil
}
