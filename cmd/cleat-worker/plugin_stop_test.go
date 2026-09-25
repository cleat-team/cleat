package main

// cleat#2147, tests. Two parts, and neither proves the other:
//
//   - the direct tests below exercise stopStoppablePlugins itself, with fakes
//     that record whether they were called;
//   - TestMainCallsStopStoppablePluginsOnShutdown reads main.go and fails if
//     the call site is gone.
//
// The second is the one this issue is about. The interface existed for
// releases with a doc comment, four lines of documentation and no caller, and
// every test in the repo stayed green throughout -- so "Stop is called
// correctly" is not the property that was missing. "Stop is called at all" is,
// and only a check on the call site can fail for that.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// stopFixturePlugin is a plugin that records whether Stop reached it.
type stopFixturePlugin struct {
	name    string
	healthy bool
	stopped bool
	onStop  func(ctx context.Context) error
}

func (p *stopFixturePlugin) Info() plugin.PluginInfo { return plugin.PluginInfo{Name: p.name} }
func (p *stopFixturePlugin) Init(context.Context, *plugin.Environment) error {
	return nil
}
func (p *stopFixturePlugin) Stop(ctx context.Context) error {
	p.stopped = true
	if p.onStop != nil {
		return p.onStop(ctx)
	}
	return nil
}

// stopFixturePlainPlugin implements Plugin and nothing else, which is what
// most plugins are.
type stopFixturePlainPlugin struct{ name string }

