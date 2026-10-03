package engine

import (
	"errors"
	"strings"
	"testing"
)

// cleat#1986 slice 2c-ii: the tighten-only rule, as a function.
//
// This is a TABLE over the whole (declared, requested) matrix rather than the
// cases that happen to be interesting, because the rule is a total function on
// two three-valued inputs plus "" and every cell is reachable from a real
// deploy: the declared value comes from an artifact's metadata and the requested
// one from a flag or a manifest. Nine real combinations, and the ones that
// matter most are the two that must FAIL.
func TestResolveExposureTightenOnly(t *testing.T) {
	cases := []struct {
		declared  ExposureClass
		requested ExposureClass
		want      ExposureClass
		wantErr   string // substring; empty means no error
		why       string
	}{
		// --- nothing declared: the caller's class stands, and auth when they too had none.
		{"", "", ExposureAuth, "", "the pre-existing default, unchanged by this slice"},
		{"", ExposureAuth, ExposureAuth, "", ""},
		{"", ExposureInternal, ExposureInternal, "",
			"a manifest may set any class when the source declared none -- there is nothing to loosen"},
		{"", ExposurePublic, ExposurePublic, "",
			"no declaration, so no loosening. `public` is refused elsewhere, at the per-tenant " +
				"opt-in, and that refusal is not this function's business"},

		// --- declared auth: the default class, so most calls are legal.
		{ExposureAuth, "", ExposureAuth, "", "no manifest opinion, so the declaration is the answer"},
		{ExposureAuth, ExposureAuth, ExposureAuth, "", ""},
		{ExposureAuth, ExposureInternal, ExposureInternal, "", "tightening"},
		{ExposureAuth, ExposurePublic, "", "less restrictive", "LOOSENING, and the central case"},

		// --- declared internal: the class that must not be given away.
		{ExposureInternal, "", ExposureInternal, "",
			"THE CASE THE EMPTY DEFAULT EXISTS FOR: an omitted --exposure must not read as `auth`"},
		{ExposureInternal, ExposureInternal, ExposureInternal, "", ""},
		{ExposureInternal, ExposureAuth, "", "less restrictive",
			"an operator asking for auth on an internal workflow is refused, not clamped"},
		{ExposureInternal, ExposurePublic, "", "less restrictive", ""},

		// --- declared public: tightenable in both directions, which is the point of the order.
		{ExposurePublic, "", ExposurePublic, "", ""},
		{ExposurePublic, ExposurePublic, ExposurePublic, "", ""},
		{ExposurePublic, ExposureAuth, ExposureAuth, "", "tightening to the default"},
		{ExposurePublic, ExposureInternal, ExposureInternal, "", "tightening all the way"},

		// --- an unrecognised DECLARED class: the fail-open cases.
		//
		// These are the reason the declared value is validated rather than
		// treated as absent when it does not parse. It arrives from the
		// artifact's metadata, which is untrusted at deploy time, so a stray
		// space or a case slip must not silently become "no declaration" --
		// that would deploy an internal workflow as auth while the operator's
		// own command line says internal.
		{"internal ", "", "", "not one of", "a trailing space is not an absence; it is a malformed stamp"},
		{"Internal", "", "", "not one of", "ParseExposure is an exact match and the column's CHECK is case-sensitive"},
		{"secrets", "", "", "not one of", ""},
		{"internal ", ExposureAuth, "", "not one of",
			"and it is refused even when the request would otherwise be a LOOSENING, because " +
				"which refusal you get decides what you edit"},
		// An unrecognised REQUESTED class belongs to the caller, not the artifact.
		{"", "sideways", "", "not one of", ""},
		{ExposureInternal, "sideways", "", "not one of", ""},
	}

	for _, tc := range cases {
		name := "declared=" + orNone(tc.declared) + "_requested=" + orNone(tc.requested)
		t.Run(name, func(t *testing.T) {
			got, err := ResolveExposure(tc.declared, tc.requested)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused a legal deploy: %v\n%s", err, tc.why)
				}
				if got != tc.want {
					t.Errorf("= %q, want %q\n%s", got, tc.want, tc.why)
				}
				return
			}

			if err == nil {
				t.Fatalf("allowed it and returned %q, but this deploy must be refused.\n%s", got, tc.why)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("refused, but not for the stated reason.\n  message: %s\n  want a "+
					"message containing %q\n%s", err, tc.wantErr, tc.why)
			}
			if got != "" {
				t.Errorf("a refused deploy still returned a class (%q); a caller that ignores the "+
					"error would stamp it", got)
			}
		})
	}
}

