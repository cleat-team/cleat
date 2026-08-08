package plugin

import (
	"errors"
	"sync"
	"testing"
)

func TestNewPluginHealthTracker(t *testing.T) {
	tracker := NewPluginHealthTracker()
	if tracker == nil {
		t.Fatal("NewPluginHealthTracker() returned nil")
	}
	if !tracker.IsHealthy("test-plugin") {
		t.Error("expected new tracker to report plugin as healthy")
	}
	statuses := tracker.UnhealthyStatus()
	if len(statuses) != 0 {
		t.Errorf("expected empty unhealthy status list, got %d items", len(statuses))
	}
}

func TestPluginHealthTracker_MarkHealthy(t *testing.T) {
	tracker := NewPluginHealthTracker()
	tracker.MarkUnhealthy("test-plugin", errors.New("fatal error"))
	if tracker.IsHealthy("test-plugin") {
		t.Fatal("expected plugin to be unhealthy after MarkUnhealthy")
	}
	tracker.MarkHealthy("test-plugin")
	if !tracker.IsHealthy("test-plugin") {
		t.Error("expected plugin to be healthy after MarkHealthy")
	}
}

func TestPluginHealthTracker_MarkUnhealthy(t *testing.T) {
	tracker := NewPluginHealthTracker()
	err := errors.New("something went wrong")
	tracker.MarkUnhealthy("test-plugin", err)
	if tracker.IsHealthy("test-plugin") {
		t.Fatal("expected plugin to be unhealthy after MarkUnhealthy")
	}
	got := tracker.UnhealthyError("test-plugin")
	if got == nil {
		t.Fatal("UnhealthyError returned nil for unhealthy plugin")
	}
	if got.Error() != "something went wrong" {
		t.Errorf("UnhealthyError = %q, want %q", got.Error(), "something went wrong")
	}
}

func TestPluginHealthTracker_IsHealthy(t *testing.T) {
	tracker := NewPluginHealthTracker()

	if !tracker.IsHealthy("alpha") {
		t.Error("expected alpha to be healthy initially")
	}
	tracker.MarkUnhealthy("alpha", errors.New("err"))
	if tracker.IsHealthy("alpha") {
		t.Error("expected alpha to be unhealthy")
	}
	tracker.MarkHealthy("alpha")
	if !tracker.IsHealthy("alpha") {
		t.Error("expected alpha to be healthy after MarkHealthy")
	}

	// A different plugin should not affect the first one.
	tracker.MarkUnhealthy("beta", errors.New("err"))
	if !tracker.IsHealthy("alpha") {
		t.Error("marking beta unhealthy should not affect alpha")
	}
	if tracker.IsHealthy("beta") {
		t.Error("expected beta to be unhealthy")
	}
}

func TestPluginHealthTracker_UnhealthyError(t *testing.T) {
	tracker := NewPluginHealthTracker()

	// Returns nil when healthy.
	if err := tracker.UnhealthyError("test-plugin"); err != nil {
		t.Errorf("expected nil for healthy plugin, got %v", err)
	}

	// Returns the error when unhealthy.
	sentinel := errors.New("disk full")
	tracker.MarkUnhealthy("test-plugin", sentinel)
	got := tracker.UnhealthyError("test-plugin")
	if got == nil {
		t.Fatal("expected non-nil error for unhealthy plugin")
	}
	if !errors.Is(got, sentinel) {
		t.Errorf("UnhealthyError = %v, want %v", got, sentinel)
	}
}

func TestPluginHealthTracker_UnhealthyStatus(t *testing.T) {
	tracker := NewPluginHealthTracker()

	// Empty when nothing is unhealthy.
	statuses := tracker.UnhealthyStatus()
	if len(statuses) != 0 {
		t.Errorf("expected empty statuses, got %d", len(statuses))
	}

	// Contains the unhealthy plugin.
	tracker.MarkUnhealthy("alpha", errors.New("boom"))
	statuses = tracker.UnhealthyStatus()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if statuses[0].Name != "alpha" {
		t.Errorf("status name = %q, want %q", statuses[0].Name, "alpha")
	}
	if statuses[0].Healthy {
		t.Error("expected status Healthy to be false")
	}
	if statuses[0].Error != "boom" {
		t.Errorf("status Error = %q, want %q", statuses[0].Error, "boom")
	}

	// After MarkHealthy the plugin disappears from unhealthy status.
	tracker.MarkHealthy("alpha")
	statuses = tracker.UnhealthyStatus()
	if len(statuses) != 0 {
		t.Errorf("expected empty statuses after MarkHealthy, got %d", len(statuses))
	}
}

func TestPluginHealthTracker_UnhealthyStatusNilError(t *testing.T) {
	tracker := NewPluginHealthTracker()
	tracker.MarkUnhealthy("alpha", nil)
	statuses := tracker.UnhealthyStatus()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if statuses[0].Name != "alpha" {
		t.Errorf("status name = %q, want %q", statuses[0].Name, "alpha")
	}
	if statuses[0].Healthy {
		t.Error("expected status Healthy to be false")
	}
	if statuses[0].Error != "" {
		t.Errorf("expected empty Error for nil cause, got %q", statuses[0].Error)
	}
}

func TestPluginHealthTracker_Concurrency(t *testing.T) {
	tracker := NewPluginHealthTracker()
	var wg sync.WaitGroup

	// Launch goroutines that hammer MarkHealthy, MarkUnhealthy, and IsHealthy.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := "plugin"
			for j := 0; j < 50; j++ {
				tracker.MarkUnhealthy(name, errors.New("err"))
				tracker.IsHealthy(name)
				tracker.MarkHealthy(name)
				tracker.IsHealthy(name)
				tracker.MarkUnhealthy(name, errors.New("err"))
			}
		}(i)
	}

	wg.Wait()

	// Final state is indeterminate but the tracker must not panic or deadlock.
	_ = tracker.IsHealthy("plugin")
	_ = tracker.UnhealthyStatus()
}

func TestPanicError_Error(t *testing.T) {
	pe := &PanicError{
		Plugin: "my-plugin",
		Value:  "index out of bounds",
		Stack:  "goroutine 1 [running]:\nmain.foo()",
	}
	msg := pe.Error()
	want := `plugin "my-plugin" panicked: index out of bounds`
	if msg != want {
		t.Errorf("PanicError.Error() = %q, want %q", msg, want)
	}
}

func TestPanicError_ErrorWithNonStringValue(t *testing.T) {
	pe := &PanicError{
		Plugin: "my-plugin",
		Value:  42,
		Stack:  "goroutine 1 [running]:\nmain.foo()",
	}
	msg := pe.Error()
	want := `plugin "my-plugin" panicked: 42`
	if msg != want {
		t.Errorf("PanicError.Error() = %q, want %q", msg, want)
	}
}

func TestPanicError_Unwrap(t *testing.T) {
	pe := &PanicError{Plugin: "p", Value: "x"}
	if err := pe.Unwrap(); err != nil {
		t.Errorf("expected nil from Unwrap, got %v", err)
	}
}

// PanicError must satisfy error.
//
// This is a compile-time assertion rather than a test because the test could
// not fail. It read:
//
//	var err error = &PanicError{Plugin: "p", Value: "x"}
//	if err == nil { t.Fatal("PanicError should implement error interface") }
//
// and staticcheck's SA4023 is right that `err == nil` is never true there: an
// interface holding a non-nil concrete pointer is not nil. The only part of it
// doing any work was the assignment on the first line, which is checked by the
// compiler -- so that is where the check belongs. Found once _test.go files
// stopped being excluded from golangci-lint.
var _ error = (*PanicError)(nil)
