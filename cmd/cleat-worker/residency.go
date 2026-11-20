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
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("residency flags: %w", err)
	}
	return cfg, nil
}