func (p *stopFixturePlainPlugin) Info() plugin.PluginInfo { return plugin.PluginInfo{Name: p.name} }
func (p *stopFixturePlainPlugin) Init(context.Context, *plugin.Environment) error {
	return nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestStopStoppablePluginsCallsStopOnEveryStoppablePlugin(t *testing.T) {
	a := &stopFixturePlugin{name: "a", healthy: true}
	b := &stopFixturePlugin{name: "b", healthy: true}
	plain := &stopFixturePlainPlugin{name: "plain"}

	stopped, failed := stopStoppablePlugins(context.Background(), []*plugin.LoadedPlugin{
		{Plugin: a, Healthy: true},
		{Plugin: plain, Healthy: true},
		{Plugin: b, Healthy: true},
	}, time.Second, quietLogger())

	if !a.stopped || !b.stopped {
		t.Errorf("Stop reached a=%v b=%v, want both: every Stoppable plugin must be "+
			"stopped, not just the first or last", a.stopped, b.stopped)
	}
	if stopped != 2 || failed != 0 {
		t.Errorf("stopped=%d failed=%d, want 2/0", stopped, failed)
	}
}

func TestStopStoppablePluginsIsBoundedByItsBudget(t *testing.T) {
	// A plugin that ignores nothing and simply waits for the deadline the host
	// gave it -- the shape of a plugin whose Stop is well-behaved but slow.
	slow := &stopFixturePlugin{
		name:    "slow",
		healthy: true,
		onStop:  func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	}
	// And one that ignores ctx entirely, which is the case the budget exists
	// for: it can only be bounded by the host not waiting for it.
	ignore := &stopFixturePlugin{
		name:    "ignores-context",
		healthy: true,
		onStop:  func(ctx context.Context) error { return nil },
	}

	const budget = 50 * time.Millisecond
	start := time.Now()
	stopped, failed := stopStoppablePlugins(context.Background(), []*plugin.LoadedPlugin{
		{Plugin: slow, Healthy: true},
		{Plugin: ignore, Healthy: true},
	}, budget, quietLogger())
	elapsed := time.Since(start)

	if elapsed > 10*budget {
		t.Errorf("stopStoppablePlugins took %v for a budget of %v.\n\n"+
			"The property is that a hung plugin cannot hold worker exit open. If this "+
			"grows with the number of plugins, the budget is being applied per plugin "+
			"rather than shared, and N slow plugins cost N times it.", elapsed, budget)
	}
	if !slow.stopped || !ignore.stopped {
		t.Errorf("Stop reached slow=%v ignore=%v, want both: a plugin that overran the "+
			"budget must not prevent the plugins after it from being offered their call",
			slow.stopped, ignore.stopped)
	}
	if stopped != 1 || failed != 1 {
		t.Errorf("stopped=%d failed=%d, want 1/1: the slow plugin's Stop returns ctx.Err() "+
			"and that must be counted as a failure, not a success", stopped, failed)
	}
}

func TestStopStoppablePluginsKeepsGoingAfterAFailure(t *testing.T) {
	broken := &stopFixturePlugin{
		name: "broken", healthy: true,
		onStop: func(context.Context) error { return errors.New("pool already closed") },
	}
	panicking := &stopFixturePlugin{
		name: "panicking", healthy: true,
		onStop: func(context.Context) error { panic("Stop blew up") },
	}
	after := &stopFixturePlugin{name: "after", healthy: true}

	stopped, failed := stopStoppablePlugins(context.Background(), []*plugin.LoadedPlugin{
		{Plugin: broken, Healthy: true},
		{Plugin: panicking, Healthy: true},
		{Plugin: after, Healthy: true},
	}, time.Second, quietLogger())

	if !after.stopped {
		t.Errorf("the plugin after a failing one was not stopped: one plugin's rough " +
			"shutdown must not silently skip the others'")
	}
	if stopped != 1 || failed != 2 {
		t.Errorf("stopped=%d failed=%d, want 1/2", stopped, failed)
	}
}

func TestStopStoppablePluginsSkipsAPluginWhoseInitFailed(t *testing.T) {
	// The control is the pair: the same fixture plugin, once with Init
	// succeeded and once with it failed. Asserting only that the failed one is
	// skipped would pass for an implementation that stops nothing at all.
	failed := &stopFixturePlugin{name: "init-failed", healthy: false}
	ok := &stopFixturePlugin{name: "init-ok", healthy: true}

	stopped, _ := stopStoppablePlugins(context.Background(), []*plugin.LoadedPlugin{
		{Plugin: failed, Healthy: false, Error: errors.New("not configured")},
		{Plugin: ok, Healthy: true},
	}, time.Second, quietLogger())

	if failed.stopped {
		t.Error("a plugin whose Init failed was offered Stop. The host cannot see what " +
			"state such a plugin is in, and the plugin is the only party that knows what " +
			"its own Init opened -- server.go's HasHealth site skips these for the same " +
			"reason. Calling Stop anyway risks a double close on a pool Init already " +
			"released on its way out")
	}
	if !ok.stopped {
		t.Error("the plugin whose Init succeeded was not stopped, so this test would " +
			"pass for an implementation that stops nothing")
	}
	if stopped != 1 {
		t.Errorf("stopped=%d, want 1", stopped)
	}
}

func TestStopStoppablePluginsToleratesNilEntries(t *testing.T) {
	// plugList comes from plugin.Discover() and is not expected to hold nils,
	// but a nil deref here is a panic on the shutdown path -- after the drain,
	// where nothing is left to recover to.
	a := &stopFixturePlugin{name: "a", healthy: true}
	stopped, _ := stopStoppablePlugins(context.Background(), []*plugin.LoadedPlugin{
		nil,
		{Plugin: nil},
		{Plugin: a, Healthy: true},
	}, time.Second, quietLogger())
	if stopped != 1 || !a.stopped {
		t.Errorf("stopped=%d a.stopped=%v, want 1/true", stopped, a.stopped)
	}
}

// TestMainCallsStopStoppablePluginsOnShutdown is the half the direct tests
// cannot cover, and the half cleat#2147 is actually about.
func TestMainCallsStopStoppablePluginsOnShutdown(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	s := string(src)

	if !strings.Contains(s, "stopStoppablePlugins(context.Background(), plugList, pluginStopDeadline, logger)") {
		t.Error("main.go's shutdown path no longer calls stopStoppablePlugins. " +
			"stopStoppablePlugins' own tests pass whether or not anything calls it -- " +
			"which is precisely how plugin.Stoppable came to be documented, implemented " +
			"and dead for four docs' worth of releases (cleat#2147).")
	}

	// The call must be after the drain, not before it: Stop is a plugin's
	// teardown, and a plugin that has closed its pool is a plugin whose
	// in-flight runs now fail. cleat#2285 is the same ordering mistake one
	// layer down.
	//
	// The stop < 0 guard is not defensive tidiness -- without it this check
	// fires a second, MISLEADING error whenever the call site is gone, because
	// strings.Index returns -1 and -1 < drain reads as "called before the
	// drain". Measured by removing the call: the failure above is the true
	// one, and this one sent the reader to the ordering when the call had
	// simply been deleted. A check that is wrong in the same run as the check
	// that is right is worse than one that says nothing.
	drain := strings.Index(s, "w.gracefulShutdown(*shutdownGrace, force)")
	stop := strings.Index(s, "stopStoppablePlugins(context.Background(), plugList, pluginStopDeadline, logger)")
	switch {
	case drain < 0:
		t.Error("main.go no longer calls w.gracefulShutdown(*shutdownGrace, force); this " +
			"test's ordering check cannot run, which is a failure of the check rather " +
			"than a finding about the tree")
	case stop < 0:
		// Already reported above. Do not add an ordering verdict to a run where
		// there is no call to order.
	default:
		if stop < drain {
			t.Error("stopStoppablePlugins is called BEFORE the drain. Stop is a plugin's " +
				"teardown; running it while runs are still in flight is how cleat#2285 lost " +
				"the runs a rolling deploy interrupted.")
		}
	}
}
