package engine

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"
)

// validKey returns a base64-encoded 32-byte key suitable for PayloadEncryption.
func validKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestNewPayloadEncryption_Valid(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pe == nil {
		t.Fatal("expected non-nil PayloadEncryption")
	}
}

func TestNewPayloadEncryption_InvalidBase64(t *testing.T) {
	_, err := NewPayloadEncryption("not-valid-base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestNewPayloadEncryption_KeyTooShort(t *testing.T) {
	shortKey := base64.StdEncoding.EncodeToString(make([]byte, 16))
	_, err := NewPayloadEncryption(shortKey)
	if err == nil {
		t.Fatal("expected error for key < 32 bytes")
	}
}

func TestNewPayloadEncryption_KeyTooLong(t *testing.T) {
	longKey := base64.StdEncoding.EncodeToString(make([]byte, 64))
	_, err := NewPayloadEncryption(longKey)
	if err == nil {
		t.Fatal("expected error for key > 32 bytes")
	}
}

func TestNewPayloadEncryption_KeyExactlyWrongLength(t *testing.T) {
	// 31 bytes — one short.
	k31 := base64.StdEncoding.EncodeToString(make([]byte, 31))
	_, err := NewPayloadEncryption(k31)
	if err == nil {
		t.Fatal("expected error for 31-byte key")
	}

	// 33 bytes — one over.
	k33 := base64.StdEncoding.EncodeToString(make([]byte, 33))
	_, err = NewPayloadEncryption(k33)
	if err == nil {
		t.Fatal("expected error for 33-byte key")
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	tests := []struct {
		name      string
		plaintext []byte
	}{
		{"empty", []byte{}},
		{"short_string", []byte("hello world")},
		{"long_string", []byte(`{"workflow_id":"abc-123","step":42,"data":"some payload with nested data"}`)},
		{"binary_with_nulls", []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0x00, 0x7F}},
		{"utf8_mixed", []byte("hello\x00world\xFF\xFEunicode")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ciphertext, err := pe.Encrypt(tt.plaintext)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			plaintext, err := pe.Decrypt(ciphertext)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(plaintext, tt.plaintext) {
				t.Errorf("round-trip mismatch: got %v, want %v", plaintext, tt.plaintext)
			}
		})
	}
}

func TestEncrypt_ProducesDifferentCiphertexts(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	plaintext := []byte("same plaintext")
	ct1, err := pe.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("first Encrypt: %v", err)
	}
	ct2, err := pe.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("second Encrypt: %v", err)
	}
	if bytes.Equal(ct1, ct2) {
		t.Error("two encryptions of same plaintext should produce different ciphertexts (random nonce)")
	}
}

func TestDecrypt_CiphertextTooShort(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	_, err = pe.Decrypt([]byte{0x01, 0x02})
	if err == nil {
		t.Fatal("expected error for ciphertext shorter than nonce")
	}
}

func TestDecrypt_TamperedCiphertext(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	plaintext := []byte("sensitive data")
	ciphertext, err := pe.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Flip a byte in the ciphertext portion (after the nonce).
	if len(ciphertext) > 13 {
		ciphertext[12] ^= 0xFF
	}

	_, err = pe.Decrypt(ciphertext)
	if err == nil {
		t.Fatal("expected authentication error for tampered ciphertext")
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	pe1, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption 1: %v", err)
	}
	pe2, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption 2: %v", err)
	}

	plaintext := []byte("secret")
	ciphertext, err := pe1.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	_, err = pe2.Decrypt(ciphertext)
	if err == nil {
		t.Fatal("expected error when decrypting with wrong key")
	}
}

func TestEncryptStringDecryptString_RoundTrip(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	tests := []string{
		"hello world",
		"",
		"special chars: !@#$%^&*()",
		"unicode: 你好世界 🌍",
	}

	for _, plaintext := range tests {
		enc, err := pe.EncryptString(plaintext)
		if err != nil {
			t.Fatalf("EncryptString(%q): %v", plaintext, err)
		}
		dec, err := pe.DecryptString(enc)
		if err != nil {
			t.Fatalf("DecryptString(%q): %v", plaintext, err)
		}
		if dec != plaintext {
			t.Errorf("round-trip mismatch: got %q, want %q", dec, plaintext)
		}
	}
}

func TestDecryptBase64_Valid(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	plaintext := []byte("test data")
	ciphertext, err := pe.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(ciphertext)

	got, err := pe.DecryptBase64(encoded)
	if err != nil {
		t.Fatalf("DecryptBase64: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round-trip mismatch: got %v, want %v", got, plaintext)
	}
}

