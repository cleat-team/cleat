package main

// cleat#2147: plugin.Stoppable was documented and implemented and never
// called. The interface had a doc comment, four lines of documentation
// describing a shutdown lifecycle that did not run, and no caller --
// CLAUDE.md's "a mechanism that exists and is wired to nothing reads as done".
// A third-party author who read either doc and implemented Stop to drain a
// pool got nothing: the worker exited without calling it.
//
// The decision is extracted here, rather than left inline in main()'s signal
// handler, for the reason a_plugin_init_error_severity_test.go gives about
// classifyPluginInitError: a call site inside a signal handler has no outcome
// a test can observe without sending the test process a signal, so the
// behaviour would go untested no matter how carefully it was written. The
// companion test file has the other half -- a source scan of main.go that
// fails if this stops being called, because these tests pass whether or not
// anything calls the function.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// pluginStopDeadline bounds the WHOLE set of plugin Stop calls, not each one.
//
// Shared rather than per-plugin deliberately: the property that matters is
// that a hung plugin cannot hold worker exit open, and N plugins each granted
// the full budget gives a worst case of N times it. One budget makes exit
// bounded by a number the operator can reason about without knowing how many
// plugins are installed.
//
// Not the shutdown grace. That budget has already been spent by the drain by
// the time this runs -- reusing it would either mean nothing on a worker that
// drained slowly, or a second full grace on every shutdown.
const pluginStopDeadline = 5 * time.Second

// stopStoppablePlugins calls Stop on every loaded plugin that implements
// plugin.Stoppable, under one shared deadline.
//
// Called AFTER the drain, beside ratelim.stop() and tenantLim.stop() in the
// signal handler. After, not before: Stop is a plugin's teardown, and running
// it while runs are still in flight would be the same mistake cleat#2285 made
// when it cancelled at the signal -- a plugin that has closed its pool is a
// plugin whose in-flight work now fails.
//
// Only plugins whose Init SUCCEEDED are stopped, which is the convention the
// host already documents for HasHealth -- "never for a plugin whose Init
// failed" (plugin-developer-guide.md), and server.go's call site gates on
// `!lp.Healthy` the same way. The reason is the same one: when Init returns an
// error the host cannot see what state the plugin is in, and the plugin is the
// only party that knows what its own Init opened. A plugin whose Init failed
// owns that cleanup; handing it a teardown it did not ask for is how you get a
// double close on a pool that Init already released on its way out.
//
// A plugin that errors or panics does not stop the others, and neither is
// fatal: this is cleanup on a path that is already exiting, and refusing to
// exit because a cleanup failed trades a leak for an outage.
func stopStoppablePlugins(ctx context.Context, plugList []*plugin.LoadedPlugin, budget time.Duration, logger *slog.Logger) (stopped, failed int) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	for _, lp := range plugList {
		if lp == nil || lp.Plugin == nil || !lp.Healthy {
			continue
		}
		s, ok := lp.Plugin.(plugin.Stoppable)
		if !ok {
			continue
		}
		name := lp.Plugin.Info().Name

		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic during Stop: %v", r)
				}
			}()
			return s.Stop(ctx)
		}()

		if err != nil {
			failed++
			// Deadline exceeded is called out separately because it is the one
			// outcome that says something about the worker rather than the
			// plugin: a plugin that ignores ctx looks identical to a plugin
			// that is merely slow, and only one of those is the plugin's bug.
			logger.ErrorContext(context.Background(), "plugin Stop failed",
				"plugin", name, "error", err, "deadline_exceeded", ctx.Err() != nil)
			continue
		}
		stopped++
		logger.InfoContext(context.Background(), "plugin stopped", "plugin", name)
	}
	return stopped, failed
}
