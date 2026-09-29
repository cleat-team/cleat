package eventtriggers

import "fmt"

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
