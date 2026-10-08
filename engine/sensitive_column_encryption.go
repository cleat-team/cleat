package engine

import (
	"context"
	"fmt"
)

// This file is event_storage_encoding.go's sibling for columns OUTSIDE
// event_history. cleat#2312: with --encrypt-sensitive-payloads on,
// event_history's ten string columns and payload are sealed
// (encodeEventForStorage), but workflow_instances.{input,result,error_msg,
// error_code,error_op,cancellation_reason,query_state},
// workflow_signals.payload, workflow_promises.{result,error_msg},
// workflow_update_requests.{payload,result,error_msg},
// workflow_schedules.input and idempotency_keys.error_msg were all stored
// in plaintext regardless of the flag -- the docs said "workflow input,
// output and error [are] encrypted at rest", which overstated what the
// first encryption pass actually covered. The owner ruled (3A, 2026-09-25)
// that these should also be encrypted, as a "should for after 0.3.0"; that
// condition passed when 0.4.0 shipped 2026-10-07.
//
// ONE WIRE FORMAT PER COLUMN SHAPE, reusing the two that already exist
// rather than inventing a third:
//   - a jsonb column holds EncryptJSON's output, a JSON string literal
//     wrapping base64 ciphertext -- exactly what encodePayloadForStorageWith
//     already produces for event_history.payload, so this reuses it rather
//     than duplicating it.
//   - a text column holds bare base64 of the sealed bytes -- exactly what
//     sealString already produces for event_history's ten string columns.
//
// See engine/testutil or the live test in
// sensitive_column_encryption_live_test.go for how this is verified against
// a real Postgres instance: start a workflow through the real store, read
// the row back on a connection that never decrypts, and confirm ciphertext.

// tenantCipherForWrite derives this store's tenant cipher for a write that
// may need to seal zero or more sensitive columns outside event_history, or
// nil when encryption is off. One derivation per call site rather than per
// field, same reasoning as encodeEventForStorage's "one derivation for the
// whole event" -- a write touching workflow_instances often seals two or
// three columns at once (result AND query_state, say), and deriving per
// field would double that work for no benefit, since every field in one
// call belongs to the same tenant.
func (s *PostgresStore) tenantCipherForWrite() (*tenantCipher, error) {
	if s.encryption == nil || !s.encryptSensitivePayloads {
		return nil, nil
	}
	return s.encryption.forTenant(tenantForAAD(s.tenantID))
}

// encryptJSONColumnForStorage seals a non-empty JSON value for a jsonb
// column outside event_history. tc is nil when encryption is off, and nil
// passes the value through unchanged -- encodePayloadForStorageWith's own
// convention, reused rather than duplicated.
//
// Every caller of this function already has a non-empty, valid-JSON string
// by construction: coerceResultJSON and marshalQueryState both default to
// "{}" rather than "", and the jsonb columns this seals
// (workflow_instances.input, workflow_signals.payload,
// workflow_schedules.input, workflow_update_requests.payload) are all
// `NOT NULL DEFAULT '{}'::jsonb` at the schema level. So this carries none
// of encodePayloadForStorage's empty-string special case; a caller with a
// Go string that could genuinely be "" must coerce it to valid JSON first,
// the way CompleteWorkflow already does for result before this is called.
func encryptJSONColumnForStorage(value string, tc *tenantCipher) (string, error) {
	out, err := encodePayloadForStorageWith(value, tc)
	if err != nil {
		return "", err
	}
	return out.String, nil
}

// encryptTextColumnForStorage seals a single TEXT column's plaintext using
// the same wire format event_history's string columns use: bare base64 of
// the sealed bytes, no JSON wrapping. Empty values are returned unchanged --
// sealString always returns at least a nonce and a GCM tag, so it cannot
// produce "", which is what lets the read side treat a stored "" as "never
// encrypted" rather than as a decryption failure (the same reasoning
// decryptField's own doc comment gives for event_history, cleat#1377).
func encryptTextColumnForStorage(value string, tc *tenantCipher) (string, error) {
	if value == "" || tc == nil {
		return value, nil
	}
	return tc.sealString(value)
}

// decryptTextColumnFromStorage is decryptPayloadJSON's (db.go) sibling for a
// TEXT column instead of a jsonb one: bare base64 ciphertext (DecryptString)
// rather than a JSON string literal (DecryptJSON). Same behaviour, restated
// for the different wire shape:
//
//   - encryption off, or the stored value is empty: pass through.
//   - the value opens: return the plaintext.
//   - the value does not open AND is not sealed-shaped: it predates
//     encryption (or was written by a path that does not encrypt yet), so
//     it is returned unchanged with no error -- the same "mixed history is
//     not a failure" rule decryptPayloadJSON's own doc comment states.
//   - the value does not open and IS sealed-shaped: a genuine decryption
//     failure. Logged, counted, and returned as the original (ciphertext)
//     value wrapped in ErrPayloadDecryption -- matching decryptPayloadJSON's
//     contract rather than decryptField's "[DECRYPTION_FAILED]" placeholder,
//     because every caller of this function is already outside
//     event_history's per-step record and decryptPayloadJSON is this
//     function's only precedent for that case.
//
// fieldName is for the log line and the wrapped error only.
func (s *PostgresStore) decryptTextColumnFromStorage(value, fieldName string) (string, error) {
	if s.encryption == nil || !s.encryptSensitivePayloads || value == "" {
		return value, nil
	}
	decrypted, err := s.encryption.DecryptString(tenantForAAD(s.tenantID), value)
	if err == nil {
		return decrypted, nil
	}
	if !sealedShape(value, false) {
		return value, nil
	}
	if !s.quietDecryptLogs {
		s.log().WarnContext(context.Background(), "decrypt text column failed", "field", fieldName, "error", err)
	}
	if s.Metrics != nil {
		s.Metrics.RecordDecryptionError(context.Background())
	}
	return value, fmt.Errorf("%w: %s column: %v", ErrPayloadDecryption, fieldName, err)
}
