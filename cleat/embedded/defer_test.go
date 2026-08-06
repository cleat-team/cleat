package embedded

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestDeferFuncsRun is the regression test for an API that silently did nothing.
//
// DurableDeferFunc appended the closure to execution.deferFuncs, and that field
// had no reader anywhere in non-test code. Every closure passed to it was
// collected and dropped: the caller got a defer ID and no cleanup.
//
// This runner is documented for "integration testing and simple single-binary
// deployments", so the failure mode was a test suite reporting that cleanup
// worked when it had never been invoked.
func TestDeferFuncsRun(t *testing.T) {
	t.Run("runs after the workflow body", func(t *testing.T) {
		r := New()
		ran := false
		r.Register("wf", func(ctx *Context) error {
			if _, err := ctx.h.DurableDeferFunc(func() { ran = true }); err != nil {
				t.Fatalf("DurableDeferFunc: %v", err)
			}
			ctx.SetOutput(`{"ok":true}`)
			return nil
		})
		if _, err := r.ExecuteWorkflow(context.Background(), "wf", "{}"); err != nil {
			t.Fatalf("ExecuteWorkflow: %v", err)
		}
		if !ran {
			t.Fatal("the deferred closure never ran")
		}
	})

	t.Run("LIFO", func(t *testing.T) {
		// Order is the point, not an incidental: resources are released in the
		// reverse of the order they were acquired, and Go's defer -- which this
		// API is named after -- is LIFO.
		r := New()
		var order []string
		r.Register("wf", func(ctx *Context) error {
			for _, name := range []string{"first", "second", "third"} {
				n := name
				if _, err := ctx.h.DurableDeferFunc(func() { order = append(order, n) }); err != nil {
					t.Fatalf("DurableDeferFunc(%s): %v", n, err)
				}
			}
			return nil
		})
		if _, err := r.ExecuteWorkflow(context.Background(), "wf", "{}"); err != nil {
			t.Fatalf("ExecuteWorkflow: %v", err)
		}
		if got := strings.Join(order, ","); got != "third,second,first" {
			t.Errorf("defer order = %q, want %q", got, "third,second,first")
		}
	})

	t.Run("runs when the workflow fails", func(t *testing.T) {
		// The case that matters most. A defer exists to clean up after the
		// thing that went wrong; one that only runs on success is not a
		// destructor.
		r := New()
		ran := false
		r.Register("wf", func(ctx *Context) error {
			if _, err := ctx.h.DurableDeferFunc(func() { ran = true }); err != nil {
				t.Fatalf("DurableDeferFunc: %v", err)
			}
			return errors.New("workflow failed")
		})
		if _, err := r.ExecuteWorkflow(context.Background(), "wf", "{}"); err == nil {
			t.Fatal("expected the workflow's error to propagate")
		}
		if !ran {
			t.Fatal("the deferred closure did not run when the workflow returned an error")
		}
	})

	t.Run("a panicking defer does not strand the others", func(t *testing.T) {
		// Deliberately unlike Go, where a panic in a defer propagates: cleanup
		// here is best-effort, and one failed release should not prevent the
		// rest. Matches engine/flush.go's runDefers so the two paths agree.
		r := New()
		var ran []string
		r.Register("wf", func(ctx *Context) error {
			ctx.h.DurableDeferFunc(func() { ran = append(ran, "first") })
			ctx.h.DurableDeferFunc(func() { panic("cleanup blew up") })
			ctx.h.DurableDeferFunc(func() { ran = append(ran, "third") })
			return nil
		})
		if _, err := r.ExecuteWorkflow(context.Background(), "wf", "{}"); err != nil {
			t.Fatalf("ExecuteWorkflow: %v", err)
		}
		if got := strings.Join(ran, ","); got != "third,first" {
			t.Errorf("ran = %q, want %q -- the panic should be contained, not fatal "+
				"and not a stopper", got, "third,first")
		}
	})

	t.Run("a defer may adjust the output", func(t *testing.T) {
		// Consequence of running defers before the output is read, which is
		// the order Go uses. Pinned because it is the observable difference
		// between "defers run" and "defers run at the right moment".
		r := New()
		r.Register("wf", func(ctx *Context) error {
			ctx.h.DurableDeferFunc(func() { ctx.SetOutput(`{"from":"defer"}`) })
			ctx.SetOutput(`{"from":"body"}`)
			return nil
		})
		out, err := r.ExecuteWorkflow(context.Background(), "wf", "{}")
		if err != nil {
			t.Fatalf("ExecuteWorkflow: %v", err)
		}
		if out != `{"from":"defer"}` {
			t.Errorf("output = %s, want the defer's value; defers must run before "+
				"the output is read", out)
		}
	})
}
