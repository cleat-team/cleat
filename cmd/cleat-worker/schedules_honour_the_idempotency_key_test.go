package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// POST /api/schedules honours Idempotency-Key. cleat#1495.
//
// It was the one work-creating endpoint outside cleat#1169's replay policy: it
// could not tell a retried request from a genuine name collision, because both
// arrive as a second create under a taken name, and it answered
// `409 schedule_exists` to both.
//
// keyedScheduleStore is the piece that makes these tests able to fail. The
// mockStore used by the other schedule tests records that CreateSchedule was
// called and discards the Schedule, so a handler that never reads the header
// passes them unchanged -- exactly the defect cleat#1167 documents for
// TestHandleDeadLetterReprocess_AlreadyExisted, whose double ignored its key
// argument and therefore asserted a branch that was unreachable.
//
// This one models what the store actually guarantees: a key that has been seen
// with the same request replays, a key seen with a DIFFERENT request is a
// mismatch, and a key is never inferred from anything else. It records every
// key it was handed, so a handler that drops the header fails these tests
// rather than sailing through them.
type keyedScheduleStore struct {
	*mockStore
	byKey map[string]engine.Schedule
	// names is separate from byKey because the real table has the NAME as its
	// primary key, independently of any idempotency key. Tracking names only
	// inside byKey would leave a keyless create invisible, so a second keyless
	// create under the same name would be accepted -- which the real store
	// refuses, and which made an earlier version of this double disagree with
	// the database it stands in for.
	names map[string]bool
	seen  []string // every key the handler passed down, in order
	made  int
}

func newKeyedScheduleStore() *keyedScheduleStore {
	k := &keyedScheduleStore{
		mockStore: &mockStore{},
		byKey:     map[string]engine.Schedule{},
		names:     map[string]bool{},
	}
	k.mockStore.createScheduleFn = k.create
	return k
}

// sameRequest compares the fields that decide what the schedule IS.
//
// NextRunAt is excluded because the handler recomputes it from the clock on
// every request, so two identical retries always disagree on it. That is the
// same exclusion engine.scheduleRequestDigest makes, and it is modelled here
// rather than ignored: a double that compared everything would report every
// retry as a mismatch and these tests would assert the feature is broken.
func sameRequest(a, b engine.Schedule) bool {
	// NextRunAt is simply not in the comparison below. An earlier version
	// normalised it away first, which `go vet` correctly reported as a
	// self-assignment: neutralising a field that is never read is a statement
	// about intent dressed as code, and the comment above is the right place
	// for it.
	return a.Name == b.Name && a.DefName == b.DefName && a.EntryPoint == b.EntryPoint &&
		a.CronExpression == b.CronExpression && string(a.Input) == string(b.Input) &&
		a.Timezone == b.Timezone && a.MisfirePolicy == b.MisfirePolicy &&
		a.CatchUpLimit == b.CatchUpLimit && a.OverlapPolicy == b.OverlapPolicy
}

func (k *keyedScheduleStore) create(_ context.Context, s engine.Schedule) error {
	k.seen = append(k.seen, s.IdempotencyKey)

	// An empty key is the ABSENCE of a token, not a token whose value is "".
	// Two callers who both sent nothing must never deduplicate against each
	// other -- which is the bug the NULL binding in the real store prevents.
	if s.IdempotencyKey != "" {
		if first, ok := k.byKey[s.IdempotencyKey]; ok {
			if sameRequest(first, s) {
				return engine.ErrScheduleIdempotentReplay
			}
			return fmt.Errorf("%w: differs from the request this key created",
				engine.ErrIdempotencyKeyInputMismatch)
		}
	}
	if k.names[s.Name] {
		return fmt.Errorf("%w: %s", engine.ErrScheduleExists, s.Name)
	}
	k.names[s.Name] = true
	k.made++
	if s.IdempotencyKey != "" {
		k.byKey[s.IdempotencyKey] = s
	}
	return nil
}

func postSchedule(t *testing.T, api *apiServer, key, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/schedules", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	api.handleSchedulesList(w, req)
	var decoded map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response body is not JSON (%d): %s", w.Code, w.Body.String())
	}
	return w.Code, decoded
}

