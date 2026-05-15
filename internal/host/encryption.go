// Package host provides the core cleat workflow engine, including encryption
// at rest for sensitive event payloads.
package host

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
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
type PayloadEncryption struct {
	key []byte
}

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

// Encrypt encrypts plaintext using AES-256-GCM and returns nonce || ciphertext.
//
// NOTE: In a multi-tenant deployment, the tenant_id should be passed as
// additional authenticated data (AAD) to bind ciphertexts to their tenant
// and prevent cross-tenant ciphertext substitution. This defense-in-depth
// improvement is reserved for future work; currently AAD is nil.
func (pe *PayloadEncryption) Encrypt(plaintext []byte) ([]byte, error) {
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
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// Decrypt decrypts data produced by Encrypt (nonce || ciphertext).
func (pe *PayloadEncryption) Decrypt(data []byte) ([]byte, error) {
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
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: open: %w", err)
	}
	return plaintext, nil
}

// EncryptString encrypts a plaintext string and returns base64-encoded output.
func (pe *PayloadEncryption) EncryptString(plaintext string) (string, error) {
	ciphertext, err := pe.Encrypt([]byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// DecryptString decrypts a base64-encoded ciphertext string.
func (pe *PayloadEncryption) DecryptString(encoded string) (string, error) {
	plaintext, err := pe.DecryptBase64(encoded)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// DecryptBase64 base64-decodes the input then decrypts the result.
func (pe *PayloadEncryption) DecryptBase64(encoded string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decrypt base64: decode: %w", err)
	}
	return pe.Decrypt(data)
}

// EncryptJSON encrypts JSON bytes and returns them as a JSON string literal
// (i.e., a quoted base64 string that is valid JSON). This allows encrypted
// payloads to be stored in JSONB columns.
func (pe *PayloadEncryption) EncryptJSON(jsonBytes []byte) ([]byte, error) {
	ciphertext, err := pe.Encrypt(jsonBytes)
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
func (pe *PayloadEncryption) DecryptJSON(jsonValue []byte) ([]byte, error) {
	// Expect a JSON string literal: "<base64>"
	if len(jsonValue) < 2 || jsonValue[0] != '"' || jsonValue[len(jsonValue)-1] != '"' {
		return nil, fmt.Errorf("decrypt JSON: value is not a JSON string literal")
	}
	encoded := string(jsonValue[1 : len(jsonValue)-1])
	return pe.DecryptBase64(encoded)
}
