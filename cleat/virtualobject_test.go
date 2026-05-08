package cleat

import (
	"testing"
)

func TestRegisterVirtualObject_Valid(t *testing.T) {
	// Use a unique name to avoid interfering with other tests.
	name := "test-vo-valid"
	def := VirtualObjectDef{
		Name: name,
		EntryPoint: func(h HostCalls, input string) (string, error) {
			return `{"ok":true}`, nil
		},
	}

	RegisterVirtualObject(def)

	got, ok := GetVirtualObject(name)
	if !ok {
		t.Fatal("GetVirtualObject returned false for registered object")
	}
	if got.Name != name {
		t.Errorf("got name %q, want %q", got.Name, name)
	}
	if got.EntryPoint == nil {
		t.Error("EntryPoint is nil")
	}
}

func TestRegisterVirtualObject_EmptyNamePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for empty name, got nil")
		}
	}()

	RegisterVirtualObject(VirtualObjectDef{Name: ""})
}

func TestRegisterVirtualObject_DuplicateNamePanics(t *testing.T) {
	name := "test-vo-duplicate"

	RegisterVirtualObject(VirtualObjectDef{
		Name:        name,
		EntryPoint:  func(h HostCalls, input string) (string, error) { return "{}", nil },
	})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate name, got nil")
		}
	}()

	RegisterVirtualObject(VirtualObjectDef{
		Name:        name,
		EntryPoint:  func(h HostCalls, input string) (string, error) { return "{}", nil },
	})
}

func TestGetVirtualObject_NonexistentReturnsFalse(t *testing.T) {
	_, ok := GetVirtualObject("nonexistent-vo-name")
	if ok {
		t.Fatal("GetVirtualObject returned true for nonexistent name")
	}
}

func TestGetVirtualObject_RegisteredReturnsDef(t *testing.T) {
	name := "test-vo-get"

	entryPoint := func(h HostCalls, input string) (string, error) {
		return `{"result":"ok"}`, nil
	}
	RegisterVirtualObject(VirtualObjectDef{
		Name:        name,
		EntryPoint:  entryPoint,
	})

	got, ok := GetVirtualObject(name)
	if !ok {
		t.Fatal("GetVirtualObject returned false for registered object")
	}
	if got.Name != name {
		t.Errorf("got name %q, want %q", got.Name, name)
	}
}
