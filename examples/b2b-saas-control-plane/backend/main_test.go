package main

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRedactDBFlagHidesTheDSN pins redactDBFlag's one job: the value
// following "--db" never survives it. cleat-review's finding on #2716 --
// runBin formatted the raw args, DSN and password included, into an error
// that createTenant/generateAPIKey/deployWorkflowDef's callers all
// s.log.Error, so a single failed signup put the admin connection string
// into this process's logs.
func TestRedactDBFlagHidesTheDSN(t *testing.T) {
	dsn := "postgres://cleat:super-secret-password@localhost:5432/cleat?sslmode=disable"
	args := []string{"--create-tenant", "acme", "--org", "abc", "--db", dsn}

	got := redactDBFlag(args)

	for _, a := range got {
		if strings.Contains(a, "super-secret-password") {
			t.Fatalf("redactDBFlag(%v) = %v, still contains the DSN's password", args, got)
		}
	}
	if got[len(got)-1] != "REDACTED" {
		t.Errorf("redactDBFlag(%v) = %v, want the --db value replaced with REDACTED", args, got)
	}

	// The rest of the argv is unchanged -- redaction should not blind a
	// reader to which command actually ran.
	want := []string{"--create-tenant", "acme", "--org", "abc", "--db", "REDACTED"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("redactDBFlag(%v)[%d] = %q, want %q", args, i, got[i], w)
		}
	}

	// The input slice itself must not be mutated -- a caller building the
	// real argv for exec.CommandContext right after calling this for a log
	// line must still see the real DSN.
	if args[len(args)-1] != dsn {
		t.Errorf("redactDBFlag mutated its input slice: args[-1] = %q, want the original DSN", args[len(args)-1])
	}
}

// TestRunBinRedactsTheDSNOnFailure exercises runBin itself, not just the
// helper it now calls: the failure this scenario actually hits is a
// nonexistent binary or a refused invocation, and the DSN must not survive
// THAT path either.
func TestRunBinRedactsTheDSNOnFailure(t *testing.T) {
	a := &tenantAdmin{}
	dsn := "postgres://cleat:super-secret-password@localhost:5432/cleat?sslmode=disable"

	_, err := a.runBin(context.Background(), "/bin/false", "--db", dsn)
	if err == nil {
		t.Fatal("runBin with /bin/false: err = nil, want a failure")
	}
	if strings.Contains(err.Error(), "super-secret-password") {
		t.Fatalf("runBin's error leaks the DSN password: %v", err)
	}
}

// TestSanitizeDisplayNameStripsALeadingDash pins the one property that
// matters: whatever a signup's business_name looks like, the value handed
// to createTenant's --tenant-display-name flag can never itself be
// mistaken for a flag by an argv-consuming parser. gosec's G702 flagged
// runBin's exec.CommandContext call for exactly this shape of taint (a
// request field reaching argv) on #2716; this is the falsification for the
// #nosec justification at that call site, not a general string-cleaning
// test.
func TestSanitizeDisplayNameStripsALeadingDash(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain name unchanged", "Acme Corp", "Acme Corp"},
		{"single leading dash stripped", "-rf", "rf"},
		{"flag-shaped input stripped to its body", "--tenant-display-name=x", "tenant-display-name=x"},
		{"interior dash kept", "Acme-Corp", "Acme-Corp"},
		{"control characters become a space", "Acme\tCorp\n", "Acme Corp"},
		{"empty after stripping falls back", "---", "tenant"},
		{"pure whitespace falls back", "   ", "tenant"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeDisplayName(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeDisplayName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.HasPrefix(got, "-") {
				t.Errorf("sanitizeDisplayName(%q) = %q, still starts with '-'", tc.in, got)
			}
		})
	}
}

// TestSanitizeDisplayNameBoundsLength confirms the length guard actually
// bounds the result, and does so on a rune boundary rather than a byte one
// -- a byte-index slice on a non-ASCII business name (an ordinary case for
// a signup form) can otherwise split a multi-byte rune and hand
// createTenant invalid UTF-8 as an argv element.
func TestSanitizeDisplayNameBoundsLength(t *testing.T) {
	// "café" repeated past maxDisplayNameLen runes; "é" is multi-byte, so a
	// byte-index truncation at exactly the rune budget would corrupt it.
	long := strings.Repeat("café ", maxDisplayNameLen)
	got := sanitizeDisplayName(long)
	if n := len([]rune(got)); n > maxDisplayNameLen {
		t.Errorf("sanitizeDisplayName returned %d runes, want <= %d", n, maxDisplayNameLen)
	}
	if !utf8.ValidString(got) {
		t.Errorf("sanitizeDisplayName(%.20q...) = %.20q..., not valid UTF-8", long, got)
	}
}
