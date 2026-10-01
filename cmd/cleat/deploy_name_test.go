package main

import (
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// TestResolveDeployName is the regression test for cleat#2842.
//
// `cleat deploy`'s two no-flag fallbacks disagreed about a ".wasm" suffix:
// the metadata branch assigned meta.WorkflowName as-is, which cmd/cleat
// build's own bug embedded WITH the suffix, while the no-metadata branch
// already trimmed wasmPath's own suffix. So `cleat init my-workflow` built
// then `cleat deploy --dry-run`'d printed `Would deploy workflow
// "my-workflow.wasm"` -- a name /api/workflows/<name>/start cannot find,
// because the registered definition is under the untrimmed one.
//
// The direct control is the LAST case below: metadata present, its
// WorkflowName still carrying ".wasm" (simulating a binary built before
// cleat#2842's build-side fix), asserting the deploy-time trim alone --
// with no rebuild -- produces the same name the no-metadata fallback would.
func TestResolveDeployName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		nameFlag string
		meta     *wasm.Metadata
		wasmPath string
		want     string
	}{
		{
			name:     "flag wins over everything",
			nameFlag: "explicit-name",
			meta:     &wasm.Metadata{WorkflowName: "my-workflow"},
			wasmPath: "/build/output.wasm",
			want:     "explicit-name",
		},
		{
			name:     "metadata name, already correct (post-cleat#2842 build)",
			nameFlag: "",
			meta:     &wasm.Metadata{WorkflowName: "my-workflow"},
			wasmPath: "/build/my-workflow.wasm",
			want:     "my-workflow",
		},
		{
			name:     "no metadata at all -- falls back to the wasm filename",
			nameFlag: "",
			meta:     nil,
			wasmPath: "/build/my-workflow.wasm",
			want:     "my-workflow",
		},
		{
			name:     "metadata name is the \"unknown\" sentinel -- treated as absent",
			nameFlag: "",
			meta:     &wasm.Metadata{WorkflowName: "unknown"},
			wasmPath: "/build/my-workflow.wasm",
			want:     "my-workflow",
		},
		{
			name:     "cleat#2842's exact failure: metadata name still carries .wasm",
			nameFlag: "",
			meta:     &wasm.Metadata{WorkflowName: "my-workflow.wasm"},
			wasmPath: "/build/my-workflow.wasm",
			want:     "my-workflow",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveDeployName(tc.nameFlag, tc.meta, tc.wasmPath); got != tc.want {
				t.Errorf("resolveDeployName(%q, %+v, %q) = %q, want %q",
					tc.nameFlag, tc.meta, tc.wasmPath, got, tc.want)
			}
		})
	}
}

// TestResolveDeployNameAgreesRegardlessOfMetadataPresence is the property
// cleat#2842 broke: for the SAME workflow, deploying with metadata present
// and deploying with it absent (or, equivalently, present but still
// suffixed, as an old binary would be) must produce the SAME default name.
// A regression here means a workflow's registered name depends on whether
// cleat.metadata happened to be embedded, not on the workflow itself.
func TestResolveDeployNameAgreesRegardlessOfMetadataPresence(t *testing.T) {
	wasmPath := "/build/my-workflow.wasm"

	withFixedMetadata := resolveDeployName("", &wasm.Metadata{WorkflowName: "my-workflow"}, wasmPath)
	withStaleMetadata := resolveDeployName("", &wasm.Metadata{WorkflowName: "my-workflow.wasm"}, wasmPath)
	withNoMetadata := resolveDeployName("", nil, wasmPath)

	if withFixedMetadata != withNoMetadata {
		t.Errorf("fixed-metadata default %q disagrees with no-metadata default %q",
			withFixedMetadata, withNoMetadata)
	}
	if withStaleMetadata != withNoMetadata {
		t.Errorf("stale-metadata (pre-cleat#2842 binary) default %q disagrees with "+
			"no-metadata default %q -- an old binary would deploy under the wrong name "+
			"even after this fix, with no way to tell from the CLI output", withStaleMetadata, withNoMetadata)
	}
}
