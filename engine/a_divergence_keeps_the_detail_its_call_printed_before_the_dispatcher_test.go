package engine

import (
	"context"
	"os"
	"strings"
	"testing"
)

// cleat#4152, G3: a divergence message keeps the detail its call printed before the call moved onto the dispatcher.
//
// docs/troubleshooting.md shows a divergence as `expected call event, got sleep.` followed by `actual request:` and `expected request:`, and
// TestEngineReplayDivergence pins those two labels for the plain call. The dispatcher's shared message named only the step and both event types, so seven calls
// lost their detail when they moved, silently: the plain call, the retried call and the heartbeat call (the request bodies), await_child (the run ID),
// await_all_children (the run IDs), fetch (method, URL and bodies) and schedule_cron (the four arguments of both the call and the record). The sweep that found
// them read every engine commit since the dispatcher began that removed a divergence or mismatch message; every other spec's old message was the shared one.
//
// THE EXPECTED TEXT BELOW IS WRITTEN OUT LITERALLY, from the removed lines of those commits (07a461f1, 815da588, ff749553, dc8a30ca), and not built by the
// functions under test. A row that built its expectation with the same function would agree with it whatever it did.
//
// The restoration was byte-faithful when it was written: the same fields, and a body past 4096 bytes cut and followed by a sha256 of the whole. It did not
// redact, because the old messages did not. cleat#4321 decided that a divergence message must not print a body at all (a store hands back the DECRYPTED payload
// because replay acts on it, and the message goes on to the guest, the stored run error and the logs), so the fields and the labels below are as they were and
// each body is named by its size and a short hash; the expected texts here were updated for that and a_divergence_names_its_bodies_and_never_prints_them_test.go
// pins the redacted form from outside.

// g3fMessage runs a real host call against a replaying session whose history is the given records and returns what it wrote to the guest.
func g3fMessage(call func(ctx context.Context, s *execSession) int64, history []EventRecord) string {
	s := stepSession(nil, history)
	buf := make([]byte, 16384)
	call(context.WithValue(context.Background(), wasmMemBufKey{}, buf), s)
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n])
}

const g3fAdvice = "\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines)."

func TestEveryRestoredDivergenceKeepsTheDetailItsCallPrintedBefore(t *testing.T) {
	wrongType := func(rec EventRecord) []EventRecord {
		rec.Step, rec.EventType = 0, EventTypeSideEffect
		return []EventRecord{rec}
	}
	for _, tc := range []struct {
		name string
		call func(ctx context.Context, s *execSession) int64
		hist []EventRecord
		want string // the detail, then the advice, exactly as the old message ended
	}{
		{"the retried call", func(ctx context.Context, s *execSession) int64 {
			return s.DurableCallWithRetry(ctx, nil, "svc", "op", `{"a":1}`, 3, 1, 100, 1, "", 0, 16384)
		}, wrongType(EventRecord{Request: "REC-REQ"}),
			"\n  actual request: [redacted: 7 bytes, sha256=015abd7f5cc57a2d]\n  expected request: [redacted: 7 bytes, sha256=af9a34fce5d51788]" + g3fAdvice},
		{"the heartbeat call", func(ctx context.Context, s *execSession) int64 {
			return s.DurableCallWithHeartbeat(ctx, nil, "svc", "op", `{"a":1}`, 1000, 0, 16384)
		}, wrongType(EventRecord{Request: "REC-REQ"}),
			"\n  actual request: [redacted: 7 bytes, sha256=015abd7f5cc57a2d]\n  expected request: [redacted: 7 bytes, sha256=af9a34fce5d51788]" + g3fAdvice},
		{"await_child", func(ctx context.Context, s *execSession) int64 {
			return s.AwaitChild(ctx, nil, "c1", 0, 16384)
		}, wrongType(EventRecord{}),
			"\n  run ID: c1" + g3fAdvice},
		{"await_all_children", func(ctx context.Context, s *execSession) int64 {
			return s.AwaitAllChildren(ctx, nil, `["c1","c2"]`, 0, 16384)
		}, wrongType(EventRecord{Request: `["c9"]`}),
			"\n  actual run IDs: [\"c1\",\"c2\"]\n  expected run IDs: [\"c9\"]" + g3fAdvice},
		{"fetch", func(ctx context.Context, s *execSession) int64 {
			return s.Fetch(ctx, nil, "GET", "http://x", "{}", "b1", 0, 16384)
		}, wrongType(EventRecord{FetchMethod: "POST", FetchURL: "http://y", FetchBody: "b2", FetchResponse: "resp"}),
			"\n  workflow: GET http://x\n  history: POST http://y\n  actual body: [redacted: 2 bytes, sha256=7dc96f776c8423e5]\n  expected body: [redacted: 2 bytes, sha256=4814d92093ac8a0f]\n  expected response: [redacted: 4 bytes, sha256=d30db74ae503504c]" + g3fAdvice},
		{"schedule_cron", func(ctx context.Context, s *execSession) int64 {
			return s.ScheduleCron(ctx, nil, "wf", "* * * * *", "UTC", `{}`, 0, 16384)
		}, wrongType(EventRecord{CronWorkflowName: "other", CronExpr: "0 * * * *", CronTimezone: "EST", CronInput: `{"k":1}`}),
			"\n  workflow: wf \"* * * * *\" tz=\"UTC\" input=[redacted: 2 bytes, sha256=44136fa355b3678a]\n  history:  other \"0 * * * *\" tz=\"EST\" input=[redacted: 7 bytes, sha256=a0da1fce57d0e4f9]" + g3fAdvice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := g3fMessage(tc.call, tc.hist)
			if !strings.HasPrefix(got, "replay divergence at step 0: expected ") {
				t.Fatalf("PRECONDITION FAILED: not a divergence message, so this row measures nothing: %q", got)
			}
			if !strings.HasSuffix(got, tc.want) {
				t.Errorf("the divergence message does not end with the detail the call printed before it moved.\n  got:  %q\n  want it to end with: %q", got, tc.want)
			}
		})
	}
}

