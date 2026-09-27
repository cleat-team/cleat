package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrScheduleIdempotentReplay reports that this exact request has already been
// served: the presented Idempotency-Key is held by a schedule this tenant
// created, with the same input.
//
// IT IS A SUCCESS WEARING AN ERROR'S CLOTHES, and that is a deliberate trade
// rather than an accident of the signature. CreateSchedule returns only `error`,
// and it is implemented thirteen times -- four real stores and nine mocks
// (re-derive with `git grep -n ') CreateSchedule(ctx'`). Widening the return, or
// adding a reserve-or-return method beside it, changes every one of those to
// give one endpoint a capability, and the nine mocks would each have to grow an
// implementation of a policy they do not model.
//
// So the outcome travels as a sentinel. The cost is that a caller which ignores
// the distinction would report a successful replay as a failure, and the reason
// none does is structural rather than diligent: this error is returned ONLY on
// the branches gated by `sch.IdempotencyKey != ""`, and only the HTTP handler
// sets that field. The other two callers -- `cleat schedule create`
// (cmd/cleat/main.go) and engine/schedules.go's registration path -- construct a
// Schedule without a key and therefore cannot reach it.
//
// That is asserted rather than described: see cmd/cleat-worker's
// TestOnlyAKeyBearingCallerCanReceiveTheReplaySentinel, which derives the caller
// set from the source instead of trusting this paragraph to stay true.
var ErrScheduleIdempotentReplay = errors.New(
	"schedule already created by this idempotency key")

// scheduleIdempotencyVerdict decides what a key hit means.
//
// The policy lives here rather than in each dialect's CreateSchedule because
// there is exactly one policy and three places that would otherwise each state
// it. Two of those three would then be free to drift, and the drift would be
// invisible on any single-dialect run -- which is most local runs.
//
// A NULL stored digest is UNKNOWN and replays, which is checkIdempotencyInput's
// rule and is inherited rather than restated: a row predating the column has
// nothing to compare against, and refusing it would be a refusal the caller
// cannot act on.
func scheduleIdempotencyVerdict(storedDigest sql.NullString, want string) error {
	// A NULL or empty stored digest is UNKNOWN, not mismatched -- the row
	// predates the column and there is nothing to compare against. Allowing it
	// through degrades to the behaviour before this change rather than to a
	// refusal the caller cannot act on. Same rule as checkIdempotencyInput,
	// restated rather than called because that function's message says "a run
	// started with a different input", and a schedule is not a run: a caller
	// reading it would go looking for a workflow that does not exist.
	if !storedDigest.Valid || storedDigest.String == "" || storedDigest.String == want {
		return ErrScheduleIdempotentReplay
	}
	return fmt.Errorf("%w: this key created a schedule from a different request "+
		"(stored digest %s, this request %s). The digest covers the name, definition, "+
		"entry point, cron expression, input and policies -- not next_run_at",
		ErrIdempotencyKeyInputMismatch,
		shortDigest(storedDigest.String), shortDigest(want))
}

// scheduleRequestDigest fingerprints the WHOLE create request, not just its
// input, and that is the difference between this check working and looking like
// it works.
//
// cleat#1170's defect was a second request being told its work was under way
// while its own arguments were discarded without a word. Digesting `input`
// alone reproduces exactly that one field over: a retry presenting the same key
// with a different CRON EXPRESSION, a different def_name, or a different name
// digests equal, replays, and the schedule the caller asked for the second time
// is never created. The caller is told `201 created` about a schedule that does
// not match what it sent.
//
// So the digest covers every field that decides what the schedule IS.
//
// NEXT_RUN_AT IS DELIBERATELY EXCLUDED, and it is the one exclusion that must
// not be tidied away later. It is computed from the cron expression and the
// clock at the moment of the request (see handleCreateSchedule), so two
// genuinely identical retries a second apart produce different values. Include
// it and every retry is a mismatch -- the feature fails closed, uniformly, and
// the error blames the caller for a difference it did not create.
//
// NORMALISED THROUGH THE SAME DEFAULTING THE STORE APPLIES. A request that omits
// `timezone` and one that sends "UTC" create the identical schedule, so they
// must digest identically; comparing the raw fields would refuse the second as a
// payload mismatch against a row it agrees with. Same reasoning as
// scheduleInputOrDefault, applied to all five defaulted fields.
func scheduleRequestDigest(sch Schedule) string {
	// Decode the input so the digest is a property of the VALUE rather than of
	// its byte encoding -- key order and whitespace are not differences. Input
	// that is not valid JSON is carried through as a string, which is what
	// IdempotencyInputDigest does with it too.
	var input any
	raw := scheduleInputOrDefault(sch.Input)
	if err := json.Unmarshal(raw, &input); err != nil {
		input = string(raw)
	}

	// json.Marshal sorts map keys, so this is canonical without a sort here.
	canon, err := json.Marshal(map[string]any{
		"name":        sch.Name,
		"def_name":    sch.DefName,
		"entry_point": sch.EntryPoint,
		"cron":        sch.CronExpression,
		"input":       input,
		// DELIBERATELY STILL "enabled", AND DELIBERATELY STILL A BOOLEAN,
		// after cleat#1702 replaced the column with `disabled_at`.
		//
		// This digest is PERSISTED, in workflow_schedules.request_digest, and
		// compared against the digest of a later request carrying the same
		// idempotency key. Renaming the key or changing the value's shape
		// changes every digest, so after an upgrade a replayed create would be
		// refused as a payload mismatch against a row it agrees with entirely.
		//
		// Digesting the timestamp instead would be wrong even on a fresh
		// database: two requests that disable the same schedule differ only in
		// WHEN, which is exactly the difference idempotency exists to ignore.
		// Liveness is the property the request expresses; the instant is not.
		"enabled":        !sch.Disabled(),
		"timezone":       scheduleTimezoneOrDefault(sch.Timezone),
		"misfire_policy": MisfirePolicyOrDefault(sch.MisfirePolicy),
		"catch_up_limit": CatchUpLimitOrDefault(sch.CatchUpLimit),
		"overlap_policy": OverlapPolicyOrDefault(sch.OverlapPolicy),
	})
	if err != nil {
		// Unreachable for this map, whose values are all marshalable. Falling
		// back to the raw input rather than to "" keeps two different requests
		// from digesting equal if it ever becomes reachable.
		return IdempotencyInputDigest(raw)
	}
	return IdempotencyInputDigest(canon)
}

// nullableScheduleKey binds an absent key as NULL rather than as "".
//
// The unique index is over (tenant_id, idempotency_key), and on PostgreSQL and
// MySQL distinct NULLs never collide while distinct ""s are all the SAME value.
// Binding the empty string would therefore make the index mean "at most one
// keyless schedule per tenant" -- so the second schedule created without any
// Idempotency-Key at all would be refused, and the error would name an
// idempotency constraint to a caller that never sent a key.
func nullableScheduleKey(key string) any {
	if key == "" {
		return nil
	}
	return key
}
