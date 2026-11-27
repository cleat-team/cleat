package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// cleat#4498: at --concurrency 1 a worker on a memory source that reads high (the Go heap on macOS, a cgroup's page cache on Linux) set its own cap to round(1*(1-p)) = 0 for any
// p > 0.5, claimed nothing while idle, and said nothing. These rows pin the floor, the log and the cgroup reading. Each was run against the unfixed code and went red for the stated reason.

// usedFraction returns a reading whose used fraction is f of a 1,000,000 byte total.
func usedFraction(f float64) func() (MemoryInfo, error) {
	return func() (MemoryInfo, error) {
		used := uint64(f * 1000000)
		return MemoryInfo{TotalBytes: 1000000, UsedBytes: used, AvailableBytes: 1000000 - used, Source: "test", CollectedAt: time.Now()}, nil
	}
}

func TestTheCapIsZeroOnlyAtTheHardLimitAndNeverBelowOneBetweenTheLimits(t *testing.T) {
	// Soft 0.80, hard 0.95: p = (f - 0.80) / 0.15. The rows are the rounding the issue was about, at the configured concurrencies a worker is run with.
	rows := []struct {
		configured int
		f          float64
		want       int
		why        string
	}{
		{1, 0.50, 1, "no pressure"},
		{1, 0.82, 1, "p = 0.13, round(0.87) = 1 (never was the problem)"},
		{1, 0.8825, 1, "p = 0.55: round(0.45) = 0, the stall of cleat#4498"},
		{1, 0.89, 1, "p = 0.60: round(0.40) = 0"},
		{1, 0.9499, 1, "p just under 1: round(0.0007) = 0"},
		{1, 0.95, 0, "the hard limit, which is the one place the cap is 0"},
		{2, 0.92, 1, "p = 0.80: round(0.40) = 0 with --concurrency 2"},
		{3, 0.9499, 1, "p just under 1"},
		{10, 0.89, 4, "p = 0.60: round(4.0), unchanged by the floor"},
		{10, 0.9499, 1, "p just under 1: round(0.007) = 0 at the default 10"},
		{10, 0.96, 0, "past the hard limit"},
	}
	for _, r := range rows {
		mc := newTestController(newTestMonitor(), r.configured, 0.80, 0.95)
		mc.monitor.readFn = usedFraction(r.f)
		mc.Tick(context.Background())
		if got := mc.DynamicConcurrency(); got != r.want {
			t.Errorf("--concurrency %d at used fraction %g (%s): cap %d, want %d", r.configured, r.f, r.why, got, r.want)
		}
		if r.want > 0 && !mc.CanClaim() {
			t.Errorf("--concurrency %d at used fraction %g: CanClaim is false with a cap of %d, so an idle worker would claim nothing", r.configured, r.f, r.want)
		}
	}
}

func TestAnIdleWorkerIsNeverCappedAtZeroAcrossTheWholeRampBelowTheHardLimit(t *testing.T) {
	for _, configured := range []int{1, 2, 3, 5, 10} {
		for f := 0.0; f < 0.9499; f += 0.0005 {
			mc := newTestController(newTestMonitor(), configured, 0.80, 0.95)
			mc.monitor.readFn = usedFraction(f)
			mc.Tick(context.Background())
			if mc.DynamicConcurrency() < 1 {
				t.Fatalf("--concurrency %d at used fraction %.4f (pressure %.3f): cap %d, an idle worker claims nothing below the hard limit", configured, f, mc.Pressure(), mc.DynamicConcurrency())
			}
		}
	}
}

// logged is a controller whose log goes to a buffer and whose clock the test moves.
func logged(configured int) (*MemoryController, *bytes.Buffer, *time.Time) {
	var buf bytes.Buffer
	mc := newTestController(newTestMonitor(), configured, 0.80, 0.95)
	mc.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	now := time.Unix(1_800_000_000, 0)
	mc.now = func() time.Time { return now }
	return mc, &buf, &now
}

