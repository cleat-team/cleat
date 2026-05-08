package main

import "sync"

// InflightRegistry provides type-safe tracking of in-flight workflow instances.
type InflightRegistry struct {
	mu    sync.RWMutex
	items map[string]interface{}
}

// NewInflightRegistry creates a new empty InflightRegistry.
func NewInflightRegistry() *InflightRegistry {
	return &InflightRegistry{
		items: make(map[string]interface{}),
	}
}

// Add stores a value under the given key.
func (r *InflightRegistry) Add(key string, value interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[key] = value
}

// Remove deletes the value associated with the given key.
func (r *InflightRegistry) Remove(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.items, key)
}

// Get returns the value associated with the given key.
func (r *InflightRegistry) Get(key string) (interface{}, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.items[key]
	return v, ok
}

// Len returns the number of items in the registry.
func (r *InflightRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.items)
}

// Range calls f sequentially for each key and value. If f returns false, range stops.
func (r *InflightRegistry) Range(f func(key string, value interface{}) bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k, v := range r.items {
		if !f(k, v) {
			break
		}
	}
}
