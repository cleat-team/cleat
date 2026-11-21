package main

import (
	"fmt"

	"github.com/cleat-team/cleat/engine"
)

// residencyConfigFromFlags is the residency policy as the flags give it, validated. It is the one place the flags become a policy, so that a test can
// run exactly what main runs (the shape of applyEngineGlobalsFromFlags, cleat#4212): a flag that parses and changes nothing is what that test exists for.
func residencyConfigFromFlags() (engine.ResidencyConfig, error) {
	cfg := engine.DefaultResidencyConfig()
	cfg.MaxRuns = *residentMaxRuns
	if *residentMemoryBudgetMB < 0 {
		return cfg, fmt.Errorf("--resident-memory-budget-mb %d is negative", *residentMemoryBudgetMB)
	}
	cfg.MemoryBudgetBytes = uint64(*residentMemoryBudgetMB) << 20
	cfg.WindowFloor = *residentWindowFloor
	cfg.WindowCap = *residentWindowCap
	cfg.ReplayMultiplier = *residentReplayMultiplier
	cfg.ReplayFixed = *residentReplayFixed
	cfg.ReplayPerEvent = *residentReplayPerEvent
	// A HELD signal wait asks the store whether its delivery has arrived at the worker's own poll interval, so a held run is never later than the same run with residency off, which
	// waits for the next claim poll (and then replays). Only a positive --poll overrides the engine's default (the same 500ms): a zero or negative value is not an interval, and it must
	// not fail the boot of a worker that does not use residency at all, which Validate would do for a negative one.
	if *pollInterval > 0 {
		cfg.RecheckInterval = *pollInterval
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("residency flags: %w", err)
	}
	return cfg, nil
}