func TestTheWorkerSaysWhenPressureTakesItsCapToZeroOrToTheFloorAndWhenItLetsGo(t *testing.T) {
	mc, buf, now := logged(10)
	tick := func(f float64) {
		mc.monitor.readFn = usedFraction(f)
		mc.Tick(context.Background())
	}

	tick(0.50)
	if buf.Len() != 0 {
		t.Fatalf("a healthy worker logged: %s", buf)
	}
	tick(0.9499) // the floor under a configured 10
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "floor of 1") || !strings.Contains(out, "concurrency_cap=1") || !strings.Contains(out, "configured_concurrency=10") {
		t.Fatalf("entering the floor logged %q, want one WARN naming the floor, the cap and the configured concurrency", out)
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d WARN lines, want 1", n)
	}
	tick(0.9499) // still there: no new line
	tick(0.96)   // the hard limit: 0 is a different state of the same squeeze, not a second entry, so still one line
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d WARN lines after staying squeezed, want 1: %s", n, buf)
	}
	tick(0.50) // p == 0: recovery steps up one at a time, so the cap is 1 -> 2 here: leaving the squeeze
	*now = now.Add(time.Second)
	time.Sleep(10 * time.Millisecond)
	tick(0.50)
	if !strings.Contains(buf.String(), "level=INFO") || !strings.Contains(buf.String(), "above its floor again") {
		t.Fatalf("leaving the floor logged nothing: %s", buf)
	}

	// Flapping inside the interval is not logged again; after it, it is.
	before := strings.Count(buf.String(), "level=WARN")
	tick(0.9499)
	if n := strings.Count(buf.String(), "level=WARN"); n != before {
		t.Fatalf("re-entering the floor inside %v logged again (%d WARN lines, was %d)", squeezeLogEvery, n, before)
	}
	tick(0.50)
	time.Sleep(10 * time.Millisecond)
	tick(0.50)
	*now = now.Add(squeezeLogEvery + time.Second)
	tick(0.9499)
	if n := strings.Count(buf.String(), "level=WARN"); n != before+1 {
		t.Fatalf("re-entering the floor after %v logged %d WARN lines in all, want %d", squeezeLogEvery, n, before+1)
	}
}

func TestTheHardLimitSaysSoAndAWorkerConfiguredForOneIsNotToldItWasSqueezedAtOne(t *testing.T) {
	mc, buf, _ := logged(1)
	mc.monitor.readFn = usedFraction(0.90) // p = 0.67: the old code's 0, the new floor 1 = the worker's own setting
	mc.Tick(context.Background())
	if buf.Len() != 0 {
		t.Fatalf("a worker configured for 1, held at 1, logged: %s", buf)
	}
	mc.monitor.readFn = usedFraction(0.96)
	mc.Tick(context.Background())
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "hard limit") || !strings.Contains(out, "concurrency_cap=0") {
		t.Fatalf("the hard limit logged %q, want a WARN naming the hard limit and a cap of 0", out)
	}
}

// The cgroup reading: memory.current counts page cache; the working set does not (cleat#4498).
func TestTheCgroupReadingExcludesInactiveFilePagesAsTheKubeletDoes(t *testing.T) {
	const limit = 512 << 20
	base := map[string]string{
		"proc/self/cgroup":             "0::/\n",
		"proc/meminfo":                 meminfo(64 * gib >> 10),
		"sys/fs/cgroup/memory.max":     "536870912\n",
		"sys/fs/cgroup/memory.current": u64(500<<20) + "\n", // 98% of the limit by the raw counter
	}
	with := func(stat string) MemoryInfo {
		files := map[string]string{}
		for k, v := range base {
			files[k] = v
		}
		if stat != "" {
			files["sys/fs/cgroup/memory.stat"] = stat
		}
		return readOnce(t, files)
	}

	// 400 MiB of the 500 MiB is inactive file cache: the working set is 100 MiB, nowhere near the soft limit.
	info := with("anon 1000\nfile " + u64(420<<20) + "\ninactive_file " + u64(400<<20) + "\nactive_file 1000\n")
	if info.UsedBytes != 100<<20 || info.AvailableBytes != limit-100<<20 || info.TotalBytes != limit {
		t.Errorf("used %d avail %d total %d, want the 100 MiB working set against the 512 MiB limit", info.UsedBytes, info.AvailableBytes, info.TotalBytes)
	}
	if p := PressureLevel(info, 0.80, 0.95); p != 0 {
		t.Errorf("pressure %g with 400 MiB of the 500 MiB being reclaimable cache, want 0", p)
	}

	// The negative control: the same counter with no memory.stat reads as before (the raw counter), which IS under pressure, so the row above is the subtraction and not the fixture.
	raw := with("")
	if raw.UsedBytes != 500<<20 {
		t.Errorf("with no memory.stat used %d, want the raw 500 MiB", raw.UsedBytes)
	}
	if p := PressureLevel(raw, 0.80, 0.95); p < 1.0 {
		t.Errorf("pressure %g for the raw 500/512 MiB, want at or past the hard limit (the control for the row above)", p)
	}

	// A stat with no inactive_file line, and one that claims more than is used, are both safe.
	if got := with("anon 1000\n").UsedBytes; got != 500<<20 {
		t.Errorf("a memory.stat with no inactive_file line: used %d, want the raw 500 MiB", got)
	}
	if got := with("inactive_file " + u64(900<<20) + "\n").UsedBytes; got != 0 {
		t.Errorf("an inactive_file larger than memory.current: used %d, want 0, not a wrapped unsigned value", got)
	}
}
