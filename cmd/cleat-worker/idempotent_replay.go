package main

// The duplicate-call policy, in one place. cleat#1169.
//
// A caller that presents an `Idempotency-Key` and repeats the request gets
// **the original response** — same status, same field names — plus one standard
// flag saying whether this was the original call or a replay. In the repo
// owner's words:
//
//	there should be a standard policy of return-the-original-result, but with
//	a standard extra flag in the result … which lets the caller know whether
//	it's the original call or a cached-idempotent result, in case they have
//	reason to care. Mostly they don't care, and shouldn't need separate code
//	to handle the cached result.
//
// THE LAST SENTENCE IS THE REQUIREMENT. The property the old design lacked was
// not information — it was uniformity. A caller that never thinks about retries
// should be correct by default, and one that cares should opt in to noticing.
// Before this, every caller had to handle two shapes, and cleat's own port
// needed a shim to do it (ports/dbos-transact-py, `run_id(body)`).
//
// WHAT WAS THERE BEFORE, measured on develop@32a8b90b — three different
// mistakes about one question, not three arbitrary policies:
//
//	start       201 {"id":X}                  -> 200 {"already_started":"true","workflow_id":X,"status":…}
//	reprocess   201 {"id":X}                  -> 200 {"already_started":"true","workflow_id":X}
//	signal      200 {"status":"delivered"}    -> 200 {"status":"already_delivered"}
//	schedules   201 {"status":"created"}      -> 409 {"detail":"schedule_exists"}
//
// start and reprocess changed the status, the shape AND the identifier's field
// name. signal had the right status and shape but welded the marker into
// `status`, so a field meaning *what happened* also carried *was this a
// replay*. schedules refused — because it had no key with which to tell a
// retry from a genuine name collision.
//
// THE FIELD RENAME WAS THE EXPENSIVE ONE, and its failure mode is an inversion
// rather than an error: a caller reading only `id` gets nothing from a
// deduplicated response, concludes its retry started a SECOND workflow, and may
// compensate, alert, or start a third. Wrong in the alarming direction.
const idempotentReplayField = "idempotent_replay"

// withReplayFlag stamps the policy's one flag onto a response body.
//
// ALWAYS PRESENT, on the original as well as the replay, and that is deliberate
// rather than tidy. An absent field cannot be told from an old server that does
// not send one, so `absent means original` would make the flag unreadable by
// exactly the cautious client most likely to check it. Present-and-false says
// "I am telling you this is the original"; absent says nothing at all.
//
// It is the same reasoning the start handler already applies to `status`, which
// is set to "unknown" rather than omitted: a caller must be able to tell "I
// cannot tell you" from "I forgot to tell you".
func withReplayFlag(body map[string]any, replayed bool) map[string]any {
	if body == nil {
		body = map[string]any{}
	}
	body[idempotentReplayField] = replayed
	return body
}
