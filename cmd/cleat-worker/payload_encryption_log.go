package main

import (
	"context"
	"log/slog"
)

// describePayloadEncryption says what the two payload-encryption flags amount to,
// as the line the worker logs at startup (cleat#3589).
//
// --encryption-key-file loads a key and --encrypt-sensitive-payloads is what makes
// the engine seal anything with it: every seal in engine/ is gated on both. A key
// with no bool loads and seals NOTHING, so the line that used to say "encryption at
// rest enabled" for that case told an operator their workflow data was encrypted
// while it was stored in plaintext. The key is not idle in that state, though:
// plugins reach it through plugin.Payloads, which is why the combination is
// described rather than refused here.
func describePayloadEncryption(keyLoaded, sealSensitivePayloads, previousKey bool) (slog.Level, string) {
	switch {
	case keyLoaded && sealSensitivePayloads && previousKey:
		return slog.LevelInfo, "encryption at rest enabled for sensitive payload fields, with a previous key for rotation"
	case keyLoaded && sealSensitivePayloads:
		return slog.LevelInfo, "encryption at rest enabled for sensitive payload fields"
	case keyLoaded:
		return slog.LevelWarn, "payload key loaded, but sensitive-payload encryption is OFF because --encrypt-sensitive-payloads is not set: " +
			"workflow data is stored in PLAINTEXT. The key is available only to plugins, through plugin.Payloads"
	default:
		return slog.LevelInfo, "sensitive-payload encryption is off: no --encryption-key-file, workflow data is stored in plaintext"
	}
}

// logPayloadEncryption logs describePayloadEncryption's line once.
func logPayloadEncryption(logger *slog.Logger, workerID string, keyLoaded, sealSensitivePayloads, previousKey bool) {
	level, msg := describePayloadEncryption(keyLoaded, sealSensitivePayloads, previousKey)
	logger.Log(context.Background(), level, msg, "worker_id", workerID)
}
