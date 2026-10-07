package main

import (
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// The build-time refusal for cleat#1986 slice 2c.
//
// This is the half of the declaration path that deploy CANNOT cover, which is
// why it is tested separately from wasm's extraction tests. A conflicting
// declaration produces two values that are each in the closed set, so the
// column's CHECK accepts whichever one the build stamped -- the database has no
// way to see that a second file disagreed. An unknown value would be caught
// there, one slow loop later.
func TestExposureDeclarationError(t *testing.T) {
	cases := []struct {
		name string
		// nil means the caller passed no UsageInfo at all.
		usage   *wasm.UsageInfo
		wantErr bool
		// mustName, when set, is a substring the message has to carry.
		mustName string
		why      string
	}{
		{
			name:  "no usage info",
			usage: nil,
			why: "a nil result is not a declaration and must not panic; runBuild cannot reach " +
				"this today, and the guard is what keeps a future caller's mistake quiet.",
		},
		{
			name:  "nothing declared",
			usage: &wasm.UsageInfo{},
			why: "empty means the source expressed no opinion, which the deploy path lets stand. " +
				"Refusing here would make every workflow's class look source-declared.",
		},
		{
			name:  "auth declared explicitly",
			usage: &wasm.UsageInfo{Exposure: "auth"},
			why:   "the default is a legal thing to declare.",
		},
		{
			name:  "internal declared",
			usage: &wasm.UsageInfo{Exposure: "internal"},
		},
		{
			name:  "public declared",
			usage: &wasm.UsageInfo{Exposure: "public"},
			why: "public is a legal CLASS. Whether THIS tenant may deploy one is a per-tenant " +
				"policy question the build cannot answer -- no tenant is in scope here -- so it " +
				"belongs to `cleat deploy`, which refuses it until the opt-in exists.",
		},
		{
			name:     "unknown class",
			usage:    &wasm.UsageInfo{Exposure: "secrets"},
			wantErr:  true,
			mustName: `"auth", "public" or "internal"`,
			why: "the message has to name the accepted set, because the reader's next move is to " +
				"edit the declaration and nothing else tells them what is legal.",
		},
		{
			name:     "wrong case",
			usage:    &wasm.UsageInfo{Exposure: "Internal"},
			wantErr:  true,
			mustName: `"internal"`,
			why: "engine.ParseExposure is an exact match and the column's CHECK is case-sensitive, " +
				"so a case-folding check here would pass a build the database rejects.",
		},
		{
			name:     "conflicting declarations",
			usage:    &wasm.UsageInfo{ExposureConflict: "auth, internal"},
			wantErr:  true,
			mustName: "auth, internal",
			why: "the message must name both values -- the repair is to delete one, and the author " +
				"needs to know which two.",
		},
		{
			name:     "a conflict is still reported beside a valid value",
			usage:    &wasm.UsageInfo{Exposure: "auth", ExposureConflict: "auth, internal"},
			wantErr:  true,
			mustName: "conflicting",
			why: "collectExposure clears Exposure when it reports a conflict, so the two fields " +
				"cannot disagree today. If that ever changes, a value that happens to be legal must " +
				"not make the function report success and let the build stamp one of the two " +
				"classes that disagreed.",
		},
		{
			name:     "a conflict takes precedence over an invalid value",
			usage:    &wasm.UsageInfo{Exposure: "secrets", ExposureConflict: "auth, internal"},
			wantErr:  true,
			mustName: "conflicting",
			why: "TWO CASES RATHER THAN ONE, because a single case with a VALID value cannot tell " +
				"the orders apart -- swapping the two checks still reports the conflict for it, so " +
				"it would pass under either. This one separates them: an invalid value makes the " +
				"value check fire, so only the conflict check running FIRST produces this message. " +
				"The author's repair differs by which it is -- delete a declaration, or fix one " +
				"value -- and in a package with two declarations the value may be the wrong one " +
				"to edit.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := exposureDeclarationError(tc.usage)

			if tc.wantErr {
				if msg == "" {
					t.Fatalf("no refusal was produced, but this declaration cannot be stamped.\n%s",
						tc.why)
				}
				if tc.mustName != "" && !strings.Contains(msg, tc.mustName) {
					t.Errorf("message does not carry %q:\n  %s\n%s", tc.mustName, msg, tc.why)
				}
				return
			}
			if msg != "" {
				t.Errorf("refused a declaration that can be stamped: %q\n%s", msg, tc.why)
			}
		})
	}
}
