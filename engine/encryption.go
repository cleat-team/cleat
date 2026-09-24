// Package host provides the core cleat workflow engine, including encryption
// at rest for sensitive event payloads.
package engine

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
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
//
// # Rotation (cleat#1992) reuses engine.KeyRing rather than a bare key
//
// The single key became a ring: one current key that seals every new write,
// and any number of previous keys that are read-only. See KeyRing's own doc
// comment (keyring.go) -- it was built generic for exactly this reuse. UNLIKE
// tenant secrets, no column on event_history carries a key_version: the GCM
// tag already discriminates which key opened a value (the three PayloadForm
// forms below), so no schema change was needed. A previous key's Version is
// therefore bookkeeping for KeyRing's own dedup checks, never read back from a
// row -- see NewPayloadEncryptionWithRing.
type PayloadEncryption struct {
	ring *KeyRing
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

// payloadKeyInfo separates this subsystem's derived keys from every other one
// that shares the master secret.
//
// A DIFFERENT string from tenant_secrets.go's "cleat-tenant-secret-v1", which is
// the point: with the same master key and the same tenant id, a different info
// string yields a different derived key, so a payload key cannot open a tenant
// secret or the reverse even if the two subsystems are ever pointed at one
// master.
const payloadKeyInfo = "cleat-payload-v1"

// tenantCipher holds one tenant's keys for the duration of one operation.
//
// WHY THIS EXISTS AT ALL, AND IT IS A MEASUREMENT RATHER THAN A PREFERENCE.
// Deriving a key costs about what a GCM seal costs -- 530ns against 540ns,
// measured at -benchtime 2s -count=5 -- a shorter benchtime is noise, see
// engine/payload_key_derivation_bench_test.go. encodeEventForStorage seals up to
// eleven fields for one event, so deriving per FIELD adds a seal's worth of work
// per field and doubles the crypto on the write path:
//
//	11 seals, no derivation (before this)   5997 ns/op   1.00x
//	11 seals, derive once per event         6537 ns/op   1.09x
//	11 seals, derive once per field        12261 ns/op   2.04x
//
// So the derived key is computed once per event and carried, rather than
// recomputed per field or cached across events.
//
// NOT A CACHE, deliberately. A per-tenant key cache would hold derived keys in
// memory indefinitely, which is the property tenant_secrets.go avoids by
// deriving per call, and it would need eviction and a mutex on a hot path. This
// holds a key for exactly one event's encoding or one row's decoding.
//
// tenant_secrets.go:112 says HKDF is "cheap enough to do per call rather than
// cache". That is true at ITS call rate -- one secret at a time -- and would
// have been false here, where "per call" means eleven times per event. The
// sentence did not transfer; the measurement is why.
type tenantCipher struct {
	tenantID string
	derived  []byte   // HKDF(current key, tenantID, payloadKeyInfo)
	master   []byte   // current key, kept for the two legacy forms below
	ring     *KeyRing // for trying previous keys on read; nil write paths never touch it
}

// forTenant derives this tenant's payload key, under the ring's CURRENT key.
//
// PER TENANT, so that a key recovered from one tenant's ciphertext -- by
// cryptanalysis, by a bug, by a disclosed plaintext -- does not decrypt
// another's. The AAD binding from cleat#1776 stops a ciphertext being MOVED
// between tenants; this stops one recovered key reading all of them. They are
// different properties and this subsystem now has both, which is what
// tenant_secrets.go has had since it was written.
func (pe *PayloadEncryption) forTenant(tenantID string) (*tenantCipher, error) {
	if tenantID == "" {
		return nil, ErrNoTenantForEncryption
	}
	// A 32-byte current key is an invariant NewPayloadEncryption and
	// NewPayloadEncryptionWithRing both enforce, and it has to be re-checked
	// HERE because the derivation silently tolerates a bad one.
	//
	// MEASURED, AND IT WAS A REGRESSION THIS CHANGE INTRODUCED. Before
	// cleat#1793 a zero-value PayloadEncryption failed at aes.NewCipher(nil) on
	// the first field, which is how TestAdaptiveFlusher_PrepareEntry_EncryptionError
	// and TestFlushEvent_EncryptGeneralFailure force the encrypt-error branch.
	// HKDF has no such objection -- a nil secret is just HMAC with an empty key
	// -- so derivation produced a perfectly valid 32-byte key from nothing and
	// the seal succeeded. A misconfigured encryptor would have written
	// ciphertext keyed off an empty master and reported success. Those two tests
	// caught it; without them the loud failure would have become a silent one.
	//
	// A nil ring reads the same as a zero-value key (len 0) -- pe.currentKey
	// handles both a nil *PayloadEncryption's ring and a ring with no current
	// key configured, so this one check covers "never configured" and "half
	// configured" identically.
	cur, curLen := pe.currentKey()
	if curLen != 32 {
		return nil, fmt.Errorf("payload encryption: master key is %d bytes, want 32", curLen)
	}
	derived := make([]byte, 32)
	r := hkdf.New(sha256.New, cur, []byte(tenantID), []byte(payloadKeyInfo))
	if _, err := io.ReadFull(r, derived); err != nil {
		return nil, fmt.Errorf("payload encryption: derive tenant key: %w", err)
	}
	return &tenantCipher{tenantID: tenantID, derived: derived, master: cur, ring: pe.ring}, nil
}

// currentKey returns the ring's current key and its length, or (nil, 0) if no
// ring is configured at all -- kept as one function so every caller that needs
// "is this thing configured" asks it the same way pe.ring == nil would answer,
// without repeating the nil check.
func (pe *PayloadEncryption) currentKey() ([]byte, int) {
	if pe == nil || pe.ring == nil {
		return nil, 0
	}
	cur := pe.ring.Current()
	return cur.Key, len(cur.Key)
}

// seal always writes the newest form.
func (tc *tenantCipher) seal(plaintext []byte) ([]byte, error) {
	return sealGCM(tc.derived, plaintext, []byte(tc.tenantID))
}

// sealString and sealJSON mirror EncryptString and EncryptJSON, minus the
// derivation -- the caller has already paid for it once.
//
// These exist so the multi-field write path can derive once per EVENT. Without
// them encodeEventForStorage would call EncryptString eleven times and derive
// eleven keys, which roughly doubles the crypto cost -- see the table above.
// See tenantCipher's doc for the numbers.
func (tc *tenantCipher) sealString(plaintext string) (string, error) {
	ct, err := tc.seal([]byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (tc *tenantCipher) sealJSON(jsonBytes []byte) ([]byte, error) {
	ct, err := tc.seal(jsonBytes)
	if err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(ct)
	out := make([]byte, 0, len(encoded)+2)
	out = append(out, '"')
	out = append(out, encoded...)
	out = append(out, '"')
	return out, nil
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
//
// Treats the key as key ring version 1, unconditionally -- the same rule
// NewSecretStore uses for tenant secrets' single-key constructor, and for the
// same reason: every pre-rotation deployment has exactly one key, and it has
// no other version to be.
func NewPayloadEncryption(keyBase64 string) (*PayloadEncryption, error) {
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return nil, fmt.Errorf("payload encryption: decode key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("payload encryption: key must be exactly 32 bytes after base64 decode, got %d", len(key))
	}
	ring, err := NewKeyRing(VersionedKey{Version: 1, Key: key})
	if err != nil {
		return nil, err
	}
	return &PayloadEncryption{ring: ring}, nil
}

// NewPayloadEncryptionWithRing builds a PayloadEncryption around a full key
// ring: a current key that seals every new write, and any number of previous
// keys that are read-only (cleat#1992). See PayloadEncryption's own doc
// comment for why a previous key's Version is never persisted anywhere.
func NewPayloadEncryptionWithRing(ring *KeyRing) (*PayloadEncryption, error) {
	if ring == nil {
		return nil, fmt.Errorf("payload encryption: a key ring is required")
	}
	return &PayloadEncryption{ring: ring}, nil
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
// One derivation per call, which is correct for a caller sealing ONE value and
// is the slow path if used per field. The multi-field write path does not come
// through here -- encodeEventForStorage takes a tenantCipher once per event and
// seals through it. See tenantCipher's doc for the measurement.
func (pe *PayloadEncryption) Encrypt(tenantID string, plaintext []byte) ([]byte, error) {
	tc, err := pe.forTenant(tenantID)
	if err != nil {
		return nil, err
	}
	return tc.seal(plaintext)
}

// PayloadForm is which of the three stored forms a ciphertext is in.
//
// Replaces the DecryptTenantBound / DecryptLegacyUnbound pair added in
// cleat#1794. Those answered a yes/no question -- "is this bound to its tenant"
// -- which stopped being the question the moment there were two bound forms:
// `cleatctl reseal-payloads` must now rewrite BOTH the nil-AAD form and the
// master-key form, and a boolean cannot tell it which it is holding. One call
// that returns the plaintext AND the form is what the sweep actually needs, and
// it cannot be answered wrongly by omission the way a pair of predicates can.
type PayloadForm int

const (
	// PayloadFormDerived is the current form: per-tenant derived key, tenant
	// id as AAD. Nothing needs doing to these.
	PayloadFormDerived PayloadForm = iota

	// PayloadFormBound is cleat#1792's form: master key, tenant id as AAD. It
	// cannot be moved between tenants, but one recovered key reads every
	// tenant's payloads, so it is still work for the sweep.
	PayloadFormBound

	// PayloadFormLegacy is pre-cleat#1792: master key, nil AAD. Bound to
	// nothing -- it decrypts in any tenant's row.
	PayloadFormLegacy

	// PayloadFormPreviousKey (cleat#1992) opened under one of the ring's
	// PREVIOUS keys rather than its current one -- in whichever of the three
	// (derived, bound, legacy) shapes that previous key was written in. The
	// sweep doesn't need to know which shape: every previous-key value needs
	// re-sealing under the current key regardless, the same as Bound and
	// Legacy do under a single-key ring.
	PayloadFormPreviousKey

	// PayloadFormUnreadable is none of the above under any configured key:
	// plaintext that happens to be valid base64, a row sealed under a key this
	// ring holds neither as current nor previous, or corruption. Reported,
	// never rewritten.
	PayloadFormUnreadable
)

func (f PayloadForm) String() string {
	switch f {
	case PayloadFormDerived:
		return "derived"
	case PayloadFormBound:
		return "bound"
	case PayloadFormLegacy:
		return "legacy"
	case PayloadFormPreviousKey:
		return "previous-key"
	default:
		return "unreadable"
	}
}

// OpenAndClassify opens data and reports which form it was in.
//
// Newest form first, so a fully converted database costs one GCM open per value
// and only unconverted rows pay for the extra attempts. Classification is by
// AUTHENTICATION rather than inspection: each form is a distinct (key, AAD)
// pair, and GCM's tag accepts exactly one of them, at forgery probability
// 2^-128. There is no length rule, no version prefix and no guessing -- which is
// what stops the sweep double-encrypting a plaintext column or rewriting a row
// it has already done.
func (pe *PayloadEncryption) OpenAndClassify(tenantID string, data []byte) ([]byte, PayloadForm, error) {
	tc, err := pe.forTenant(tenantID)
	if err != nil {
		return nil, PayloadFormUnreadable, err
	}
	return tc.openClassify(data)
}

// openClassify is the three attempts under the CURRENT key, then (cleat#1992)
// the same three attempts under each PREVIOUS key in the ring, in one place.
//
// Both entry points come through here -- OpenAndClassify for the sweep, which
// needs the form, and open for the read paths, which do not. One implementation
// because the ORDER is the contract: current key first and newest form first
// within it, so a fully converted row costs one GCM open and only a row still
// under an old key or an old form pays for the extra attempts.
func (tc *tenantCipher) openClassify(data []byte) ([]byte, PayloadForm, error) {
	if pt, err := openGCM(tc.derived, data, []byte(tc.tenantID)); err == nil {
		return pt, PayloadFormDerived, nil
	}
	if pt, err := openGCM(tc.master, data, []byte(tc.tenantID)); err == nil {
		return pt, PayloadFormBound, nil
	}
	pt, err := openGCM(tc.master, data, nil)
	if err == nil {
		return pt, PayloadFormLegacy, nil
	}
	if pt, perr := tc.openUnderPreviousKeys(data); perr == nil {
		return pt, PayloadFormPreviousKey, nil
	}
	// Report the newest attempt's error -- see Decrypt's doc for why: every
	// form fails with the same "message authentication failed" for a wrong
	// tenant, so the first is the one describing what the caller actually
	// asked for, not whichever previous key happened to run last.
	return nil, PayloadFormUnreadable, err
}

// openUnderPreviousKeys is cleat#1992's read half of rotation: mid-rotation, a
// row sealed under a key the operator has since retired to "previous" must
// still open, in whichever of the three shapes it was written in. Tries every
// version the ring holds except the current one -- KeyRing.Versions() already
// guarantees no two carry the same key, so trying them in any order reaches
// the right one; which order costs nothing but a handful of failed GCM opens
// on data that predates the CURRENT key, which is by construction the
// uncommon case once reseal-payloads has run.
func (tc *tenantCipher) openUnderPreviousKeys(data []byte) ([]byte, error) {
	if tc.ring == nil {
		return nil, errors.New("payload encryption: no key ring")
	}
	curVersion := tc.ring.Current().Version
	var lastErr error = errors.New("payload encryption: no previous key configured")
	for _, v := range tc.ring.Versions() {
		if v == curVersion {
			continue
		}
		vk, ok := tc.ring.Key(v)
		if !ok {
			continue
		}
		derived := make([]byte, 32)
		r := hkdf.New(sha256.New, vk.Key, []byte(tc.tenantID), []byte(payloadKeyInfo))
		if _, err := io.ReadFull(r, derived); err != nil {
			lastErr = err
			continue
		}
		if pt, err := openGCM(derived, data, []byte(tc.tenantID)); err == nil {
			return pt, nil
		}
		if pt, err := openGCM(vk.Key, data, []byte(tc.tenantID)); err == nil {
			return pt, nil
		}
		if pt, err := openGCM(vk.Key, data, nil); err == nil {
			return pt, nil
		} else {
			lastErr = err
		}
	}
	return nil, lastErr
}

// open is openClassify for callers that only want the plaintext.
func (tc *tenantCipher) open(data []byte) ([]byte, error) {
	pt, _, err := tc.openClassify(data)
	return pt, err
}

// Decrypt opens data produced by Encrypt, in any of the three forms.
//
// The two older forms are the only thing keeping rows written before this
// change readable, and they are also the residual: a PayloadFormLegacy value is
// bound to no tenant, and a PayloadFormBound value is readable with a key
// recovered from any other tenant's ciphertext. `cleatctl reseal-payloads`
// converts both; until it has run, this fallback is what a reader depends on.
//
// Errors report the NEWEST attempt, not the last: every form fails with
// "message authentication failed" for a wrong tenant, and the first is the one
// describing what the caller asked for.
func (pe *PayloadEncryption) Decrypt(tenantID string, data []byte) ([]byte, error) {
	pt, _, err := pe.OpenAndClassify(tenantID, data)
	return pt, err
}

// sealGCM and openGCM are the primitives, parameterised by KEY as well as AAD.
//
// Key-taking rather than methods on PayloadEncryption because there are now two
// keys in play -- the master and the per-tenant derived one -- and every form in
// the matrix above is one (key, aad) pair. A method closed over pe.key could
// only express the master-key forms, which is how the derived form would have
// ended up with its own near-duplicate of this code.
func sealGCM(key, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
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
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

func openGCM(key, data, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
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
