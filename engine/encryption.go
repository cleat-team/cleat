// Package host provides the core cleat workflow engine, including encryption
// at rest for sensitive event payloads.
package engine

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// PayloadEncryption provides AES-256-GCM encryption and decryption for
// sensitive event payload fields. The key must be exactly 32 bytes after
// base64 decoding. The wire format is:
//
//	base64(nonce || ciphertext)
//
// where nonce is 12 random bytes and ciphertext includes the 16-byte GCM
// authentication tag.
//
// # Ciphertexts are bound to their tenant (cleat#1776)
//
// Every call takes a tenantID, which is passed as GCM additional authenticated
// data. AAD is not encrypted and adds no bytes; it is covered by the
// authentication tag, so a ciphertext written under tenant A does not open when
// presented in tenant B's row. Before this it was nil, and a blob moved between
// tenants decrypted to the real plaintext.
//
// # The transition needs no envelope, because GCM already discriminates
//
// Rows written before cleat#1776 are sealed with nil AAD, and there is no
// migration that can re-seal them -- a numbered migration has no key, since the
// key is worker configuration. So Decrypt tries AAD = tenantID and falls back
// to AAD = nil, and the authentication tag decides which form it is holding,
// with forgery probability 2^-128. A version prefix would be strictly worse: the
// raw-byte form (Decrypt) has no reserved space, so any magic string can be
// produced by a random nonce, and a false positive makes an OLD row unreadable.
//
// WHAT THAT FALLBACK COSTS, AND IT IS NOT NOTHING. A legacy blob authenticates
// nothing about its tenant, so it can still be substituted across tenants for
// as long as one exists. Binding new writes does not retroactively bind old
// ones, and eliminating them is a re-seal, not a flag.
type PayloadEncryption struct {
	key []byte
}

// tenantForAAD resolves the tenant a row's ciphertext must be bound to.
//
// AN EMPTY TENANT IS NOT "NO TENANT", and getting this wrong breaks the read
// side rather than the write side. The untenanted write path -- an Engine with
// no tenantID (engine/flush.go:433) -- inserts without setting RLS, so the row
// takes the tenant_id column DEFAULT, which is this constant on all three
// dialects. A store reading that row defaults its own tenantID to the same
// value (engine/db.go:78). So both ends already agree on who owns the row, and
// this function is the one place that says so, rather than two `if == ""`
// branches that can drift apart.
//
// Binding to "" would be worse than wrong: a zero-length AAD is byte-identical
// to a nil one, so the ciphertext would be sealed UNBOUND while every test of
// the form "the wrong tenant fails" carried on passing.
func tenantForAAD(tenantID string) string {
	if tenantID == "" {
		return DefaultTenantUUID
	}
	return tenantID
}

// ErrNoTenantForEncryption is returned when a caller seals with an empty tenant.
//
// REFUSING IS NOT PEDANTRY, IT IS THE ONLY WAY THIS FIX CANNOT SILENTLY NOT
// HAPPEN. Measured: GCM treats a zero-length AAD and a nil AAD identically --
// Seal(..., []byte("")) and Seal(..., nil) produce BYTE-IDENTICAL output, and a
// blob sealed with one opens with the other. So a caller that passes "" writes a
// blob indistinguishable from a legacy one, the fallback below opens it happily,
// and every test of the form "the wrong tenant fails" still passes, because the
// blob was never bound to anything. There is no observable difference to catch
// it later; the only place to catch it is here.
var ErrNoTenantForEncryption = errors.New("payload encryption: refusing to seal with an empty tenant id: a zero-length AAD is identical to nil, so the ciphertext would not be bound to any tenant")

// NewPayloadEncryption creates a PayloadEncryption from a base64-encoded
// key string. The decoded key must be exactly 32 bytes (AES-256).
func NewPayloadEncryption(keyBase64 string) (*PayloadEncryption, error) {
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return nil, fmt.Errorf("payload encryption: decode key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("payload encryption: key must be exactly 32 bytes after base64 decode, got %d", len(key))
	}
	return &PayloadEncryption{key: key}, nil
}

