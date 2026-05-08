package main

// VetResult represents a single issue found by a vet analyzer.
type VetResult struct {
	Code       string   `json:"code"`
	File       string   `json:"file"`
	Line       int      `json:"line"`
	Column     int      `json:"column"`
	Message    string   `json:"message"`
	Suggestion string   `json:"suggestion"`
	Chain      []string `json:"chain,omitempty"`
}

// VetOutput is the top-level JSON output structure for vet results.
type VetOutput struct {
	Errors   []VetResult `json:"errors"`
	Warnings []VetResult `json:"warnings"`
	Summary  VetSummary  `json:"summary"`
}

// VetSummary aggregates the vet analysis counts.
type VetSummary struct {
	Functions      int `json:"functions"`
	DurableLeaves  int `json:"durable_leaves"`
	DurableClosure int `json:"durable_closure"`
	Pure           int `json:"pure"`
}