// (A test here used to pin that a request past 4096 bytes was cut and followed by a sha256 of the whole. A body is no longer printed at all, cut or whole
// (cleat#4321); TestRedactedBodyIsItsSizeAndAShortHashAndNothingElse pins a 5000-byte body named by its whole size and hash.)

// The dispatcher puts a spec's detail between the step line and the advice for BOTH kinds of divergence (a record of the wrong type, and one that fails the
// spec's Identity), and a spec with no detail reads exactly as the shared message always has.
func TestTheDispatcherPlacesASpecsDetailAndLeavesASpecWithoutOneAlone(t *testing.T) {
	detail := func(a stepArgs, rec *EventRecord) string {
		return "\n  asked: " + a.s1 + "\n  recorded: " + rec.Request
	}
	build := func(withDetail bool, identity bool) *stepSpec {
		var ran int32
		sp := fakeSpec(effectEffect, recordsOne, &ran, recordOneFake)
		if withDetail {
			sp.DivergenceDetail, sp.NoDivergenceDetail = detail, ""
		}
		if identity {
			sp.Identity, sp.NoIdentity = func(stepArgs, *EventRecord) string { return "different thing" }, ""
		}
		return sp
	}
	run := func(sp *stepSpec, rec EventRecord) string {
		return g3fMessage(func(ctx context.Context, s *execSession) int64 {
			return s.step(ctx, nil, sp, stepArgs{s1: "ASKED", outPtr: 0, outMaxLen: 16384})
		}, []EventRecord{rec})
	}
	wrong := EventRecord{Step: 0, EventType: EventTypeSideEffect, Request: "REC"}
	right := EventRecord{Step: 0, EventType: fakeEvent, Request: "REC"}

	if got, want := run(build(true, false), wrong), "replay divergence at step 0: expected "+string(fakeEvent)+", got side_effect.\n  asked: ASKED\n  recorded: REC"+g3fAdvice; got != want {
		t.Errorf("type divergence with detail:\n  got:  %q\n  want: %q", got, want)
	}
	if got, want := run(build(true, true), right), "replay divergence at step 0: fake asked for something other than the "+string(fakeEvent)+" recorded there: different thing.\n  asked: ASKED\n  recorded: REC"+g3fAdvice; got != want {
		t.Errorf("identity divergence with detail:\n  got:  %q\n  want: %q", got, want)
	}
	// A spec with no detail: the message is the shared builder's, byte for byte.
	if got, want := run(build(false, false), wrong), divergenceMessageWith(&wrong, fakeEvent, ""); got != want {
		t.Errorf("type divergence without detail changed:\n  got:  %q\n  want: %q", got, want)
	}
	if got, want := run(build(false, true), right), "replay divergence at step 0: fake asked for something other than the "+string(fakeEvent)+" recorded there: different thing."+g3fAdvice; got != want {
		t.Errorf("identity divergence without detail changed:\n  got:  %q\n  want: %q", got, want)
	}
}

