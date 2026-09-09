package main

import (
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// TestNonGoMetadataPassesValidate is the regression test for cleat#1077.
//
// The Rust, Java and AssemblyScript build paths wrote
// `&wasm.Metadata{Language: "..."}` and nothing else, so every other field
// took its zero value. `cleat deploy` then rejected the binary that
// `cleat build` had just produced, because runDeploy calls Validate() and
// exits 1 -- measured on a real AssemblyScript guest:
//
//	lang="assemblyscript" abi=0 minCompat=0 name="" wfVersion=0
//	Validate() -> metadata: workflow_name is empty
//
// Validate() is the exact predicate that failed, so it is what this asserts:
// not a field-by-field restatement, which would pass while disagreeing with
// the thing that actually rejects the binary.
func TestNonGoMetadataPassesValidate(t *testing.T) {
	for _, lang := range []string{"rust", "java", "assemblyscript"} {
		t.Run(lang, func(t *testing.T) {
			m := nonGoMetadata(lang, "my_workflow", 3)
			if err := m.Validate(); err != nil {
				t.Fatalf("metadata for %s does not validate: %v\n\n"+
					"`cleat deploy` runs exactly this check and exits 1 on failure, "+
					"so a binary built with it cannot be deployed. See cleat#1077.", lang, err)
			}
			if m.Language != lang {
				t.Errorf("Language = %q, want %q", m.Language, lang)
			}
			if m.WorkflowName != "my_workflow" {
				t.Errorf("WorkflowName = %q, want %q", m.WorkflowName, "my_workflow")
			}
			if m.WorkflowVersion != 3 {
				t.Errorf("WorkflowVersion = %d, want 3: the --version flag is threaded "+
					"through to non-Go targets, not defaulted away", m.WorkflowVersion)
			}
			// The field cleat#1054 will compare. It was 0 on every non-Go guest,
			// so enforcement would have rejected all of them.
			if m.ABIVersion != wasm.CurrentABIVersion {
				t.Errorf("ABIVersion = %d, want %d (wasm.CurrentABIVersion)",
					m.ABIVersion, wasm.CurrentABIVersion)
			}
		})
	}
}

// A non-positive version must not silently produce metadata that fails at
// deploy time. `cleat build` defaults --version to 1, but the guard is here
// rather than trusted from the caller: the whole defect in #1077 was a value
// that was wrong at build time and only rejected by a later command.
func TestNonGoMetadataRejectsNonPositiveVersion(t *testing.T) {
	for _, v := range []int{0, -1} {
		m := nonGoMetadata("rust", "wf", v)
		if err := m.Validate(); err != nil {
			t.Errorf("version %d produced metadata that fails Validate(): %v", v, err)
		}
		if m.WorkflowVersion <= 0 {
			t.Errorf("version %d was carried through as %d", v, m.WorkflowVersion)
		}
	}
}
