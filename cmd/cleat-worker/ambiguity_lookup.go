package main

import (
	"fmt"
	"strings"
)

// ambiguityLookupEntry is one configured service.operation -> service.lookupOperation
// mapping.
type ambiguityLookupEntry struct {
	Service         string
	Operation       string
	LookupOperation string
}

// key returns the "service.operation" string the engine's AmbiguityResolver
// is asked about -- the same form parseWriteAheadIntentOps and
// engine.WithWriteAheadIntentOps use, so the two sets can be compared
// directly.
func (e ambiguityLookupEntry) key() string {
	return e.Service + "." + e.Operation
}

// parseAmbiguityLookup turns --ambiguity-lookup into a set of lookup
// mappings, keyed by "service.operation".
//
// The format is `service.operation=service.lookup_operation`, comma
// separated, matching --service-endpoints' `name=url` shape:
//
//	--ambiguity-lookup "payment.charge=payment.get_by_key,shipping.dispatch=shipping.get_by_key"
//
// SAME SERVICE ON BOTH SIDES, ENFORCED HERE. The design (cleat#1984) asks the
// lookup on the SAME service as the original call, under the SAME
// idempotency key -- a lookup operation on a different service would not
// have the key to look up, and accepting one would silently build a
// mechanism that always answers "cannot say" rather than refusing the
// configuration that can never work.
//
// EVERY ERROR HERE IS FATAL AT BOOT, matching parseServiceEndpoints: a
// malformed or self-contradictory mapping is a configuration mistake, and
// the moment to report one is startup, not the first ambiguity a crash
// produces days later.
func parseAmbiguityLookup(v string) (map[string]ambiguityLookupEntry, error) {
	out := map[string]ambiguityLookupEntry{}
	for _, entry := range splitCommaList(v) {
		lhs, rhs, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("ambiguity lookup %q is not service.operation=service.lookup_operation", entry)
		}
		lhs = strings.TrimSpace(lhs)
		rhs = strings.TrimSpace(rhs)
		svc, op, ok := strings.Cut(lhs, ".")
		if !ok || svc == "" || op == "" {
			return nil, fmt.Errorf("ambiguity lookup %q: left side %q is not service.operation", entry, lhs)
		}
		lookupSvc, lookupOp, ok := strings.Cut(rhs, ".")
		if !ok || lookupSvc == "" || lookupOp == "" {
			return nil, fmt.Errorf("ambiguity lookup %q: right side %q is not service.lookup_operation", entry, rhs)
		}
		if lookupSvc != svc {
			return nil, fmt.Errorf("ambiguity lookup %q: lookup operation is on service %q, "+
				"but the call is on service %q -- a lookup must be on the SAME service, "+
				"under the same idempotency key", entry, lookupSvc, svc)
		}
		e := ambiguityLookupEntry{Service: svc, Operation: op, LookupOperation: lookupOp}
		if _, dup := out[e.key()]; dup {
			return nil, fmt.Errorf("ambiguity lookup for %q is registered twice", e.key())
		}
		out[e.key()] = e
	}
	return out, nil
}

// validateAmbiguityLookupOps refuses a lookup configured for an operation
// that is not declared --write-ahead-intent-ops.
//
// WriteAheadIntentOps is the only semantics that ever records a pending
// intent (engine/callintent.go's callSemantics): an AtLeastOnce operation
// never leaves a row for a resolver to be asked about, so a lookup
// configured for one is a mechanism wired to nothing -- it would pass
// validation, compile, run, and never once be consulted. Refusing at boot
// turns that into a configuration error instead of a silent no-op an
// operator discovers by its absence. cleat#1984, and the shape §4 of the
// prior session's "A MECHANISM THAT EXISTS AND IS WIRED TO NOTHING READS AS
// DONE" class of defect exists to prevent.
func validateAmbiguityLookupOps(lookups map[string]ambiguityLookupEntry, writeAheadOps []string) error {
	declared := make(map[string]bool, len(writeAheadOps))
	for _, op := range writeAheadOps {
		declared[op] = true
	}
	for key, e := range lookups {
		if !declared[key] {
			return fmt.Errorf("--ambiguity-lookup declares a lookup for %q, "+
				"which is not in --write-ahead-intent-ops: only a write-ahead-intent "+
				"operation can ever leave a pending call for a resolver to be asked about",
				e.key())
		}
	}
	return nil
}
