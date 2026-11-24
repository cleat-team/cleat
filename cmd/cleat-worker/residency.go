package main

import (
	"fmt"
	"log/slog"

	"github.com/cleat-team/cleat/engine"
)

// residencyProbeRoot and residencyPhysical are what a test replaces: the filesystem the memory limit is read from, and the host-memory source for a platform with no
// /proc/meminfo. In the worker they are "/" and the platform's own.
var (
	residencyProbeRoot = "/"
	residencyPhysical  = physicalMemory
)

// fallbackResidentRuns is how many runs of the dearest language the budget allows when the worker's memory cannot be read: the bound residency had before it was a
// percentage (8 runs), kept for the one case where there is no number to take a percentage of.
const fallbackResidentRuns = 8

// residencyConfigFromFlags is the residency policy as the flags give it, validated. It is the one place the flags become a policy, so that a test can
// run exactly what main runs (the shape of applyEngineGlobalsFromFlags, cleat#4212): a flag that parses and changes nothing is what that test exists for.
func residencyConfigFromFlags() (engine.ResidencyConfig, error) {
	cfg := engine.DefaultResidencyConfig()
	cfg.MaxRuns = *residentMaxRuns
	if *residentMemoryBudgetMB < 0 {
		return cfg, fmt.Errorf("--resident-memory-budget-mb %d is negative", *residentMemoryBudgetMB)
	}
	cfg.MemoryBudgetBytes = uint64(*residentMemoryBudgetMB) << 20
	// The percentage is validated even when residency is off, like every --resident-* flag: a value that cannot work stops the worker at boot.
	if err := validateResidentMemoryPercent(*residentMemoryPercent); err != nil {
		return cfg, err
	}
	if *residentMemoryPercent == 0 {
		cfg.MaxRuns = 0
	}
	cfg.WindowFloor = *residentWindowFloor
	cfg.WindowCap = *residentWindowCap
	cfg.ReplayMultiplier = *residentReplayMultiplier
	cfg.ReplayFixed = *residentReplayFixed
	cfg.ReplayPerEvent = *residentReplayPerEvent
	// A HELD signal wait asks the store whether its delivery has arrived at the worker's own poll interval, so on a dialect with no NOTIFY a held run waits at most one interval for its signal, where the same run with residency off waits for the next claim poll and then
	// replays. On Postgres the NOTIFY listener nudges the held waits instead (ResidentSet.Nudge), and the interval is only the backstop. Only a positive --poll overrides the engine's default (the same 500ms): a zero or negative value is not an interval, and it must
	// not fail the boot of a worker that does not use residency at all, which Validate would do for a negative one.
	if *pollInterval > 0 {
		cfg.RecheckInterval = *pollInterval
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("residency flags: %w", err)
	}
	deriveResidencyBudget(&cfg)
	return cfg, nil
}

// deriveResidencyBudget sets cfg.MemoryBudgetBytes from --resident-memory-percent of the worker's memory, unless an explicit --resident-memory-budget-mb is already in it
// or residency is off, and logs the figure and where it came from, once, at boot (cleat#4377). It never fails the boot: a worker whose memory cannot be read still starts,
// with the old fixed bound and a warning, because residency is an optimisation and a refusal to start over it would be the wrong trade.
func deriveResidencyBudget(cfg *engine.ResidencyConfig) {
	if cfg.MaxRuns <= 0 {
		return
	}
	if cfg.MemoryBudgetBytes > 0 {
		slog.Info("residency memory budget", "bytes", cfg.MemoryBudgetBytes, "mib", cfg.MemoryBudgetBytes>>20, "source", "--resident-memory-budget-mb")
		return
	}
	mem, err := probeAvailableMemory(residencyProbeRoot, residencyPhysical)
	if err != nil {
		cfg.MemoryBudgetBytes = engine.DearestResidentInstanceBytes() * fallbackResidentRuns
		slog.Warn("residency memory budget: the worker's memory could not be read, so the budget is the fixed bound of "+fmt.Sprint(fallbackResidentRuns)+
			" runs of the dearest language, not a share of memory; set --resident-memory-budget-mb to choose one",
			"bytes", cfg.MemoryBudgetBytes, "mib", cfg.MemoryBudgetBytes>>20, "error", err)
		return
	}
	cfg.MemoryBudgetBytes = residencyBudgetBytes(mem.Bytes, *residentMemoryPercent)
	slog.Info("residency memory budget", "bytes", cfg.MemoryBudgetBytes, "mib", cfg.MemoryBudgetBytes>>20,
		"percent", *residentMemoryPercent, "of_bytes", mem.Bytes, "of_mib", mem.Bytes>>20, "source", mem.Source)
	if mem.Warning != "" {
		slog.Warn("residency memory budget: " + mem.Warning)
	}
}