// The rule itself: exactly one of DivergenceDetail and NoDivergenceDetail, so a call cannot be moved without saying what its divergence carries. Each fixture
// reaches this rule and no other.
func TestADivergenceDetailOrAReasonForNoneIsRequired(t *testing.T) {
	mk := func(detail func(stepArgs, *EventRecord) string, reason string) *stepSpec {
		var ran int32
		sp := fakeSpec(effectEffect, recordsOne, &ran, recordOneFake)
		sp.DivergenceDetail, sp.NoDivergenceDetail = detail, reason
		return sp
	}
	fn := func(stepArgs, *EventRecord) string { return "" }
	for _, tc := range []struct {
		name string
		sp   *stepSpec
		want string
	}{
		{"a detail", mk(fn, ""), ""},
		{"a reason for none", mk(nil, "its old message carried nothing"), ""},
		{"neither", mk(nil, ""), "must set exactly one of DivergenceDetail and NoDivergenceDetail"},
		{"both", mk(fn, "and a reason"), "must set exactly one of DivergenceDetail and NoDivergenceDetail"},
	} {
		err := tc.sp.validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: a well-formed spec was refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

// THE PIN: these specs have a detail function (they are the seven calls that lost one), and every other registered spec says why it has none, citing the
// sweep. A spec moved from one list to the other changes this test in the same diff, where a reviewer reads the reason against the old message.
func TestExactlyTheSpecsThatPrintedDetailBeforeStillHaveIt(t *testing.T) {
	// call is here since the plain call moved onto the dispatcher (its detail is requestDivergenceDetail, and TestEngineReplayDivergence pins the labels).
	want := map[string]bool{"call": true, "plugin_call": true, "call_with_retry": true, "call_with_heartbeat": true, "await_child": true, "await_all_children": true, "fetch": true, "schedule_cron": true}
	got := map[string]bool{}
	for _, sp := range stepSpecs {
		if sp.DivergenceDetail != nil {
			got[sp.Name] = true
			continue
		}
		if !strings.HasSuffix(sp.NoDivergenceDetail, divergenceSweepNote) {
			t.Errorf("spec %q has no detail and a reason that does not cite the sweep: %q", sp.Name, sp.NoDivergenceDetail)
		}
	}
	if len(stepSpecs) < 20 {
		t.Fatalf("PRECONDITION FAILED: %d registered specs; the scan is not reading the registry", len(stepSpecs))
	}
	for n := range want {
		if !got[n] {
			t.Errorf("spec %q printed detail before it moved and has none now", n)
		}
	}
	for n := range got {
		if !want[n] {
			t.Errorf("spec %q has a detail function and is not in this test's list; add it with the old message it restores", n)
		}
	}
}

// g3fRedactedA is `{"a":1}` as a divergence message names it (cleat#4321); size and hash computed outside Go.
const g3fRedactedA = "[redacted: 7 bytes, sha256=015abd7f5cc57a2d]"

// The detail is applied at EVERY site the dispatcher reports a divergence from, not only the first record of a run: the first row above reaches the wrong-typed
// first record, and a divergence reported from the identity check, from the middle of a run or from a foreign record after the prefix is a different line of the
// dispatcher. Each row names the site it reaches and asserts the marker of the call's detail; without these a mutant that drops the detail at one site stays green.
func TestRestoredDetailIsAppliedAtEveryDivergenceSite(t *testing.T) {
	retry := func(ctx context.Context, s *execSession) int64 {
		return s.DurableCallWithRetry(ctx, nil, "svc", "op", `{"a":1}`, 3, 1, 100, 1, "", 0, 16384)
	}
	fetch := func(ctx context.Context, s *execSession) int64 {
		return s.Fetch(ctx, nil, "GET", "http://x", "{}", "b1", 0, 16384)
	}
	failed := func(op string) EventRecord {
		return EventRecord{Step: 0, EventType: EventTypeCallAttemptFailed, Service: "svc", Op: op, Request: "REC-REQ"}
	}
	foreign := EventRecord{Step: 1, EventType: EventTypeSideEffect, Request: "REC-REQ"}
	for _, tc := range []struct {
		name   string
		call   func(ctx context.Context, s *execSession) int64
		hist   []EventRecord
		site   string
		marker string
	}{
		{"single record, a record of the wrong type", fetch, []EventRecord{{Step: 0, EventType: EventTypeSideEffect, FetchURL: "http://y"}},
			"step: the type check", "history: "},
		{"single record, the right type naming another URL", fetch, []EventRecord{{Step: 0, EventType: EventTypeFetch, FetchMethod: "GET", FetchURL: "http://y", FetchBody: "b1"}},
			"step: the Identity check", "history: GET http://y"},
		{"a run whose first record is foreign", retry, []EventRecord{{Step: 0, EventType: EventTypeSideEffect, Request: "REC-REQ"}},
			"run: no record of the run at all", "actual request: " + g3fRedactedA},
		{"a run whose record names another operation", retry, []EventRecord{failed("another")},
			"run: Identity on a record of the run", "actual request: " + g3fRedactedA},
		{"a run of attempts followed by a foreign record", retry, []EventRecord{failed("op"), foreign},
			"run: a foreign record after the prefix", "actual request: " + g3fRedactedA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := g3fMessage(tc.call, tc.hist)
			if !strings.HasPrefix(got, "replay divergence at step ") {
				t.Fatalf("PRECONDITION FAILED: this row did not reach a divergence (%s), so it measures nothing: %q", tc.site, got)
			}
			if !strings.Contains(got, tc.marker) {
				t.Errorf("the divergence reported from %s carries no detail (want %q): %q", tc.site, tc.marker, got)
			}
		})
	}
}

// docs/troubleshooting.md shows a divergence with `actual request:` and `expected request:` lines under it. This reads the labels OUT OF THE DOC and requires
// the real message of the calls that carry a request to contain each, so the page and the code cannot drift apart again without a test saying so. A doc whose
// example has no such lines (the section was reworded) is UNMEASURED, not a pass.
func TestTheTroubleshootingDivergenceExampleIsStillWhatTheCallsPrint(t *testing.T) {
	doc, err := os.ReadFile("../docs/troubleshooting.md")
	if err != nil {
		t.Fatalf("UNMEASURED: cannot read docs/troubleshooting.md: %v", err)
	}
	var labels, exampleLines []string
	firstLine := ""
	inSection := false
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.HasPrefix(line, "## ") {
			inSection = strings.Contains(line, "Replay Divergence")
		}
		if inSection && firstLine == "" && strings.HasPrefix(line, "replay divergence at step ") {
			firstLine = line
		}
		// the two lines of the example, `  actual request: [redacted: ...]` and `  expected request: [redacted: ...]` (cleat#4321: a body is named, never printed)
		if t := strings.TrimSpace(line); inSection && (strings.HasPrefix(t, "actual request: ") || strings.HasPrefix(t, "expected request: ")) {
			labels = append(labels, t[:strings.Index(t, "request: ")+len("request: ")])
			exampleLines = append(exampleLines, "  "+t)
		}
	}
	if len(labels) != 2 {
		t.Fatalf("UNMEASURED: the Replay Divergence section of docs/troubleshooting.md shows %d `request:` lines, want the two this test reads (`actual request:` and `expected request:`); the section was reworded, and this test must be told what it shows now: %q", len(labels), labels)
	}
	if firstLine == "" {
		t.Fatalf("UNMEASURED: the Replay Divergence section of docs/troubleshooting.md has no line starting `replay divergence at step `")
	}
	// The page's example is the plain call meeting a sleep, and its two request lines are what that call prints for the example's two bodies, word for word
	// (the bodies are the ones the page's prose is about: the same order, a different amount). A page that showed a body, or a format the code no longer
	// prints, fails here.
	example := g3fMessage(func(ctx context.Context, s *execSession) int64 {
		return s.DurableCall(ctx, nil, "svc", "op", `{"order_id":"o-1042","amount":2500}`, 0, 16384)
	}, []EventRecord{{Step: 0, EventType: EventTypeSleep, Request: `{"order_id":"o-1042","amount":2750}`}})
	if lines := strings.Split(example, "\n"); len(lines) < 3 || lines[1] != exampleLines[0] || lines[2] != exampleLines[1] {
		t.Errorf("docs/troubleshooting.md shows %q and the plain call prints %q for the page's example bodies", exampleLines, example)
	}
	// The page's own example is the plain call meeting a sleep: its FIRST line must be what the call prints, word for word.
	plain := g3fMessage(func(ctx context.Context, s *execSession) int64 {
		return s.DurableCall(ctx, nil, "svc", "op", `{}`, 0, 16384)
	}, []EventRecord{{Step: 0, EventType: EventTypeSleep, Request: "r"}})
	if got := strings.SplitN(plain, "\n", 2)[0]; got != firstLine {
		t.Errorf("docs/troubleshooting.md shows %q and the plain call prints %q", firstLine, got)
	}
	for _, c := range []struct {
		name string
		call func(ctx context.Context, s *execSession) int64
	}{
		{"the retried call", func(ctx context.Context, s *execSession) int64 {
			return s.DurableCallWithRetry(ctx, nil, "svc", "op", `{}`, 3, 1, 100, 1, "", 0, 16384)
		}},
		{"the heartbeat call", func(ctx context.Context, s *execSession) int64 {
			return s.DurableCallWithHeartbeat(ctx, nil, "svc", "op", `{}`, 1000, 0, 16384)
		}},
	} {
		got := g3fMessage(c.call, []EventRecord{{Step: 0, EventType: EventTypeSideEffect, Request: "r"}})
		for _, l := range labels {
			if !strings.Contains(got, "\n  "+l) {
				t.Errorf("%s: docs/troubleshooting.md shows %q under a divergence and the call's message does not carry it: %q", c.name, l, got)
			}
		}
	}
}

