package main

import (
	"strings"
	"testing"
)

func TestParseAmbiguityLookup(t *testing.T) {
	t.Run("accepts what the flag documents", func(t *testing.T) {
		got, err := parseAmbiguityLookup("payment.charge=payment.get_by_key,shipping.dispatch=shipping.get_by_key")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		e, ok := got["payment.charge"]
		if !ok {
			t.Fatal(`got["payment.charge"] missing`)
		}
		if e.Service != "payment" || e.Operation != "charge" || e.LookupOperation != "get_by_key" {
			t.Errorf("payment.charge entry = %+v", e)
		}
		if _, ok := got["shipping.dispatch"]; !ok {
			t.Fatal(`got["shipping.dispatch"] missing`)
		}
	})

	t.Run("empty is empty, not an error", func(t *testing.T) {
		got, err := parseAmbiguityLookup("")
		if err != nil || len(got) != 0 {
			t.Errorf("got %v, %v; want empty map and no error", got, err)
		}
	})

	// Each of these is a configuration mistake that must stop the worker
	// rather than wait for a crash's ambiguity to reach a misconfigured
	// resolver, days later, reading as "cannot say" with no clue why.
	for _, tc := range []struct{ name, in, wants string }{
		{"no equals", "payment.charge", "not service.operation=service.lookup_operation"},
		{"left side no dot", "payment=payment.get_by_key", "not service.operation"},
		{"left side empty op", "payment.=payment.get_by_key", "not service.operation"},
		{"right side no dot", "payment.charge=get_by_key", "not service.lookup_operation"},
		{"right side empty op", "payment.charge=payment.", "not service.lookup_operation"},
		{"different service", "payment.charge=billing.get_by_key", "SAME service"},
		{"duplicate", "payment.charge=payment.a,payment.charge=payment.b", "registered twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAmbiguityLookup(tc.in)
			if err == nil {
				t.Fatalf("parseAmbiguityLookup(%q) returned no error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not mention %q, so it does not say what to fix", err, tc.wants)
			}
		})
	}
}

// TestValidateAmbiguityLookupOps is the startup-refusal case the issue's
// acceptance table asks for: a lookup configured for an operation that is
// not declared --write-ahead-intent-ops is a mechanism wired to nothing --
// AtLeastOnce never leaves a pending intent row for a resolver to be asked
// about -- and the worker must refuse to start rather than ship it silently
// unreachable. cleat#1984.
func TestValidateAmbiguityLookupOps(t *testing.T) {
	t.Run("declared op passes", func(t *testing.T) {
		lookups, err := parseAmbiguityLookup("payment.charge=payment.get_by_key")
		if err != nil {
			t.Fatalf("parseAmbiguityLookup: %v", err)
		}
		if err := validateAmbiguityLookupOps(lookups, []string{"payment.charge"}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("undeclared op is refused", func(t *testing.T) {
		lookups, err := parseAmbiguityLookup("payment.charge=payment.get_by_key")
		if err != nil {
			t.Fatalf("parseAmbiguityLookup: %v", err)
		}
		err = validateAmbiguityLookupOps(lookups, nil)
		if err == nil {
			t.Fatal("a lookup for an operation outside --write-ahead-intent-ops was accepted")
		}
		if !strings.Contains(err.Error(), "payment.charge") || !strings.Contains(err.Error(), "write-ahead-intent-ops") {
			t.Errorf("error %q does not name the operation or the flag that must declare it", err)
		}
	})

	t.Run("undeclared op is refused even when other ops ARE declared", func(t *testing.T) {
		lookups, err := parseAmbiguityLookup("payment.charge=payment.get_by_key")
		if err != nil {
			t.Fatalf("parseAmbiguityLookup: %v", err)
		}
		// A DIFFERENT operation is write-ahead -- payment.charge itself is
		// still not, so this must still refuse. A validator that only checks
		// "is the set non-empty" would pass this case wrongly.
		if err := validateAmbiguityLookupOps(lookups, []string{"shipping.dispatch"}); err == nil {
			t.Fatal("validation passed with payment.charge undeclared, because an unrelated op was declared")
		}
	})
}
