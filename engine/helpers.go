package engine

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
)

// truncateWithHash truncates s to maxLen bytes, appending "... [sha256=<hash>]"
// if truncation occurred. Returns s unchanged if len(s) <= maxLen.
func truncateWithHash(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%s... [sha256=%x]", s[:maxLen], h)
}

// lookupKey returns a unique key for a plugin function. The \x00 separator
// prevents collisions between names like "a/b" and "a/b" (which would collide
// with "/") and is guaranteed not to appear in valid plugin or function names.
func lookupKey(pluginName, funcName string) string {
	return pluginName + "\x00" + funcName
}

// isComponentWasm detects if the WASM binary uses the Component Model format.
// Component Model binaries use version 0x0001000d in the header, distinguishing
// them from core WASM modules (version 1).
func isComponentWasm(wasmBytes []byte) bool {
	return len(wasmBytes) >= 8 &&
		wasmBytes[4] == 0x0d && wasmBytes[5] == 0x00 &&
		wasmBytes[6] == 0x01 && wasmBytes[7] == 0x00
}

// DeferralsFromHistory returns the defers registered during the execution that produced
// the given history. It scans the history for defer events.
func DeferralsFromHistory(history []EventRecord) map[string]string {
	defs := make(map[string]string)
	for _, rec := range history {
		if rec.EventType == EventTypeDefer {
			defs[rec.DeferID] = rec.DeferDescription
		}
	}
	return defs
}

// ---- Result packing helpers ----

func packSleepResult(status byte, durationMs int64) int64 {
	return int64(uint64(status)<<56 | uint64(durationMs)&0x00FFFFFFFFFFFFFF)
}

func packAwaitSignalsResult(sigNameLen, payloadLen uint32, timedOut bool, errCode uint32) int64 {
	toFlag := uint32(0)
	if timedOut {
		toFlag = 1
	}
	return int64(uint64(sigNameLen)<<48 | uint64(payloadLen)<<32 | uint64(toFlag)<<16 | uint64(errCode))
}

func packAwaitChildResult(written uint32, errCode uint32) int64 {
	return int64(uint64(written)<<32 | uint64(errCode))
}

func packAwaitChildResultSuspend() int64 {
	return 1 << 62
}

func packAwaitPromiseResult(resultLen uint32, timedOut bool, errCode uint16) int64 {
	toFlag := uint32(0)
	if timedOut {
		toFlag = 1
	}
	return int64(uint64(resultLen)<<32 | uint64(toFlag)<<16 | uint64(errCode))
}

func packAcquireLockResult(acquired bool, errCode uint32) int64 {
	a := uint32(0)
	if acquired {
		a = 1
	}
	return int64(uint64(a)<<8 | uint64(errCode))
}

// isDefinitelyNonRetryable checks if an error should not be retried.
// Returns true if the error's Retryable() method returns false, or if
// the error message matches any of the non-retryable patterns.
func isDefinitelyNonRetryable(err error, nonRetryablePatterns []string) bool {
	// Check if error self-reports as non-retryable via Retryable interface.
	var re RetryableError
	if errors.As(err, &re) {
		if !re.Retryable() {
			return true
		}
	}

	// Check non-retryable error substrings.
	if len(nonRetryablePatterns) > 0 {
		errMsg := err.Error()
		for _, p := range nonRetryablePatterns {
			if strings.Contains(errMsg, p) {
				return true
			}
		}
	}

	return false
}

func splitSignalNames(names string) []string {
	if names == "" {
		return nil
	}
	parts := make([]string, 0)
	start := 0
	for i := 0; i < len(names); i++ {
		if names[i] == ',' {
			parts = append(parts, names[start:i])
			start = i + 1
		}
	}
	parts = append(parts, names[start:])
	return parts
}

// stripCompactedEvents removes virtual events that were prepended from compaction
// state from the result history. This ensures the caller sees only the tail events
// plus any new events produced during this execution.
func stripCompactedEvents(history []EventRecord, compactedStep int) []EventRecord {
	if compactedStep <= 0 || compactedStep >= len(history) {
		return history
	}
	result := make([]EventRecord, len(history)-compactedStep)
	copy(result, history[compactedStep:])
	return result
}

// parseDeferStepNo extracts the numeric step from a defer ID of the form "defer-N".
// Returns -1 if the ID does not match the expected pattern.
func parseDeferStepNo(id string) int {
	var n int
	if _, err := fmt.Sscanf(id, "defer-%d", &n); err != nil {
		return -1
	}
	return n
}
