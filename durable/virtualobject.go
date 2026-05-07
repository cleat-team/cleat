// Package durable provides the VirtualObject helper for key-scoped
// stateful services (analogous to Restate's Virtual Object pattern).
//
// VirtualObject wraps a HostCalls and automatically scopes all state
// operations to a specific object type and instance key, so each
// instance has its own isolated state namespace.
package durable

// VirtualObject wraps a key-scoped workflow as a long-lived stateful entity.
// State operations are automatically scoped to "{objectType}:{instanceKey}".
//
// Usage:
//
//	vo := NewVirtualObject(h, "counter", "room-123")
//	vo.Set("visits", 42)
//	count := vo.GetInt("visits")
type VirtualObject struct {
	h           HostCalls
	ObjectType  string
	InstanceKey string
}

// NewVirtualObject creates a VirtualObject that scopes all state operations
// to the given objectType and instanceKey.
func NewVirtualObject(h HostCalls, objectType, instanceKey string) *VirtualObject {
	return &VirtualObject{
		h:           h,
		ObjectType:  objectType,
		InstanceKey: instanceKey,
	}
}

// withScope sets the HostCalls scope to this VirtualObject's
// objectType/instanceKey and returns a restore function. Callers MUST
// defer the restore function immediately:
//
//	defer vo.withScope()()
func (vo *VirtualObject) withScope() func() {
	prevType, prevKey := vo.h.GetScope()
	vo.h.SetScope(vo.ObjectType, vo.InstanceKey)
	return func() {
		if prevType != "" || prevKey != "" {
			vo.h.SetScope(prevType, prevKey)
		} else {
			vo.h.ClearScope()
		}
	}
}

// Get retrieves a typed value from the virtual object's scoped state.
// result must be a pointer; the stored value is unmarshaled into it.
func (vo *VirtualObject) Get(key string, result interface{}) error {
	defer vo.withScope()()
	return vo.h.GetState(key, result)
}

// Set stores a typed value in the virtual object's scoped state.
// value is marshaled to JSON for persistence.
func (vo *VirtualObject) Set(key string, value interface{}) {
	defer vo.withScope()()
	vo.h.SetState(key, value)
}

// GetInt is a convenience method that retrieves an integer value from
// the virtual object's scoped state. Returns 0 if the key does not exist.
func (vo *VirtualObject) GetInt(key string) int64 {
	defer vo.withScope()()
	var val int64
	err := vo.h.GetState(key, &val)
	if err != nil {
		return 0
	}
	return val
}

// Delete removes a key from the virtual object's scoped state.
func (vo *VirtualObject) Delete(key string) {
	defer vo.withScope()()
	vo.h.DeleteState(key)
}

// Has returns true if the key exists in the virtual object's scoped state.
func (vo *VirtualObject) Has(key string) bool {
	defer vo.withScope()()
	return vo.h.HasState(key)
}

// List returns all state keys in the virtual object's scoped namespace
// matching the given prefix. The prefix is relative to the virtual
// object's scope. Returns empty slice if no keys match.
func (vo *VirtualObject) List(prefix string) []string {
	defer vo.withScope()()
	return vo.h.ListState(prefix)
}

// ContinueAsNew creates a new workflow run with fresh event history,
// passing the current state as input. The new run starts with no
// history, effectively "rehydrating" the virtual object's state.
func (vo *VirtualObject) ContinueAsNew(newInputJSON string) error {
	// Note: we intentionally do NOT use withScope here because
	// ContinueAsNew is not a state operation -- it resets the
	// workflow history. The scope is irrelevant.
	return vo.h.ContinueAsNew(newInputJSON)
}
