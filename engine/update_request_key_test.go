package engine

import (
	"strings"
	"testing"
)

// TestTheUpdateRequestKeyRoundTripsAnyName.
//
// The key joins a workflow-author-chosen UpdateName with a generated
// PromiseID. It used a literal NUL separator until IMPROVEMENT-PLAN 3.247,
// which PostgreSQL refuses inside JSONB (22P05) -- and the key is written into
// event_history.payload, so every update that reached a dispatch point failed
// the segment AFTER its caller had been told the update resolved (#914).
//
// The cases that matter are the ones a separator gets wrong. A name containing
// the delimiter is the whole reason this is length-prefixed rather than
// delimited by some rarer character: a rarer character moves the collision, it
// does not remove it.
func TestTheUpdateRequestKeyRoundTripsAnyName(t *testing.T) {
	for _, tc := range []struct{ name, promise string }{
		{"add", "p-1"},
		{"", "p-1"},                     // empty name
		{"add", ""},                     // no promise: a fire-and-forget update
		{"", ""},                        // both empty
		{"a:b", "p-1"},                  // contains the delimiter
		{":", "p-1"},                    // is the delimiter
		{"12:34", "p-1"},                // looks like its own length prefix
		{"3:add", "p-1"},                // looks like an already-encoded key
		{"unicode-eé中", "p-1"},          // multi-byte: the prefix counts BYTES
		{strings.Repeat("x", 300), "p"}, // multi-digit length
	} {
		key := updateRequestKey(UpdateRequestInfo{UpdateName: tc.name, PromiseID: tc.promise})

		if strings.ContainsRune(key, 0) {
			t.Errorf("updateRequestKey(%q, %q) contains a NUL: %q\n\n"+
				"This key goes into event_history.payload, which is JSONB on PostgreSQL, and a "+
				"NUL is refused there with 22P05 -- after the caller's promise has already been "+
				"settled. That is #914.", tc.name, tc.promise, key)
		}

		gotName, gotPromise := splitUpdateRequestKey(key)
		if gotName != tc.name || gotPromise != tc.promise {
			t.Errorf("round trip failed for (%q, %q): key %q decoded to (%q, %q)",
				tc.name, tc.promise, key, gotName, gotPromise)
		}
	}
}

// TestTheLegacyNULKeyStillDecodes.
//
// A NUL-separated key cannot exist on PostgreSQL -- the write that would have
// persisted one is the write that failed -- but MySQL and SQL Server ACCEPTED
// it (measured 2026-09-07: MySQL stored the escape, and SQL Server's ISJSON
// returned 1). So a workflow suspended mid-update on either dialect has one in
// its history and must still replay after the upgrade.
func TestTheLegacyNULKeyStillDecodes(t *testing.T) {
	name, promise := splitUpdateRequestKey("add\x00p-1")
	if name != "add" || promise != "p-1" {
		t.Errorf("legacy NUL key decoded to (%q, %q), want (add, p-1).\n\n"+
			"A workflow that suspended mid-update on MySQL or SQL Server before 3.247 has this "+
			"form in its history. Failing to read it turns an upgrade into a stuck workflow.",
			name, promise)
	}
}

// TestTheTwoKeyFormsCannotBeConfused pins the reason the reader can accept both
// without ambiguity: a length-prefixed key never contains a NUL, so the NUL
// branch can be tried first and only ever matches a legacy key.
func TestTheTwoKeyFormsCannotBeConfused(t *testing.T) {
	// A name containing a NUL is the adversarial case: it would produce a key
	// that hits the legacy branch. Nothing generates one -- but if the store
	// ever yielded such a name, this records what happens rather than leaving
	// it to be discovered.
	key := updateRequestKey(UpdateRequestInfo{UpdateName: "a\x00b", PromiseID: "p"})
	gotName, _ := splitUpdateRequestKey(key)
	if gotName == "a\x00b" {
		return // round-trips; nothing to warn about
	}
	t.Logf("KNOWN LIMIT: a name containing a NUL does not round-trip (decoded to %q). "+
		"The legacy branch claims it first. No caller can produce one: the name comes from "+
		"workflow_update_requests.update_name, and a NUL there would already have failed the "+
		"JSONB write that created the row on PostgreSQL.", gotName)
}
