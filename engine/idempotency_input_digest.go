package engine

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// IdempotencyInputDigest is the fingerprint an idempotency key is compared on,
// alongside the workflow definition.
//
// cleat#1170: a second request presenting a key that is already held was handed
// the first request's workflow id with alreadyExisted = true, and its own input
// was discarded without a word. The caller is told the work is under way and
// never learns it is under way with somebody else's arguments -- worse than a
// duplicate execution, which at least leaves a row behind.
//
// CANONICALISED, NOT HASHED RAW. Two requests carrying the same object with
// different key order or whitespace are the same request, and digesting the
// bytes would refuse the second. json.Marshal over the decoded value sorts map
// keys, so the digest is a property of the VALUE. Input that is not valid JSON
// is digested as-is: there is nothing to canonicalise, and a byte comparison is
// the only honest answer available.
//
// WHAT THIS DIGEST CANNOT SEE. On the HTTP path the input has already been
// through engine.Redact by the time any store sees it, so a sensitive field
// arrives as "[REDACTED]" and two requests differing ONLY in such a field
// digest equal and replay. That residue is stated rather than hidden: closing
// it means digesting before redaction, which means computing it in the handler
// and passing it down. See cleat#1170.
func IdempotencyInputDigest(input json.RawMessage) string {
	b := []byte(input)
	var v any
	if err := json.Unmarshal(b, &v); err == nil {
		if canon, err := json.Marshal(v); err == nil {
			b = canon
		}
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// checkIdempotencyInput refuses a key hit whose stored input digest disagrees
// with this request's.
//
// A NULL stored digest is UNKNOWN, not mismatched: the row predates this column
// and there is nothing to compare against. It is allowed through, which degrades
// to the behaviour before cleat#1170 rather than to a refusal a caller cannot
// act on -- the same rule cleat#1047 set for a NULL def_name, and the reason
// `*_a_duplicate_key_with_a_different_payload_is_refused.sql` backfills nothing
// -- cited by NAME because the three dialects carry it at three different
// numbers (this one moved twice in review as other branches took 058 and 059).
// A digest computed by SQL would be a second
// derivation of the same value, free to disagree with the Go one for a whole
// upgrade before anybody noticed.
func checkIdempotencyInput(stored sql.NullString, want string) error {
	if !stored.Valid || stored.String == "" || stored.String == want {
		return nil
	}
	return fmt.Errorf("%w: the key is held by a run started with a different input "+
		"(stored digest %s, this request %s)", ErrIdempotencyKeyInputMismatch,
		shortDigest(stored.String), shortDigest(want))
}

// shortDigest keeps an error message readable. The full value is in the row.
func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
