package main

import "github.com/cleat-team/cleat/wasm"

// nonGoMetadata builds the cleat.metadata block for a non-Go build target.
//
// The Go path (runBuild) fills this struct out in full. The Rust, Java and
// AssemblyScript paths used to write `&wasm.Metadata{Language: "..."}` and
// nothing else, so every other field took its zero value -- and
// `cleat deploy` rejected the result, because runDeploy calls
// meta.Validate() and exits 1. Measured 2026-09-09 on a guest that
// `cleat build --target assemblyscript` had just produced:
//
//	lang="assemblyscript" abi=0 minCompat=0 name="" wfVersion=0
//	Validate() -> metadata: workflow_name is empty
//
// Validate() short-circuits, so that one message is the first of four:
// workflow_name, workflow_version, abi_version and min_compatible_version
// each fail in turn as the previous is repaired. Fixing only what the
// message names leaves three.
//
// Note that the Python path, which writes NO metadata section at all, was
// unaffected -- runDeploy reports "no cleat.metadata section found" and
// continues with flags-only configuration. Partial metadata was worse than
// none, which is why this returns a struct that passes Validate() rather
// than one that carries a little more than before.
//
// See cleat#1077. The ABI fields matter beyond deploy: cleat#1054 proposes
// comparing a guest's abi_version against the host's, and until this landed
// every non-Go guest carried 0 against a host CurrentABIVersion of 1.
func nonGoMetadata(language, workflowName string, workflowVersion int) *wasm.Metadata {
	// `cleat build` defaults --version to 1; guard anyway, because a
	// non-positive value here fails Validate() at deploy time rather than
	// here, which is the displacement this whole change is about.
	if workflowVersion <= 0 {
		workflowVersion = 1
	}
	return &wasm.Metadata{
		WorkflowName:         workflowName,
		WorkflowVersion:      workflowVersion,
		ABIVersion:           wasm.CurrentABIVersion,
		MinCompatibleVersion: wasm.CurrentABIVersion,
		Language:             language,
	}
}