func TestSchedulesHonourTheIdempotencyKey(t *testing.T) {
	const body = `{"name":"nightly","cron":"0 3 * * *","def_name":"w","input":{"region":"eu"}}`

	t.Run("a first keyed request is an ordinary create", func(t *testing.T) {
		ks := newKeyedScheduleStore()
		api := newTestAPIServer(ks.mockStore)

		code, got := postSchedule(t, api, "K", body)
		if code != 201 {
			t.Fatalf("status = %d, want 201: %v", code, got)
		}
		if got["status"] != "created" {
			t.Errorf("status field = %v, want \"created\" -- the original response shape "+
				"is part of the policy and must not change", got["status"])
		}
		// PRESENT AND FALSE, not absent. An absent flag cannot be told from an
		// old server that does not send one, so the cautious client most likely
		// to check it is the one it fails.
		if replay, ok := got["idempotent_replay"]; !ok || replay != false {
			t.Errorf("idempotent_replay = %v (present=%v), want false", replay, ok)
		}
		// THE HANDLER MUST HAVE PASSED THE KEY DOWN. Without this assertion,
		// deleting the header read leaves every other check in this file green.
		if len(ks.seen) != 1 || ks.seen[0] != "K" {
			t.Errorf("the store was handed keys %q, want exactly [\"K\"]. The handler is "+
				"not reading Idempotency-Key, so nothing below can deduplicate.", ks.seen)
		}
	})

	t.Run("the same request again replays", func(t *testing.T) {
		ks := newKeyedScheduleStore()
		api := newTestAPIServer(ks.mockStore)

		if code, got := postSchedule(t, api, "K", body); code != 201 {
			t.Fatalf("first create: %d %v", code, got)
		}
		code, got := postSchedule(t, api, "K", body)
		if code != 201 {
			t.Fatalf("a retry returned %d, want 201: %v\n\nThis is the defect: it was 409 "+
				"schedule_exists, which tells the caller somebody else's name is in the "+
				"way when in fact it is its own.", code, got)
		}
		if got["status"] != "created" {
			t.Errorf("status = %v, want \"created\": a replay returns THE ORIGINAL "+
				"response, not a different one", got["status"])
		}
		if got["idempotent_replay"] != true {
			t.Errorf("idempotent_replay = %v, want true", got["idempotent_replay"])
		}
		if ks.made != 1 {
			t.Errorf("the store created %d schedules, want 1 -- a replay must create "+
				"nothing", ks.made)
		}
	})

	t.Run("the same key with a different request is refused", func(t *testing.T) {
		ks := newKeyedScheduleStore()
		api := newTestAPIServer(ks.mockStore)

		if code, _ := postSchedule(t, api, "K", body); code != 201 {
			t.Fatal("first create did not succeed")
		}
		const different = `{"name":"nightly","cron":"0 4 * * *","def_name":"w","input":{"region":"eu"}}`
		code, got := postSchedule(t, api, "K", different)
		if code != 409 {
			t.Fatalf("reusing a key with a different request returned %d, want 409: %v", code, got)
		}
		if got["detail"] != "idempotency_key_input_mismatch" {
			t.Errorf("detail = %v, want \"idempotency_key_input_mismatch\" -- the same "+
				"string the start handler uses, so a client branching on it does not "+
				"need to know which endpoint it called", got["detail"])
		}
	})

	t.Run("a genuine name collision still says so", func(t *testing.T) {
		ks := newKeyedScheduleStore()
		api := newTestAPIServer(ks.mockStore)

		if code, _ := postSchedule(t, api, "K", body); code != 201 {
			t.Fatal("first create did not succeed")
		}
		// A DIFFERENT caller, a different key, the same name.
		code, got := postSchedule(t, api, "OTHER", body)
		if code != 409 {
			t.Fatalf("a different key under a taken name returned %d, want 409: %v", code, got)
		}
		if got["detail"] != "schedule_exists" {
			t.Errorf("detail = %v, want \"schedule_exists\". cleat#1495 does not reverse "+
				"ErrScheduleExists; it relieves it of answering a second question.",
				got["detail"])
		}
	})

	t.Run("a keyless request behaves exactly as before", func(t *testing.T) {
		ks := newKeyedScheduleStore()
		api := newTestAPIServer(ks.mockStore)

		code, got := postSchedule(t, api, "", body)
		if code != 201 {
			t.Fatalf("keyless create returned %d, want 201: %v", code, got)
		}
		// The flag is present even here. A caller that sends no key still gets
		// an unambiguous answer about what happened.
		if got["idempotent_replay"] != false {
			t.Errorf("idempotent_replay = %v, want false", got["idempotent_replay"])
		}
		if len(ks.seen) != 1 || ks.seen[0] != "" {
			t.Errorf("the store was handed keys %q; a request with no header must pass "+
				"the empty string, not a value invented by the handler", ks.seen)
		}
		// And a keyless duplicate is a collision, not a replay: there is
		// nothing to replay against.
		code, got = postSchedule(t, api, "", body)
		if code != 409 || got["detail"] != "schedule_exists" {
			t.Errorf("a keyless duplicate returned %d %v, want 409 schedule_exists", code, got)
		}
	})
}
