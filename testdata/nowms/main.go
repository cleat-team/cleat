// Package nowms is the fixture for the NowMs row of
// TestEachRewiredMethodWiresItsImport. Its only host call is h.NowMs(), which
// compiled to zero host functions and returned 0 -- an epoch timestamp --
// before IMPROVEMENT-PLAN 3.234.
package nowms

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

//cleat:entry
func Stamp(h cleat.HostCalls, input string) (string, error) {
	return fmt.Sprintf("%d", h.NowMs()), nil
}
