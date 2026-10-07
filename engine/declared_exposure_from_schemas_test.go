package engine

import (
	"strings"
	"testing"
)

// cleat#1986, Python half. DeclaredExposureFromSchemas is the fallback a
// deploy path reads when wasm.Metadata carries no declaration of its own --
// today, any non-Go language, since Python's build has no metadata write
// path and rides its <wasm>.schema.json sidecar's "exposure" key instead.
func TestDeclaredExposureFromSchemas(t *testing.T) {
	cases := []struct {
		name    string
		schemas map[string]EntryPointSchema
		want    ExposureClass
		wantErr string // substring; empty means no error
		why     string
	}{
		{
			name:    "nil map",
			schemas: nil,
			want:    "",
			why:     "no sidecar at all -- the caller's own nil check already handles this, but the function must too",
		},
		{
			name:    "no entry declares anything",
			schemas: map[string]EntryPointSchema{"PlaceOrder": {}},
			want:    "",
			why:     "an older sidecar, or one from before this field existed -- absence, not auth",
		},
		{
			name:    "one entry declares internal",
			schemas: map[string]EntryPointSchema{"InventorySync": {Exposure: ExposureInternal}},
			want:    ExposureInternal,
			why:     "the one case Python has today: exactly one entry per sidecar",
		},
		{
			name: "two entries agree",
			schemas: map[string]EntryPointSchema{
				"A": {Exposure: ExposureInternal},
				"B": {Exposure: ExposureInternal},
			},
			want: ExposureInternal,
			why:  "no live Python caller produces this yet, but agreement must not be refused as if it were a conflict",
		},
		{
			name: "one declares, one is silent",
			schemas: map[string]EntryPointSchema{
				"A": {Exposure: ExposureInternal},
				"B": {},
			},
			want: ExposureInternal,
			why:  "an entry with no opinion does not dilute another's declaration",
		},
		{
			name: "two entries disagree",
			schemas: map[string]EntryPointSchema{
				"A": {Exposure: ExposureInternal},
				"B": {Exposure: ExposureAuth},
			},
			want:    "",
			wantErr: "disagree",
			why: "mirrors Go's own rule for //cleat:exposure: a build declares one class for the " +
				"whole definition, not one per entry point -- refused, not merged",
		},
		{
			name:    "an unparseable class",
			schemas: map[string]EntryPointSchema{"A": {Exposure: ExposureClass("sideways")}},
			want:    "",
			wantErr: "none of",
			why: "the value came from the sidecar file, which is as untrusted as a wasm.Metadata " +
				"stamp -- a malformed value must not read as an absence and deploy as auth",
		},
		{
			name: "an unparseable class beside a valid one",
			schemas: map[string]EntryPointSchema{
				"A": {Exposure: ExposureInternal},
				"B": {Exposure: ExposureClass("sideways")},
			},
			want:    "",
			wantErr: "none of",
			why:     "the malformed entry must be caught regardless of iteration order over the map",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeclaredExposureFromSchemas(tc.schemas)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused a legal sidecar: %v\n%s", err, tc.why)
				}
				if got != tc.want {
					t.Errorf("= %q, want %q\n%s", got, tc.want, tc.why)
				}
				return
			}

			if err == nil {
				t.Fatalf("allowed it and returned %q, but this must be refused.\n%s", got, tc.why)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("refused, but not for the stated reason.\n  message: %s\n  want a message "+
					"containing %q\n%s", err, tc.wantErr, tc.why)
			}
			if got != "" {
				t.Errorf("a refused sidecar still returned a class (%q); a caller that ignores the "+
					"error would stamp it", got)
			}
		})
	}
}
