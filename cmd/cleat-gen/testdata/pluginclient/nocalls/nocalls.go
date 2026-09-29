// Package nocalls is a synthetic plugin used only by
// cmd/cleat-gen/pluginclient_test.go, to confirm the generator refuses a
// package with no plugin.RegisterTyped call sites rather than silently
// emitting an empty client.
package nocalls

import "github.com/cleat-team/cleat/plugin"

func Register(_ plugin.FuncRegistry) error {
	return nil
}
