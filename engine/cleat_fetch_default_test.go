package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/internal/tenantctx"
)

// cleat#3377. A cleat-worker installs a Fetcher only when started with
// --enable-workflow-fetch, so the engine-side contract is two things: the
// no-Fetcher failure tells the reader which flag turns the call on, and a
// replayed fetch returns the recorded response without asking any Fetcher.

func TestCleatFetchWithoutAFetcherNamesTheFlagThatEnablesIt(t *testing.T) {
	s := newTestExecSession() // no Fetcher

	result := s.Fetch(context.Background(), nil, "GET", "https://example.invalid/", "{}", "", 0, 0)

	if byte(result) != 1 {
		t.Fatalf("UNMEASURED: errCode = %d, want 1, so the failure branch was not taken and the "+
			"message below is about nothing", byte(result))
	}
	if len(s.history) != 1 {
		t.Fatalf("UNMEASURED: %d events recorded, want 1", len(s.history))
	}
	got := s.history[0].Err
	for _, want := range []string{"--enable-workflow-fetch", "engine.WithFetcher"} {
		if !strings.Contains(got, want) {
			t.Errorf("the no-Fetcher error does not name %q:\n  %s", want, got)
		}
	}
	// The wording this replaced told a worker operator there was nothing to do.
	if strings.Contains(got, "cannot serve it") {
		t.Errorf("the error still says a stock worker cannot serve cleat_fetch:\n  %s", got)
	}
}

// failingFetcher fails the test if it is ever asked.
type failingFetcher struct{ t *testing.T }

func (f failingFetcher) Fetch(context.Context, string, string, string, string) (string, error) {
	f.t.Helper()
	f.t.Error("a replayed cleat_fetch went back to the Fetcher; the recorded response is the only " +
		"answer a replay may give")
	return "", nil
}

func TestAReplayedCleatFetchGivesTheRecordedResponseAndNeverAsksTheFetcher(t *testing.T) {
	s := newTestExecSession()
	s.engine.fetcher = failingFetcher{t}
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeFetch,
		FetchMethod: "GET", FetchURL: "https://example.invalid/",
		FetchResponse: `{"status":200,"headers":{},"body":"recorded"}`,
	}}

	result := s.Fetch(context.Background(), nil, "GET", "https://example.invalid/", "{}", "", 0, 0)

	if byte(result) != 0 {
		t.Errorf("errCode = %d, want 0 (the recorded response replays)", byte(result))
	}
	if s.stepCount != 1 {
		t.Errorf("stepCount = %d, want 1; the replayed fetch did not consume its step", s.stepCount)
	}
}

// tenantRecordingFetcher records which tenant, if any, the Fetcher was handed.
type tenantRecordingFetcher struct {
	got   uuid.UUID
	found bool
}

func (f *tenantRecordingFetcher) Fetch(ctx context.Context, _, _, _, _ string) (string, error) {
	f.got, f.found = tenantctx.From(ctx)
	return `{"status":200,"headers":{},"body":""}`, nil
}

// The worker's Fetcher answers to the TENANT's egress allowlist, and that list
// is looked up from the context. Without the tenant on it the guard has no list
// and refuses every request -- which would look like the flag not working.
func TestCleatFetchHandsTheFetcherTheWorkflowsTenant(t *testing.T) {
	tenant := uuid.MustParse("0b8f1c52-6f0e-4a57-9a53-5d6f2c1d9e11")
	f := &tenantRecordingFetcher{}
	s := newTestExecSession()
	s.engine.fetcher = f
	s.tenantID = tenant.String()

	result := s.Fetch(context.Background(), nil, "GET", "https://example.invalid/", "{}", "", 0, 0)

	if byte(result) != 0 {
		t.Fatalf("UNMEASURED: errCode = %d, want 0; the Fetcher may not have been reached", byte(result))
	}
	if !f.found || f.got != tenant {
		t.Errorf("the Fetcher saw tenant (%v, found=%v), want %v", f.got, f.found, tenant)
	}
}
