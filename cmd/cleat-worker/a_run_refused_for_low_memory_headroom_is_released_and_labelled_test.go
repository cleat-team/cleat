package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// An in-process component compile the container has no memory left for is refused (engine.compileHeadroom), and the worker must release the run, not fail it, and make the refusal countable on its OWN
// label value, so a fleet can tell "the child was killed" from "the worker declined to compile in its own process". Same fixture as the killed-child test beside it.
func TestARunRefusedForLowMemoryHeadroomIsReleasedNotFailedAndLabelledLowHeadroom(t *testing.T) {
	const retryAfter = 5 * time.Minute
	refusal := &engine.ComponentCompileRefusedError{MemoryLimitBytes: 2 << 30, RetryAfter: retryAfter, Window: retryAfter,
		Reason: engine.ComponentCompileRefusedLowHeadroom, HeadroomBytes: 300 << 20, NeededBytes: 800 << 20}
	f, rb := newRefusalFixture(t, refusal)
	f.run()

	if rb.calls.Load() != 1 {
		t.Fatalf("the backend was reached %d time(s), want 1", rb.calls.Load())
	}
	if f.fails.Load() != 0 {
		t.Fatalf("FailWorkflow was called %d time(s): the guest never ran, so the run did nothing wrong. Log:\n%s", f.fails.Load(), f.logs.String())
	}
	if f.releases.Load() != 1 {
		t.Fatalf("releases = %d, want 1.\nLog:\n%s", f.releases.Load(), f.logs.String())
	}
	log := f.logs.String()
	for _, want := range []string{"level=ERROR", "300 MiB of memory left", "800 MiB", "--component-compile-min-headroom-mb"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	rec := httptest.NewRecorder()
	f.w.Metrics.ServeHTTP().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	var line string
	for _, l := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(l, "cleat_workflow_releases_total") && strings.Contains(l, `check="component_compile_refused"`) {
			line = l
		}
	}
	for _, want := range []string{`reason="low_headroom"`, `memory_limit_mib="2048"`} {
		if !strings.Contains(line, want) {
			t.Errorf("cleat_workflow_releases_total{check=\"component_compile_refused\"} = %q, want it to carry %s", line, want)
		}
	}
	if strings.Contains(line, `reason="child_killed"`) {
		t.Errorf("a low-headroom refusal was labelled as a killed child: %q", line)
	}
}

func TestTheCompileHeadroomReaderTreatsAnUnreadableOrGoHeapReadingAsCannotRead(t *testing.T) {
	cases := []struct {
		name   string
		mi     MemoryInfo
		wantOK bool
		want   uint64
	}{
		{"a cgroup v2 reading is used", MemoryInfo{Source: "cgroupv2", AvailableBytes: 900 << 20}, true, 900 << 20},
		{"a /proc/meminfo reading is used", MemoryInfo{Source: "procmeminfo", AvailableBytes: 5 << 30}, true, 5 << 30},
		{"the Go heap is not the container's memory", MemoryInfo{Source: "goheap", AvailableBytes: 1 << 40}, false, 0},
		{"no source at all is no reading", MemoryInfo{}, false, 0},
	}
	for _, c := range cases {
		got, ok := compileHeadroomReader(func() MemoryInfo { return c.mi })()
		if ok != c.wantOK || got != c.want {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
}
