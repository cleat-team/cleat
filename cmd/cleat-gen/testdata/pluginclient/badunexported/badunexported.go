// Package badunexported is a synthetic plugin used only by
// cmd/cleat-gen/pluginclient_test.go, to confirm the generator refuses an
// unexported request type rather than emitting a client nobody outside
// this package could call.
package badunexported

import (
	"context"

	"github.com/cleat-team/cleat/plugin"
)

func Register(scope plugin.FuncRegistry) error {
	return plugin.RegisterTyped(scope, plugin.FuncOptions{Name: "do_thing"}, doThing)
}

type badInput struct {
	Name string `json:"name"`
}

type BadOutput struct {
	OK bool `json:"ok"`
}

func doThing(_ context.Context, _ badInput) (BadOutput, error) {
	return BadOutput{}, nil
}
