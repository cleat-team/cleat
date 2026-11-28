// Package exposureinvalid is the Go fixture for cleat#3524: a workflow whose
// package declares an exposure class that is not one of the three, which
// `cleat build` refuses and `cleat vet` used to print OK for.
//
// It is deliberately broken.
//
//cleat:exposure bogus
package exposureinvalid

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// Handle is the workflow's entry point; nothing here is about the host ABI.
func Handle(h cleat.HostCalls, input string) (string, error) {
	return fmt.Sprintf(`{"handled":%q}`, input), nil
}
