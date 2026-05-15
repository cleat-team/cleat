// Package generics demonstrates a durable workflow using Go generics
// for testing the transformer's analysis pipeline.
//
// It defines:
//   - A generic helper function Process[T any] with type parameters
//   - A generic durable leaf GenericLeaf[T any]
//   - A generic type Container[T] with methods
//   - An entry point that instantiates and calls generic functions
//
// NOTE: This test fixture exercises the low-level DurableCall API.
// Production code should prefer DurableCallTyped which handles JSON
// marshaling/unmarshaling automatically and eliminates injection risks.
package generics

import (
	"github.com/cleat-team/cleat/cleat"
)

// ---- Generic functions ----

// Process is a generic workflow helper that processes any item type.
// It is in the durable closure (called by EntryPoint).
func Process[T any](h cleat.HostCalls, item T) (T, error) {
	h.DurableLog("processing item")
	return item, nil
}

// GenericLeaf demonstrates a generic durable leaf function.
// It calls a HostCalls method directly, making it a durable leaf.
func GenericLeaf[T any](h cleat.HostCalls, items []T) error {
	for range items {
		h.DurableLog("processing generic item")
	}
	return nil
}

// ---- Generic type with methods ----

// Container is a generic type that holds items.
type Container[T any] struct {
	Items []T
	Label string
}

// Process is a method on a generic type Container[T].
// It is a durable leaf because it calls h.DurableLog.
func (c *Container[T]) Process(h cleat.HostCalls) error {
	for _, item := range c.Items {
		_ = item
		h.DurableLog("processing container item")
	}
	return nil
}

// ---- Entry point ----

// EntryPoint is a workflow entry point that uses generics.
// It instantiates generic functions with concrete types
// and uses generic types with methods.
func EntryPoint(h cleat.HostCalls, input string) (string, error) {
	// Instantiate generic function with string type parameter.
	result, err := Process[string](h, "hello")
	if err != nil {
		return "", err
	}

	// Call generic leaf with string type parameter.
	items := []string{"a", "b", "c"}
	if err := GenericLeaf[string](h, items); err != nil {
		return "", err
	}

	// Use generic type with method call.
	container := &Container[string]{Items: items, Label: "test"}
	if err := container.Process(h); err != nil {
		return "", err
	}

	// Generic function with int type parameter.
	count, err := Process[int](h, len(items))
	if err != nil {
		return "", err
	}

	_ = count
	return result, nil
}