// Encrypt seals plaintext under the tenant, returning nonce || ciphertext.
//
// tenantID becomes the GCM additional authenticated data, so the result opens
// only when the same tenant is presented on read. An empty tenantID is refused
// rather than sealed unbound -- see ErrNoTenantForEncryption for why that has to
// be an error and not a warning.
//
// This method took no tenant before cleat#1776, which is the defect: the note
// that used to be here said "reserved for future work; currently AAD is nil",
// and it was accurate for long enough to be quoted in the bug report. The
// parameter is required rather than added as a sibling method on purpose -- a
// surviving Encrypt(plaintext) is an unbound write path that outlives the fix,
// and the compiler finding every caller is the point.
func (pe *PayloadEncryption) Encrypt(tenantID string, plaintext []byte) ([]byte, error) {
	if tenantID == "" {
		return nil, ErrNoTenantForEncryption
	}
	block, err := aes.NewCipher(pe.key)
	if err != nil {
		return nil, fmt.Errorf("encrypt: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encrypt: new GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("encrypt: nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, plaintext, []byte(tenantID))
	return ciphertext, nil
}

// Decrypt opens data produced by Encrypt (nonce || ciphertext) for one tenant.
//
// It tries AAD = tenantID first, then AAD = nil. The second attempt is the only
// thing keeping rows written before cleat#1776 readable, and it is also the
// residual hole: such a row is not bound to any tenant and never will be until
// it is re-sealed. Distinguishing the two forms needs an open with no fallback,
// which nothing in production asks for yet -- the re-seal that will ask for it
// can export one then. The tests do it directly, in
// payload_ciphertext_is_bound_to_its_tenant_test.go.
//
// The order matters and is not arbitrary: a tenant-bound blob does NOT open with
// nil AAD (measured), so trying the bound form first can never mistake a new
// blob for a legacy one, and the fallback is reached only by blobs that really
// are unbound.
func (pe *PayloadEncryption) Decrypt(tenantID string, data []byte) ([]byte, error) {
	plaintext, err := pe.open(data, []byte(tenantID))
	if err == nil {
		return plaintext, nil
	}
	// Legacy: sealed before cleat#1776, with nil AAD.
	legacy, lerr := pe.open(data, nil)
	if lerr == nil {
		return legacy, nil
	}
	// Report the BOUND attempt's error, not the fallback's. Both say
	// "message authentication failed" for a wrong tenant, and the first is the
	// one describing what the caller asked for.
	return nil, err
}

// open is the shared GCM open, parameterised by AAD so that the two callers
// above differ in exactly one argument and nothing else.
func (pe *PayloadEncryption) open(data, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(pe.key)
	if err != nil {
		return nil, fmt.Errorf("decrypt: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("decrypt: new GCM: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return nil, fmt.Errorf("decrypt: ciphertext too short (len=%d, need at least %d)", len(data), nonceSize)
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("decrypt: open: %w", err)
	}
	return plaintext, nil
}

// EncryptString seals a plaintext string for one tenant, base64-encoded.
func (pe *PayloadEncryption) EncryptString(tenantID, plaintext string) (string, error) {
	ciphertext, err := pe.Encrypt(tenantID, []byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// DecryptString opens a base64-encoded ciphertext string for one tenant.
func (pe *PayloadEncryption) DecryptString(tenantID, encoded string) (string, error) {
	plaintext, err := pe.DecryptBase64(tenantID, encoded)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// DecryptBase64 base64-decodes the input then opens it for one tenant.
func (pe *PayloadEncryption) DecryptBase64(tenantID, encoded string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decrypt base64: decode: %w", err)
	}
	return pe.Decrypt(tenantID, data)
}

// EncryptJSON encrypts JSON bytes and returns them as a JSON string literal
// (i.e., a quoted base64 string that is valid JSON). This allows encrypted
// payloads to be stored in JSONB columns.
func (pe *PayloadEncryption) EncryptJSON(tenantID string, jsonBytes []byte) ([]byte, error) {
	ciphertext, err := pe.Encrypt(tenantID, jsonBytes)
	if err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(ciphertext)
	// Wrap in quotes to form a JSON string literal.
	result := make([]byte, 0, len(encoded)+2)
	result = append(result, '"')
	result = append(result, []byte(encoded)...)
	result = append(result, '"')
	return result, nil
}

// DecryptJSON parses a JSON string literal containing base64-encoded
// ciphertext and decrypts it, returning the original JSON bytes.
func (pe *PayloadEncryption) DecryptJSON(tenantID string, jsonValue []byte) ([]byte, error) {
	// Expect a JSON string literal: "<base64>"
	if len(jsonValue) < 2 || jsonValue[0] != '"' || jsonValue[len(jsonValue)-1] != '"' {
		return nil, fmt.Errorf("decrypt JSON: value is not a JSON string literal")
	}
	encoded := string(jsonValue[1 : len(jsonValue)-1])
	return pe.DecryptBase64(tenantID, encoded)
}

// recordEncryptionFailure counts a failed encrypt-on-write (cleat#1317).
//
// A method on the store because the encode path is free functions taking a
// *PayloadEncryption, with no receiver and so no Metrics -- and because the
// nil check belongs in one place rather than at each of the three call sites.
//
// Its decryption twin has been counted since db.go:176; this side simply never
// was. The difference in SHAPE is deliberate and worth keeping: a failed
// decrypt is swallowed and counted, because a row that will not decrypt should
// not stop a read; a failed encrypt is counted AND returned, because writing
// the plaintext instead is the outcome encryption exists to prevent.
func (s *PostgresStore) recordEncryptionFailure(ctx context.Context) {
	if s.Metrics == nil {
		return
	}
	s.Metrics.RecordEncryptionError(ctx)
}
