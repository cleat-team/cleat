// Package badoptsvar is a synthetic plugin used only by
// cmd/cleat-gen/pluginclient_test.go, to confirm the generator refuses a
// FuncOptions.Name that isn't a string literal (it cannot be read
// statically) rather than silently guessing an operation name.
package badoptsvar

import (
	"context"

	"github.com/cleat-team/cleat/plugin"
)

var opName = "do_thing"

func Register(scope plugin.FuncRegistry) error {
	return plugin.RegisterTyped(scope, plugin.FuncOptions{Name: opName}, doThing)
}

type Input struct {
	Name string `json:"name"`
}

type Output struct {
	OK bool `json:"ok"`
}

func doThing(_ context.Context, _ Input) (Output, error) {
	return Output{}, nil
}