// The two refusals are different KINDS, and a caller has to be able to tell them
// apart: `ErrExposureLoosened` means the operator's request contradicts the
// source and they should edit the DEPLOY, while a malformed class means edit the
// ARTIFACT or the flag. Same class of distinction as the guard exit statuses --
// `1` sends you to the thing named, `2` says the check is broken.
func TestResolveExposureDistinguishesLooseningFromMalformed(t *testing.T) {
	_, err := ResolveExposure(ExposureInternal, ExposureAuth)
	var loosened *ErrExposureLoosened
	if !errors.As(err, &loosened) {
		t.Fatalf("a loosening refusal is not an *ErrExposureLoosened, so a caller cannot branch "+
			"on it: %v", err)
	}
	if loosened.Declared != ExposureInternal || loosened.Requested != ExposureAuth {
		t.Errorf("the error carries declared=%q requested=%q, want both named so the message can "+
			"say which two classes disagree", loosened.Declared, loosened.Requested)
	}

	// The control: a malformed class must NOT satisfy the loosening type, or the
	// branch above would be true for every refusal and separate nothing.
	_, err = ResolveExposure("secrets", "")
	if errors.As(err, &loosened) {
		t.Errorf("a MALFORMED declared class was reported as a loosening: %v", err)
	}
}

// ResolveDeployableExposure is what the three deploy paths actually call: the
// tighten-only rule plus the `public` gate. The gate is asserted here rather
// than three times over in cmd/, because it is deliberately one decision --
// when the per-tenant opt-in lands, this is the single condition to relax.
func TestResolveDeployableExposureGatesPublic(t *testing.T) {
	cases := []struct {
		name      string
		declared  ExposureClass
		requested ExposureClass
		want      ExposureClass
		wantErr   string
	}{
		{"nothing declared anywhere", "", "", ExposureAuth, ""},
		{"a requested public", "", ExposurePublic, "", "public"},
		{"a DECLARED public", ExposurePublic, "", "", "public"},
		{"a declared public tightened away", ExposurePublic, ExposureInternal, ExposureInternal, ""},
		{"a declared public tightened to auth", ExposurePublic, ExposureAuth, ExposureAuth, ""},
		{"declared internal, no opinion", ExposureInternal, "", ExposureInternal, ""},
		{"a loosening, passed through", ExposureInternal, ExposureAuth, "", "less restrictive"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveDeployableExposure(tc.declared, tc.requested)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused a deployable class: %v", err)
				}
				if got != tc.want {
					t.Errorf("= %q, want %q", got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("allowed and returned %q", got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("message does not contain %q: %s", tc.wantErr, err)
			}
		})
	}
}

// The two refusals a deploy can meet must be distinguishable by TYPE, because
// they send the reader to different places: the public gate is a policy that
// will lift when the opt-in ships, while a loosening is the operator's request
// contradicting the artifact. A caller that could only match on the message
// would break the first time either string was reworded.
func TestResolveDeployableExposureKeepsTheTwoRefusalsApart(t *testing.T) {
	_, pubErr := ResolveDeployableExposure("", ExposurePublic)
	var unavailable *ErrExposurePublicUnavailable
	if !errors.As(pubErr, &unavailable) {
		t.Errorf("the public gate is not an *ErrExposurePublicUnavailable: %v", pubErr)
	}
	var loosened *ErrExposureLoosened
	if errors.As(pubErr, &loosened) {
		t.Errorf("the public gate also satisfies the LOOSENING type, so the two cannot be told apart: %v", pubErr)
	}

	_, loosErr := ResolveDeployableExposure(ExposureInternal, ExposureAuth)
	if !errors.As(loosErr, &loosened) {
		t.Errorf("a loosening through the deployable wrapper is not an *ErrExposureLoosened: %v", loosErr)
	}
	if errors.As(loosErr, &unavailable) {
		t.Errorf("a loosening also satisfies the PUBLIC type, so the two cannot be told apart: %v", loosErr)
	}
}

func orNone(c ExposureClass) string {
	if c == "" {
		return "(none)"
	}
	return string(c)
}
