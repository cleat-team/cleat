package main

import (
	"log/slog"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// compileHeadroomFlagDefaultMB is the memory a container must have left for the worker to compile ONE Python-sized component in its own process: about 500 MiB of scratch while the compile runs plus about 300 MiB it
// keeps for good, rounded up to 800. Sources, all measured 2026-11-23 on arm64 Linux: a Python component's first in-worker compile raised RSS by +486 to +512 MiB and each further distinct one by
// +185 to +268 (component-memory measurements); in a 2 GiB container the resident anonymous memory after each of eight in-worker compiles of distinct Python components was 533, 836, 1034, 1277, 1585,
// 1661, 1716 and 1760 MiB, which is up to about 300 MiB (198 to 308 for each of the first four further components, then 44 to 76) retained per component. This is the need of a Python-sized component, the MAXIMUM: the engine scales it down by the component's wasm length to a
// floor of 256 MiB for a small C# workflow, from the C# measurements in engine/wasmtime_options.go (a C# compile peaked at +118 MiB for the first in a process and +11 to +60 after). It is a measured
// estimate and a flag, not a constant of nature: --component-compile-min-headroom-mb overrides it, and 0 turns the check off.
const compileHeadroomFlagDefaultMB = 800

// compileHeadroomReader adapts the memory monitor to what the engine's headroom check reads. A source that is not the container's or the host's memory (the Go heap, which the monitor falls back to when
// nothing else is readable) or no reading at all is "cannot read", and the engine then compiles as it always did: refusing on a number that is not memory left would be worse than not checking.
func compileHeadroomReader(read func() MemoryInfo) func() (uint64, bool) {
	return func() (uint64, bool) {
		mi := read()
		if mi.Source == "" || mi.Source == "goheap" {
			return 0, false
		}
		return mi.AvailableBytes, true
	}
}

// compileHeadroomOptionFromFlags makes the worker refuse to compile a component IN ITS OWN PROCESS (the fallback after a failed precompile child, and every compile with --component-precompile=false)
// when the container has less memory left than --component-compile-min-headroom-mb. A refused run is released, not failed. With no cgroup limit the reading is the host's available memory, and it says so.
func compileHeadroomOptionFromFlags(logger *slog.Logger) []engine.WasmtimeOption {
	if *componentCompileMinHeadroomMB <= 0 {
		logger.Info("in-process component compiles are not checked against the memory left (--component-compile-min-headroom-mb is 0)")
		return nil
	}
	mon := NewMemoryMonitor(0) // 0: never cached, because a compile decision needs the memory left NOW
	mi := mon.Read()
	switch mi.Source {
	case "cgroupv2":
	case "procmeminfo":
		logger.Warn("no cgroup v2 memory limit was found, so the memory left for an in-process component compile is the HOST's available memory (/proc/meminfo), which a container with a limit or a cgroup v1 limit can exceed; set a cgroup v2 limit for the check to mean what it says")
	default:
		logger.Warn("the memory left in this container could not be read, so in-process component compiles are not checked against it", "source", mi.Source)
		return nil
	}
	need := uint64(*componentCompileMinHeadroomMB) << 20
	logger.Info("in-process component compiles are refused when the memory left is under the minimum", "min_headroom_mib", *componentCompileMinHeadroomMB, "source", mi.Source,
		"total_mib", mi.TotalBytes>>20, "available_mib", mi.AvailableBytes>>20)
	return []engine.WasmtimeOption{engine.WithWasmtimeCompileHeadroom(compileHeadroomReader(mon.Read), need, componentCompileRefusalWindowOrDefault(), mi.TotalBytes)}
}

// componentCompileRefusalWindowOrDefault is --component-precompile-refusal-window, the back-off a refused component waits whether its child was killed or the memory was short.
func componentCompileRefusalWindowOrDefault() time.Duration {
	if *componentPrecompileRefusalWindow > 0 {
		return *componentPrecompileRefusalWindow
	}
	return engine.DefaultComponentPrecompileRefusalWindow
}
