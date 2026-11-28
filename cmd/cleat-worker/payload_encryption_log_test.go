package main

import (
	"log/slog"
	"strings"
	"testing"
)

// cleat#3589: --encryption-key-file alone loads a key and seals nothing, and used to
// log the same "encryption at rest enabled" line as the combination that does seal.
func TestPayloadEncryptionStartupLineSaysWhatIsTrue(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		keyLoaded, seal, prior bool
		level                  slog.Level
		mustHave               []string
		mustNotHave            []string
	}{
		{"neither flag", false, false, false, slog.LevelInfo,
			[]string{"is off", "plaintext"}, []string{"enabled"}},
		{"key file only: nothing is sealed, and that is a WARN", true, false, false, slog.LevelWarn,
			[]string{"OFF", "--encrypt-sensitive-payloads", "PLAINTEXT", "plugin.Payloads"}, []string{"encryption at rest enabled"}},
		{"key file only, with a previous key", true, false, true, slog.LevelWarn,
			[]string{"OFF", "PLAINTEXT"}, []string{"encryption at rest enabled"}},
		{"both flags: sealed", true, true, false, slog.LevelInfo,
			[]string{"encryption at rest enabled for sensitive payload fields"}, []string{"OFF", "PLAINTEXT", "rotation"}},
		{"both flags with a previous key: sealed, rotating", true, true, true, slog.LevelInfo,
			[]string{"encryption at rest enabled", "previous key for rotation"}, []string{"OFF", "PLAINTEXT"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			level, msg := describePayloadEncryption(tc.keyLoaded, tc.seal, tc.prior)
			if level != tc.level {
				t.Errorf("level = %v, want %v (message: %q)", level, tc.level, msg)
			}
			for _, w := range tc.mustHave {
				if !strings.Contains(msg, w) {
					t.Errorf("message %q lacks %q", msg, w)
				}
			}
			for _, w := range tc.mustNotHave {
				if strings.Contains(msg, w) {
					t.Errorf("message %q contains %q, which is not true in this state", msg, w)
				}
			}
		})
	}
}

// The line an operator reads for "key only" must differ from the one for "both":
// the bug was that they were the same string.
func TestPayloadEncryptionKeyOnlyAndSealedAreNotTheSameLine(t *testing.T) {
	_, keyOnly := describePayloadEncryption(true, false, false)
	_, sealed := describePayloadEncryption(true, true, false)
	if keyOnly == sealed {
		t.Fatalf("key-only and sealed log the same line: %q", keyOnly)
	}
}
