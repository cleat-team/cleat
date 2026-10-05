package engine

import (
	"errors"
	"fmt"
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
// than three times over in cmd/, because it is deliberately one decision.
//
// Every "public" case below runs twice, with tenantAllowsPublic false and
// true, because that argument is this test's whole subject since cleat#1986's
// enforcement slice: a case that only exercised one value could not tell a
// working gate from one that always refuses, or one that never does.
func TestResolveDeployableExposureGatesPublic(t *testing.T) {
	// gatedByOptIn: the resolved class is `public`, so the outcome genuinely
	// depends on tenantAllowsPublic -- want applies when it is true, and a
	// not-opted-in tenant is refused regardless of want.
	//
	// Everything else (including a loosening) is INDEPENDENT of the opt-in:
	// a loosening is refused before ResolveDeployableExposure ever looks at
	// tenantAllowsPublic, so it must refuse the same way whether the tenant
	// is opted in or not -- that is the case this table's first draft got
	// wrong, by assuming any non-success case was opt-in-gated.
	cases := []struct {
		name          string
		declared      ExposureClass
		requested     ExposureClass
		gatedByOptIn  bool
		want          ExposureClass // when gatedByOptIn: the class when allowed=true
		wantErrSubstr string        // when !gatedByOptIn and non-empty: wanted regardless of allowed
	}{
		{"nothing declared anywhere", "", "", false, ExposureAuth, ""},
		{"a requested public", "", ExposurePublic, true, ExposurePublic, ""},
		{"a DECLARED public", ExposurePublic, "", true, ExposurePublic, ""},
		{"a declared public tightened away", ExposurePublic, ExposureInternal, false, ExposureInternal, ""},
		{"a declared public tightened to auth", ExposurePublic, ExposureAuth, false, ExposureAuth, ""},
		{"declared internal, no opinion", ExposureInternal, "", false, ExposureInternal, ""},
		{"a loosening, passed through", ExposureInternal, ExposureAuth, false, "", "less restrictive"},
	}

	for _, tc := range cases {
		for _, allowed := range []bool{false, true} {
			name := fmt.Sprintf("%s/opted-in=%v", tc.name, allowed)
			t.Run(name, func(t *testing.T) {
				got, err := ResolveDeployableExposure(tc.declared, tc.requested, allowed, "t1")

				switch {
				case tc.gatedByOptIn && !allowed:
					if err == nil {
						t.Fatalf("not-opted-in tenant allowed, returned %q", got)
					}
					if !strings.Contains(err.Error(), "opt-in") {
						t.Errorf("message does not name the opt-in gate: %s", err)
					}
				case tc.gatedByOptIn && allowed:
					if err != nil {
						t.Fatalf("opted-in tenant still refused: %v", err)
					}
					if got != tc.want {
						t.Errorf("= %q, want %q", got, tc.want)
					}
				case tc.wantErrSubstr != "":
					if err == nil {
						t.Fatalf("allowed=%v: expected a refusal containing %q, got %q", allowed, tc.wantErrSubstr, got)
					}
					if !strings.Contains(err.Error(), tc.wantErrSubstr) {
						t.Errorf("allowed=%v: message does not contain %q: %s", allowed, tc.wantErrSubstr, err)
					}
				default:
					if err != nil {
						t.Fatalf("allowed=%v: refused a deployable class: %v", allowed, err)
					}
					if got != tc.want {
						t.Errorf("allowed=%v: = %q, want %q", allowed, got, tc.want)
					}
				}
			})
		}
	}
}

// The two refusals a deploy can meet must be distinguishable by TYPE, because
// they send the reader to different places: the public gate is a per-tenant
// policy question, while a loosening is the operator's request contradicting
// the artifact. A caller that could only match on the message would break the
// first time either string was reworded.
func TestResolveDeployableExposureKeepsTheTwoRefusalsApart(t *testing.T) {
	_, pubErr := ResolveDeployableExposure("", ExposurePublic, false, "t1")
	var notOptedIn *ErrExposurePublicNotOptedIn
	if !errors.As(pubErr, &notOptedIn) {
		t.Errorf("the public gate is not an *ErrExposurePublicNotOptedIn: %v", pubErr)
	}
	var loosened *ErrExposureLoosened
	if errors.As(pubErr, &loosened) {
		t.Errorf("the public gate also satisfies the LOOSENING type, so the two cannot be told apart: %v", pubErr)
	}

	_, loosErr := ResolveDeployableExposure(ExposureInternal, ExposureAuth, false, "t1")
	if !errors.As(loosErr, &loosened) {
		t.Errorf("a loosening through the deployable wrapper is not an *ErrExposureLoosened: %v", loosErr)
	}
	if errors.As(loosErr, &notOptedIn) {
		t.Errorf("a loosening also satisfies the PUBLIC type, so the two cannot be told apart: %v", loosErr)
	}
}

// ErrExposurePublicNotOptedIn's message names the tenant when one was
// supplied, and degrades to a generic phrase rather than an empty or
// malformed sentence when it was not -- the case ResolveDeployableExposure's
// own callers hit when a store does not implement the reader at all.
func TestErrExposurePublicNotOptedInNamesTheTenantWhenItHasOne(t *testing.T) {
	_, withTenant := ResolveDeployableExposure("", ExposurePublic, false, "tenant-123")
	if !strings.Contains(withTenant.Error(), "tenant tenant-123") {
		t.Errorf("message does not name the tenant: %s", withTenant)
	}

	_, noTenant := ResolveDeployableExposure("", ExposurePublic, false, "")
	if !strings.Contains(noTenant.Error(), "this tenant") {
		t.Errorf("message with no tenant id does not fall back to the generic phrase: %s", noTenant)
	}
}

func orNone(c ExposureClass) string {
	if c == "" {
		return "(none)"
	}
	return string(c)
}
