//go:build !tinygo

package plugin

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultOfficialPublicKey is the default Ed25519 public key for verifying
// official cleat plugin signatures. This is a well-known key distributed with
// the cleat binary. In production, operators can override this with a custom
// key via configuration.
//
// The hex-encoded value below is a placeholder and MUST be replaced with the
// actual official cleat signing public key before release.
var DefaultOfficialPublicKey = ""

// OfficialPublicKeys maps SigningKeyID to Ed25519 public keys for official
// plugin verification. Keys are registered at build time or loaded from
// configuration.
var OfficialPublicKeys = map[string]ed25519.PublicKey{}

// RegisterOfficialPublicKey registers an Ed25519 public key for verifying
// official plugin signatures. The keyID is an opaque identifier (e.g.,
// "cleat-official-2026") and the pubKeyHex is a hex-encoded Ed25519 public
// key (32 bytes = 64 hex chars).
func RegisterOfficialPublicKey(keyID, pubKeyHex string) error {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return fmt.Errorf("plugin signing: key ID must not be empty")
	}

	pubKeyHex = strings.TrimSpace(pubKeyHex)
	if len(pubKeyHex) != ed25519.PublicKeySize*2 {
		return fmt.Errorf("plugin signing: public key hex must be %d hex characters (got %d)",
			ed25519.PublicKeySize*2, len(pubKeyHex))
	}

	raw, err := hex.DecodeString(pubKeyHex)
	if err != nil {
		return fmt.Errorf("plugin signing: decode public key hex: %w", err)
	}

	pubKey := ed25519.PublicKey(raw)
	OfficialPublicKeys[keyID] = pubKey
	return nil
}

// MustRegisterOfficialPublicKey is like RegisterOfficialPublicKey but panics
// on error. Use in init() functions or main() to register well-known keys.
func MustRegisterOfficialPublicKey(keyID, pubKeyHex string) {
	if err := RegisterOfficialPublicKey(keyID, pubKeyHex); err != nil {
		panic(fmt.Sprintf("plugin signing: register key %q: %v", keyID, err))
	}
}

// VerifyManifestSignature checks the Ed25519 signature on a manifest.
// If the manifest has no signature field, verification is skipped (returns nil).
// For official plugins (those using a cleat/ prefix or having no slash in the
// name), a missing signature is an error unless allowUnsigned is true.
func VerifyManifestSignature(m *Manifest, allowUnsigned bool) error {
	if m.Signature == "" {
		if !allowUnsigned && isOfficialName(m.Name) {
			return fmt.Errorf("plugin %q is official but has no signature", m.Name)
		}
		return nil
	}

	pubKey, err := resolvePublicKey(m.SigningKeyID)
	if err != nil {
		return fmt.Errorf("plugin %q: %w", m.Name, err)
	}

	// Compute the canonical JSON of the manifest excluding the signature field.
	canonical, err := canonicalManifestJSON(m)
	if err != nil {
		return fmt.Errorf("plugin %q: canonical JSON: %w", m.Name, err)
	}

	sig, err := hex.DecodeString(m.Signature)
	if err != nil {
		return fmt.Errorf("plugin %q: decode signature hex: %w", m.Name, err)
	}

	if !ed25519.Verify(pubKey, canonical, sig) {
		return fmt.Errorf("plugin %q: Ed25519 signature verification failed", m.Name)
	}

	return nil
}

// VerifyWasmSignature checks the Ed25519 signature of a WASM binary against
// the expected checksum and the index version's signature field.
// The signature is computed over the SHA-256 hash of the WASM binary bytes.
func VerifyWasmSignature(wasmBytes []byte, iv *IndexVersion, pluginName string, allowUnsigned bool) error {
	if iv.Signature == "" {
		if !allowUnsigned && isOfficialName(pluginName) {
			return fmt.Errorf("plugin %q: official plugin WASM has no signature", iv.Version)
		}
		return nil
	}

	pubKey, err := resolvePublicKey(iv.SigningKeyID)
	if err != nil {
		return fmt.Errorf("plugin version %q: %w", iv.Version, err)
	}

	// The signature is over the SHA-256 hash of the WASM binary.
	wasmHash := sha256.Sum256(wasmBytes)

	sig, err := hex.DecodeString(iv.Signature)
	if err != nil {
		return fmt.Errorf("plugin version %q: decode signature hex: %w", iv.Version, err)
	}

	if !ed25519.Verify(pubKey, wasmHash[:], sig) {
		return fmt.Errorf("plugin version %q: Ed25519 WASM signature verification failed", iv.Version)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// canonicalManifestJSON returns a canonical JSON representation of the
// manifest with the signature and signing_key_id fields removed, suitable
// for signing or signature verification.
func canonicalManifestJSON(m *Manifest) ([]byte, error) {
	// Create a copy to avoid mutating the original.
	clone := *m
	clone.Signature = ""
	clone.SigningKeyID = ""

	// Marshal with field ordering (Go's encoding/json produces
	// deterministic output for structs with the same field order).
	return json.Marshal(clone)
}

// resolvePublicKey looks up an Ed25519 public key by key ID.
// If keyID is empty, returns the default official public key.
func resolvePublicKey(keyID string) (ed25519.PublicKey, error) {
	if keyID == "" {
		if DefaultOfficialPublicKey != "" {
			raw, err := hex.DecodeString(DefaultOfficialPublicKey)
			if err != nil {
				return nil, fmt.Errorf("invalid default public key: %w", err)
			}
			return ed25519.PublicKey(raw), nil
		}
		// Try a single registered key.
		for _, pk := range OfficialPublicKeys {
			return pk, nil
		}
		return nil, fmt.Errorf("no signing key registered and no default key configured")
	}

	pk, ok := OfficialPublicKeys[keyID]
	if !ok {
		return nil, fmt.Errorf("unknown signing key ID %q", keyID)
	}
	return pk, nil
}

// isOfficialName returns true if the plugin name indicates an official plugin
// (cleat/ prefix or no slash in the name).
func isOfficialName(name string) bool {
	return strings.HasPrefix(name, "cleat/") || !strings.Contains(name, "/")
}

