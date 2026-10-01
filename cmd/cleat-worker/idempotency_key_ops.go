package main

import (
	"fmt"
	"strings"
)

// parseIdempotencyKeyOps turns --idempotency-key-ops into a set of declared
// "service.operation" keys. Unlike --ambiguity-lookup, there is no second
// operation to name: the mechanism re-dispatches the SAME operation under
// its original key, so the flag is a plain comma-separated list.
//
//	--idempotency-key-ops "payment.charge,shipping.dispatch"
//
// EVERY ERROR HERE IS FATAL AT BOOT, matching parseAmbiguityLookup: a
// malformed entry is a configuration mistake, and the moment to report one
// is startup, not the first ambiguity a crash produces days later.
func parseIdempotencyKeyOps(v string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, entry := range splitCommaList(v) {
		svc, op, ok := strings.Cut(entry, ".")
		if !ok || svc == "" || op == "" {
			return nil, fmt.Errorf("idempotency-key op %q is not service.operation", entry)
		}
		out[svc+"."+op] = true
	}
	return out, nil
}

// validateIdempotencyKeyOps refuses an operation declared --idempotency-key-ops
// that is not also --write-ahead-intent-ops (same reasoning as
// validateAmbiguityLookupOps: AtLeastOnce never leaves a pending intent row,
// so a declaration for one would be wired to nothing), and refuses an
// operation declared under BOTH --idempotency-key-ops and --ambiguity-lookup.
//
// The second check exists because the two mechanisms answer the same
// question -- "what happened to this ambiguous call" -- through different
// means for different service capabilities, and nothing in the engine
// decides a precedence between them if both were configured for one
// operation: resolveAmbiguityViaKeyReplay is simply tried first, which would
// make --ambiguity-lookup's configuration for that operation silently
// unreachable. Refusing it at boot turns a config mistake that would read as
// "the lookup is never consulted" into an error that names the conflict.
func validateIdempotencyKeyOps(idempotencyOps map[string]bool, lookups map[string]ambiguityLookupEntry, writeAheadOps []string) error {
	declared := make(map[string]bool, len(writeAheadOps))
	for _, op := range writeAheadOps {
		declared[op] = true
	}
	for key := range idempotencyOps {
		if !declared[key] {
			return fmt.Errorf("--idempotency-key-ops declares %q, "+
				"which is not in --write-ahead-intent-ops: only a write-ahead-intent "+
				"operation can ever leave a pending call for this mechanism to resolve", key)
		}
		if _, dup := lookups[key]; dup {
			return fmt.Errorf("%q is declared in both --idempotency-key-ops and --ambiguity-lookup: "+
				"these are different capabilities a service either has or does not -- "+
				"choose one for this operation", key)
		}
	}
	return nil
}