func TestDecryptBase64_InvalidBase64(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	_, err = pe.DecryptBase64("!!!not-base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestDecryptString_InvalidCiphertext(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	// Valid base64 but not valid ciphertext (random 20 bytes).
	garbage := base64.StdEncoding.EncodeToString(make([]byte, 20))
	_, err = pe.DecryptString(garbage)
	if err == nil {
		t.Fatal("expected error for garbage ciphertext")
	}
}

func TestEncryptJSONDecryptJSON_RoundTrip(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	tests := [][]byte{
		[]byte(`{"key":"value"}`),
		[]byte(`{"nested":{"a":1,"b":[2,3]}}`),
		[]byte(`[]`),
		[]byte(`"just a string"`),
		[]byte(`42`),
	}

	for _, original := range tests {
		enc, err := pe.EncryptJSON(original)
		if err != nil {
			t.Fatalf("EncryptJSON(%s): %v", original, err)
		}

		// Output should be a JSON string literal (quoted).
		if len(enc) < 2 || enc[0] != '"' || enc[len(enc)-1] != '"' {
			t.Errorf("EncryptJSON output should be quoted: got %s", enc)
		}

		got, err := pe.DecryptJSON(enc)
		if err != nil {
			t.Fatalf("DecryptJSON(%s): %v", enc, err)
		}
		if !bytes.Equal(got, original) {
			t.Errorf("JSON round-trip mismatch: got %s, want %s", got, original)
		}
	}
}

func TestDecryptJSON_NotAString(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	// Input is not a JSON string literal.
	_, err = pe.DecryptJSON([]byte(`123`))
	if err == nil {
		t.Fatal("expected error for non-string JSON value")
	}

	_, err = pe.DecryptJSON([]byte(`true`))
	if err == nil {
		t.Fatal("expected error for non-string JSON value")
	}
}

func TestDecryptJSON_InvalidBase64Content(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	// Quoted but content is not valid base64.
	_, err = pe.DecryptJSON([]byte(`"!!!not-base64!!!"`))
	if err == nil {
		t.Fatal("expected error for invalid base64 content")
	}
}

func TestEncryptDecrypt_LargePayload(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	large := bytes.Repeat([]byte("x"), 100000)
	ciphertext, err := pe.Encrypt(large)
	if err != nil {
		t.Fatalf("Encrypt large: %v", err)
	}
	plaintext, err := pe.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt large: %v", err)
	}
	if !bytes.Equal(plaintext, large) {
		t.Errorf("large payload round-trip mismatch: len(got)=%d, len(want)=%d", len(plaintext), len(large))
	}
}

func TestEncrypt_OutputContainsNonce(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	plaintext := []byte("test")
	ciphertext, err := pe.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Empty plaintext produces nonce (12 bytes) + tag (16 bytes) = 28 bytes.
	// Non-empty adds plaintext length.
	if len(ciphertext) < 28 {
		t.Errorf("ciphertext too short: len=%d, expected at least 28 (12 nonce + 16 tag)", len(ciphertext))
	}
}

func TestEncrypt_EmptyPlaintext(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	ct, err := pe.Encrypt(nil)
	if err != nil {
		t.Fatalf("Encrypt nil: %v", err)
	}
	pt, err := pe.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if len(pt) != 0 {
		t.Errorf("expected empty result, got %v", pt)
	}

	ct2, err := pe.Encrypt([]byte{})
	if err != nil {
		t.Fatalf("Encrypt empty: %v", err)
	}
	pt2, err := pe.Decrypt(ct2)
	if err != nil {
		t.Fatalf("Decrypt empty: %v", err)
	}
	if len(pt2) != 0 {
		t.Errorf("expected empty result, got %v", pt2)
	}
}

func TestEncryptDecrypt_MultipleEncryptionsIndependent(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	// Run many encryptions and verify all decrypt correctly.
	for i := 0; i < 50; i++ {
		plaintext := []byte("iteration test")
		ct, err := pe.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt iteration %d: %v", i, err)
		}
		pt, err := pe.Decrypt(ct)
		if err != nil {
			t.Fatalf("Decrypt iteration %d: %v", i, err)
		}
		if !bytes.Equal(pt, plaintext) {
			t.Errorf("iteration %d: round-trip mismatch", i)
		}
	}
}

func TestEncryptDecrypt_Concurrent(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	const goroutines = 10
	const iterations = 100
	errs := make(chan error, goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			for i := 0; i < iterations; i++ {
				plaintext := []byte("concurrent test data")
				ct, err := pe.Encrypt(plaintext)
				if err != nil {
					errs <- err
					return
				}
				pt, err := pe.Decrypt(ct)
				if err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(pt, plaintext) {
					errs <- fmt.Errorf("concurrent round-trip mismatch: got %v, want %v", pt, plaintext)
					return
				}
			}
			errs <- nil
		}(g)
	}

	for g := 0; g < goroutines; g++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent test error: %v", err)
		}
	}
}

func TestDecrypt_EmptyInput(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	_, err = pe.Decrypt(nil)
	if err == nil {
		t.Fatal("expected error for nil input")
	}
	_, err = pe.Decrypt([]byte{})
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestDecryptJSON_EmptyInput(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	_, err = pe.DecryptJSON(nil)
	if err == nil {
		t.Fatal("expected error for nil input")
	}
	_, err = pe.DecryptJSON([]byte{})
	if err == nil {
		t.Fatal("expected error for empty input")
	}

	// Empty JSON string (just quotes).
	_, err = pe.DecryptJSON([]byte(`""`))
	if err == nil {
		t.Fatal("expected error for empty quoted string (empty base64)")
	}
}

func TestEncryptJSON_EmptyInput(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	enc, err := pe.EncryptJSON([]byte{})
	if err != nil {
		t.Fatalf("EncryptJSON empty: %v", err)
	}
	if len(enc) < 2 || enc[0] != '"' || enc[len(enc)-1] != '"' {
		t.Errorf("expected quoted output, got %s", enc)
	}
	got, err := pe.DecryptJSON(enc)
	if err != nil {
		t.Fatalf("DecryptJSON: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}
