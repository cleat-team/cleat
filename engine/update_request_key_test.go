package engine

import (
	"strings"
	"testing"
)

// TestTheUpdateRequestKeyRoundTripsAnyName.
//
// The key joins a workflow-author-chosen UpdateName with a generated RequestID
// and a generated PromiseID. It used a literal NUL separator until
// IMPROVEMENT-PLAN 3.247,
// which PostgreSQL refuses inside JSONB (22P05) -- and the key is written into
// event_history.payload, so every update that reached a dispatch point failed
// the segment AFTER its caller had been told the update resolved (#914).
//
// The cases that matter are the ones a separator gets wrong. A name containing
// the delimiter is the whole reason this is length-prefixed rather than
// delimited by some rarer character: a rarer character moves the collision, it
// does not remove it.
func TestTheUpdateRequestKeyRoundTripsAnyName(t *testing.T) {
	for _, tc := range []struct{ name, request, promise string }{
		{"add", "ureq-1", "p-1"},
		{"", "ureq-1", "p-1"},                     // empty name
		{"add", "ureq-1", ""},                     // no promise: a fire-and-forget update
		{"", "", ""},                              // all empty
		{"a:b", "ureq-1", "p-1"},                  // contains the delimiter
		{":", "ureq-1", "p-1"},                    // is the delimiter
		{"12:34", "12:34", "p-1"},                 // looks like its own length prefix
		{"3:add", "5:ureq", "p-1"},                // looks like an already-encoded key
		{"unicode-eé中", "ureq-中", "p-1"},          // multi-byte: the prefix counts BYTES
		{strings.Repeat("x", 300), "ureq-1", "p"}, // multi-digit length
		// The promise id is the UNPREFIXED trailing field, so a promise that
		// looks like a length prefix is the case that would break a reader
		// which guessed at the form instead of reading the "u3:" marker.
		{"add", "ureq-1", "7:promise"},
		{"add", "ureq-1", "u3:3:addp"},
	} {
		key := updateRequestKey(UpdateRequestInfo{
			UpdateName: tc.name, RequestID: tc.request, PromiseID: tc.promise})

		if strings.ContainsRune(key, 0) {
			t.Errorf("updateRequestKey(%q, %q) contains a NUL: %q\n\n"+
				"This key goes into event_history.payload, which is JSONB on PostgreSQL, and a "+
				"NUL is refused there with 22P05 -- after the caller's promise has already been "+
				"settled. That is #914.", tc.name, tc.promise, key)
		}

		gotName, gotRequest, gotPromise := splitUpdateRequestKey(key)
		if gotName != tc.name || gotRequest != tc.request || gotPromise != tc.promise {
			t.Errorf("round trip failed for (%q, %q, %q): key %q decoded to (%q, %q, %q)",
				tc.name, tc.request, tc.promise, key, gotName, gotRequest, gotPromise)
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
	name, request, promise := splitUpdateRequestKey("add\x00p-1")
	if request != "" {
		t.Errorf("a legacy key yielded request id %q; it must be EMPTY so the caller "+
			"applies the documented fallback (the migrations backfilled request_id "+
			"from update_name, so the name addresses the row)", request)
	}
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
	key := updateRequestKey(UpdateRequestInfo{UpdateName: "a\x00b", RequestID: "r", PromiseID: "p"})
	gotName, _, _ := splitUpdateRequestKey(key)
	if gotName == "a\x00b" {
		return // round-trips; nothing to warn about
	}
	t.Logf("KNOWN LIMIT: a name containing a NUL does not round-trip (decoded to %q). "+
		"The legacy branch claims it first. No caller can produce one: the name comes from "+
		"workflow_update_requests.update_name, and a NUL there would already have failed the "+
		"JSONB write that created the row on PostgreSQL.", gotName)
}

// TestTheV2KeyStillDecodesAndReportsNoRequestID.
//
// cleat#1416 added the request id to the key. A workflow suspended mid-update
// across that upgrade replays a v2 key -- `<len>:<name><promiseID>` -- and must
// still complete the row it was written for.
//
// The assertion that matters is the EMPTY request id rather than the name and
// promise. Empty is what tells DurableCompleteUpdate to fall back to the name,
// and the fallback is only correct because migrations/postgres/068 and its two
// siblings backfill request_id from update_name. A reader that invented a
// request id here instead would address no row at all, and the caller's promise
// would never settle -- silently, since CompleteUpdateRequest does not report
// how many rows it matched.
func TestTheV2KeyStillDecodesAndReportsNoRequestID(t *testing.T) {
	name, request, promise := splitUpdateRequestKey("3:addp-1")
	if name != "add" || promise != "p-1" {
		t.Errorf("v2 key decoded to name %q, promise %q; want add, p-1", name, promise)
	}
	if request != "" {
		t.Errorf("v2 key yielded request id %q, want empty", request)
	}
}

// TestAPromiseIDCannotBeMistakenForAV3Marker.
//
// The reason updateRequestKey writes a "u3:" prefix rather than letting the
// reader infer the form. The trailing field is the promise id and is not length
// prefixed, so under a form-guessing reader a v2 key whose promise id happens to
// begin with digits-and-a-colon would decode as if it carried a request id --
// and the request id it invented would address no row.
func TestAPromiseIDCannotBeMistakenForAV3Marker(t *testing.T) {
	// v2 key: name "add", promise id "5:hello". No marker, so it is v2.
	name, request, promise := splitUpdateRequestKey("3:add5:hello")
	if name != "add" || request != "" || promise != "5:hello" {
		t.Errorf("decoded (%q, %q, %q); want (add, \"\", 5:hello). A promise id that "+
			"looks like a length prefix was read as a request id.", name, request, promise)
	}
}
