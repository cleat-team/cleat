package eventtriggers

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// maxCorrelationKeys and maxCorrelationKeyBytes are the shape §4 of
// docs/contributor/design/event-routing-design.md settles on: three slots,
// each capped at 128 bytes. See that section for why three (two covers every
// correlation observed in the wild, the third is headroom) and why 128 (the
// composite index has to fit inside MySQL's 3072-byte InnoDB key-prefix
// limit alongside tenant_id and event_type).
const (
	maxCorrelationKeys     = 3
	maxCorrelationKeyBytes = 128
)

// keySlots validates a correlation-key list and left-pads it into the three
// string slots migrations.go's Version 6 adds to ingested_events and
// event_awaiters. cleat#2625, P1 of the event routing design.
//
// Never truncates. §4.3 of the design doc is explicit about why: a
// correlation key that got silently cut at the slot boundary would still
// look like a valid key and would simply never match again -- the exact
// "read cleanest where it measured least" failure CLAUDE.md's "Is this
// result real?" section is about, arriving through a schema limit instead of
// a test. A key over the cap is a hard error instead, so the caller finds
// out at the call that is wrong rather than at every future call that
// silently never correlates.
//
// A missing slot is "", never omitted -- §4.4's rule that an unused slot is
// the empty string sentinel, not SQL NULL, because NULL = NULL is unknown
// and there is no dialect-portable spelling of "is not distinct from" across
// all three tier-1 databases.
func keySlots(keys []string) (key1, key2, key3 string, err error) {
	if len(keys) > maxCorrelationKeys {
		return "", "", "", fmt.Errorf(
			"event-triggers: at most %d correlation keys are supported (see the "+
				"event routing design's §4.1), got %d", maxCorrelationKeys, len(keys))
	}
	var slots [maxCorrelationKeys]string
	for i, k := range keys {
		if len(k) > maxCorrelationKeyBytes {
			return "", "", "", fmt.Errorf(
				"event-triggers: correlation key %d is %d bytes, over the %d-byte slot "+
					"cap -- hash it in the guest rather than have it silently truncated "+
					"and never match again",
				i+1, len(k), maxCorrelationKeyBytes)
		}
		slots[i] = k
	}
	return slots[0], slots[1], slots[2], nil
}

// registrationKey collapses the five-field registration tuple (workflowID,
// eventType, key1, key2, key3) into a single 64-character SHA-256 hex
// digest -- the value migrations.go's Version 6 puts the unique
// registration constraint on, in place of a raw five-column index.
//
// FOUND IN CLEAT-REVIEW ON THIS PR, NOT DESIGNED IN ADVANCE. The first
// version of this migration made the five raw columns the unique index, and
// it does not fit: MySQL's utf8mb4 VARCHAR(255) columns cost 1020 bytes
// each, so workflow_id + event_type + three 128-byte keys is
// 1020+1020+1536 = 3576 bytes against InnoDB's 3072-byte limit. Narrowing a
// column buys back only as many bytes as that column is wide, and the
// narrowest fix considered (shrinking workflow_id to VARCHAR(128)) left
// MySQL at 3068/3072 -- four bytes of margin on a limit that had already
// been hit once. A 64-byte hash sidesteps the ceiling permanently instead of
// buying headroom the next column change would spend.
//
// LENGTH-PREFIXED, NOT DELIMITER-JOINED. Writing "workflowID:eventType:..."
// and hashing the result would let ("ab", "c") and ("a", "bc") collide onto
// the same key the moment any field can contain the delimiter -- correlation
// keys are caller-supplied strings (§4 of the design doc), so nothing here
// can assume a byte is absent from them. Prefixing each field with its own
// length removes the ambiguity by construction: two different (workflowID,
// eventType, key1, key2, key3) tuples cannot produce the same length-prefixed
// byte stream, whatever bytes the fields themselves contain.
func registrationKey(workflowID, eventType, key1, key2, key3 string) string {
	h := sha256.New()
	var lenBuf [8]byte
	for _, s := range [5]string{workflowID, eventType, key1, key2, key3} {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		h.Write(lenBuf[:])
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}