// The sixth site, a record whose Role is none of the three, is DEFENSIVE: no registered spec returns such a value, so no real call can reach it and only a fake
// spec can. It was the one site with no mutant failing (the detail dropped there left every test green) until this row.
func TestRestoredDetailIsAppliedWhenARunRecordHasAnUnknownRole(t *testing.T) {
	var ran int32
	var dec, pre []EventRecord
	sp := runSpec(&ran, &dec, &pre)
	sp.NoDivergenceDetail = ""
	sp.DivergenceDetail = func(a stepArgs, rec *EventRecord) string {
		return "\n  asked: " + a.s1 + "\n  recorded: " + rec.ScopeKey
	}
	sp.Role = func(a stepArgs, rec *EventRecord) recordRole {
		if rec.EventType == runPrefixEvent {
			return rolePrefix
		}
		return recordRole(99)
	}
	got := g3fMessage(func(ctx context.Context, s *execSession) int64 {
		return s.step(ctx, nil, sp, stepArgs{s1: "ASKED", outPtr: 0, outMaxLen: 16384})
	}, []EventRecord{
		{Step: 0, EventType: runPrefixEvent, ScopeKey: "ASKED"},
		{Step: 1, EventType: EventTypeSideEffect, ScopeKey: "UNKNOWN-ROLE-RECORD"},
	})
	if !strings.HasPrefix(got, "replay divergence at step 1: ") {
		t.Fatalf("PRECONDITION FAILED: the unknown-role record did not produce a divergence at step 1, so this row measures nothing: %q", got)
	}
	if !strings.Contains(got, "\n  asked: ASKED\n  recorded: UNKNOWN-ROLE-RECORD") {
		t.Errorf("a divergence reported for a record of unknown role carries no detail: %q", got)
	}
}
