package plugin

import (
	"github.com/tetratelabs/wazero"
)

// hostModuleBuilder implements HostModuleBuilder by wrapping wazero's API.
type hostModuleBuilder struct {
	builder wazero.HostModuleBuilder
}

// NewHostModuleBuilder creates a HostModuleBuilder backed by a wazero
// HostModuleBuilder. Pass this to plugins that implement HasHostFunctions.
func NewHostModuleBuilder(builder wazero.HostModuleBuilder) HostModuleBuilder {
	return &hostModuleBuilder{builder: builder}
}

// Register adds a host function to the module. fn must be a function
// compatible with wazero's WithFunc signature requirements (e.g.,
// func(ctx context.Context, m api.Module, params...) uint64).
func (b *hostModuleBuilder) Register(name string, fn interface{}) error {
	b.builder.NewFunctionBuilder().WithFunc(fn).Export(name)
	return nil
}
