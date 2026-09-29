// Package fixture is a synthetic plugin used only by
// cmd/cleat-gen/pluginclient_test.go. It exercises the two things a real
// plugin's RegisterTyped call site can need from the generator: a field
// type from a foreign package (time.Time, forcing an import) and a field
// type declared in the plugin's own package (NestedInfo, which must be
// redeclared rather than imported -- see pluginclient.go's file doc
// comment).
package fixture

import (
	"context"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// Register registers doThing under the operation name "do_thing".
func Register(scope plugin.FuncRegistry) error {
	return plugin.RegisterTyped(scope, plugin.FuncOptions{Name: "do_thing"}, doThing)
}

type DoThingInput struct {
	Name string `json:"name"`
}

type NestedInfo struct {
	Count int `json:"count"`
}

type DoThingOutput struct {
	When   time.Time  `json:"when"`
	Nested NestedInfo `json:"nested"`
}

func doThing(_ context.Context, _ DoThingInput) (DoThingOutput, error) {
	return DoThingOutput{}, nil
}
