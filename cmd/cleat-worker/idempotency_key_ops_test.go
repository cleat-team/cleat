package main

import (
	"strings"
	"testing"
)

func TestParseIdempotencyKeyOps(t *testing.T) {
	t.Run("accepts what the flag documents", func(t *testing.T) {
		got, err := parseIdempotencyKeyOps("payment.charge,shipping.dispatch")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got["payment.charge"] {
			t.Error(`got["payment.charge"] missing`)
		}
		if !got["shipping.dispatch"] {
			t.Error(`got["shipping.dispatch"] missing`)
		}
		if len(got) != 2 {
			t.Errorf("len(got) = %d, want 2", len(got))
		}
	})

	t.Run("empty is empty, not an error", func(t *testing.T) {
		got, err := parseIdempotencyKeyOps("")
		if err != nil || len(got) != 0 {
			t.Errorf("got %v, %v; want empty map and no error", got, err)
		}
	})

	for _, tc := range []struct{ name, in, wants string }{
		{"no dot", "payment", "not service.operation"},
		{"empty op", "payment.", "not service.operation"},
		{"empty service", ".charge", "not service.operation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseIdempotencyKeyOps(tc.in)
			if err == nil {
				t.Fatalf("parseIdempotencyKeyOps(%q) returned no error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not mention %q, so it does not say what to fix", err, tc.wants)
			}
		})
	}
}

// TestValidateIdempotencyKeyOps is cleat#2897's startup-refusal case, same
// shape as TestValidateAmbiguityLookupOps: an operation declared for this
// mechanism that is not --write-ahead-intent-ops would never leave a pending
// row to resolve, and one declared for BOTH mechanisms at once would make
// --ambiguity-lookup's configuration for it silently unreachable (this
// mechanism is tried first). Both are configuration mistakes the worker must
// refuse to start with, not discover days later against a real ambiguity.
func TestValidateIdempotencyKeyOps(t *testing.T) {
	t.Run("declared op passes", func(t *testing.T) {
		ops, err := parseIdempotencyKeyOps("payment.charge")
		if err != nil {
			t.Fatalf("parseIdempotencyKeyOps: %v", err)
		}
		if err := validateIdempotencyKeyOps(ops, nil, []string{"payment.charge"}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("undeclared op is refused", func(t *testing.T) {
		ops, err := parseIdempotencyKeyOps("payment.charge")
		if err != nil {
			t.Fatalf("parseIdempotencyKeyOps: %v", err)
		}
		err = validateIdempotencyKeyOps(ops, nil, nil)
		if err == nil {
			t.Fatal("an idempotency-key op outside --write-ahead-intent-ops was accepted")
		}
		if !strings.Contains(err.Error(), "payment.charge") || !strings.Contains(err.Error(), "write-ahead-intent-ops") {
			t.Errorf("error %q does not name the operation or the flag that must declare it", err)
		}
	})

	t.Run("undeclared op is refused even when other ops ARE declared", func(t *testing.T) {
		ops, err := parseIdempotencyKeyOps("payment.charge")
		if err != nil {
			t.Fatalf("parseIdempotencyKeyOps: %v", err)
		}
		if err := validateIdempotencyKeyOps(ops, nil, []string{"shipping.dispatch"}); err == nil {
			t.Fatal("validation passed with payment.charge undeclared, because an unrelated op was declared")
		}
	})

	t.Run("an op declared for both mechanisms is refused", func(t *testing.T) {
		ops, err := parseIdempotencyKeyOps("payment.charge")
		if err != nil {
			t.Fatalf("parseIdempotencyKeyOps: %v", err)
		}
		lookups, err := parseAmbiguityLookup("payment.charge=payment.get_by_key")
		if err != nil {
			t.Fatalf("parseAmbiguityLookup: %v", err)
		}
		err = validateIdempotencyKeyOps(ops, lookups, []string{"payment.charge"})
		if err == nil {
			t.Fatal("an op declared under both --idempotency-key-ops and --ambiguity-lookup was accepted")
		}
		if !strings.Contains(err.Error(), "payment.charge") {
			t.Errorf("error %q does not name the conflicting operation", err)
		}
	})

	t.Run("two DIFFERENT ops, one per mechanism, both pass", func(t *testing.T) {
		ops, err := parseIdempotencyKeyOps("payment.charge")
		if err != nil {
			t.Fatalf("parseIdempotencyKeyOps: %v", err)
		}
		lookups, err := parseAmbiguityLookup("shipping.dispatch=shipping.get_by_key")
		if err != nil {
			t.Fatalf("parseAmbiguityLookup: %v", err)
		}
		if err := validateIdempotencyKeyOps(ops, lookups, []string{"payment.charge", "shipping.dispatch"}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
