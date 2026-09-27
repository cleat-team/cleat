package main

import (
	"strings"
	"testing"
)

func TestParseServiceEndpoints(t *testing.T) {
	t.Run("accepts what the flag documents", func(t *testing.T) {
		got, err := parseServiceEndpoints("billing=https://billing.internal,crm=http://crm.internal:8080/api/")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["billing"] != "https://billing.internal" {
			t.Errorf("billing = %q", got["billing"])
		}
		// The trailing slash is trimmed so forwardToService's `{base}/call/...`
		// does not produce a double slash.
		if got["crm"] != "http://crm.internal:8080/api" {
			t.Errorf("crm = %q", got["crm"])
		}
	})

	t.Run("empty is empty, not an error", func(t *testing.T) {
		got, err := parseServiceEndpoints("")
		if err != nil || len(got) != 0 {
			t.Errorf("got %v, %v; want empty map and no error", got, err)
		}
	})

	// Each of these is a configuration mistake that must stop the worker rather
	// than wait for a workflow to reach the service. A case that returned nil
	// here would defer the failure to a call site days later, where it reads as
	// a failed DurableCall rather than a bad flag.
	for _, tc := range []struct{ name, in, wants string }{
		{"no equals", "billing", "not name=url"},
		{"empty url", "billing=", "empty name or url"},
		{"empty name", "=https://x", "empty name or url"},
		{"service.operation", "billing.charge=https://x", "contains a dot"},
		{"duplicate", "b=https://x,b=https://y", "registered twice"},
		{"bad scheme", "b=ftp://x", "must be http or https"},
		{"no scheme", "b=billing.internal", "must be http or https"},
		{"no host", "b=https://", "no host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseServiceEndpoints(tc.in)
			if err == nil {
				t.Fatalf("parseServiceEndpoints(%q) returned no error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not mention %q, so it does not say what to fix", err, tc.wants)
			}
		})
	}
}
