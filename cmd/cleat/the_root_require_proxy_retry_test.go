package main

// cleat#4264: TestTheRootRequireNamesAPublishedVersion made ONE un-retried call to proxy.golang.org, and a single timeout failed a merge group that held four
// PRs. The call now retries what is not an answer (a transport error, a 5xx) and nothing else. These tests pin both halves, against a stub server and
// with no network, because "it passes when the proxy is slow" is satisfied by a test that can no longer fail:
//
//   - the KNOWN-POSITIVE: a definite "this version does not exist" (404, 410) is FINAL, is not retried, and still fails with the message that stops the
//     require which cleat#2452 got wrong;
//   - the KNOWN-NEGATIVE: a timeout, or a 5xx, followed by a 200 passes;
//   - and a proxy that never answers still FAILS once the bounded attempts run out. It is never a skip.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubProxy serves the scripted statuses in order, one per request; a status of 0 stalls past the client's timeout (a proxy that never sends headers).
// After the script it repeats the last entry. It reports how many requests it saw.
func stubProxy(t *testing.T, script ...int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		if i >= len(script) {
			i = len(script) - 1
		}
		if script[i] == 0 {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(script[i])
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// ask runs askProxy against the stub with a client that times out fast and pauses that are recorded, not slept.
func ask(t *testing.T, srv *httptest.Server, pauses ...time.Duration) (status, attempts int, err error, slept []time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 60 * time.Millisecond}
	status, attempts, err = askProxy(client, srv.URL+"/x.info", pauses, func(d time.Duration) { slept = append(slept, d) })
	return status, attempts, err, slept
}

var twoPauses = []time.Duration{time.Millisecond, 2 * time.Millisecond}

// The known-positive. A 404 or a 410 is the proxy saying the version is UNKNOWN: it is the answer, so it is never retried (one request), and it FAILS.
func TestADefiniteUnknownVersionIsFinalNotRetriedAndStillFails(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		srv, seen := stubProxy(t, code, http.StatusOK) // a 200 queued behind it: a retry would find it and pass, which is the defect
		status, attempts, err, slept := ask(t, srv, twoPauses...)
		if err != nil || status != code || attempts != 1 || seen.Load() != 1 || len(slept) != 0 {
			t.Fatalf("HTTP %d: status %d, attempts %d, requests %d, err %v, slept %v; want it returned at once after ONE request", code, status, attempts, seen.Load(), err, slept)
		}
		msg := proxyVerdict("v9.9.9", "https://example.invalid/x.info", status, attempts, err)
		if msg == "" {
			t.Fatalf("HTTP %d passed: a version the proxy does not serve must FAIL (this is the cleat#2452 hole)", code)
		}
		if !strings.Contains(msg, "does not serve it") || !strings.Contains(msg, "ALREADY PUBLISHED") {
			t.Errorf("HTTP %d failed with %q, want the message that explains the published-version rule", code, msg)
		}
	}
}

// Any other definite status below 500 is also final and fails: it says something about the request, not about the proxy's health.
func TestAnotherDefiniteStatusBelow500IsFinalAndFails(t *testing.T) {
	srv, seen := stubProxy(t, http.StatusForbidden, http.StatusOK)
	status, attempts, err, _ := ask(t, srv, twoPauses...)
	if err != nil || status != http.StatusForbidden || attempts != 1 || seen.Load() != 1 {
		t.Fatalf("status %d, attempts %d, requests %d, err %v; want 403 after one request", status, attempts, seen.Load(), err)
	}
	if proxyVerdict("v9.9.9", "u", status, attempts, err) == "" {
		t.Error("a 403 passed")
	}
}

// The known-negative: a timeout (no answer at all) and then a 200 passes, after exactly one pause.
func TestATimeoutThenASuccessPasses(t *testing.T) {
	srv, seen := stubProxy(t, 0, http.StatusOK)
	status, attempts, err, slept := ask(t, srv, twoPauses...)
	if err != nil || status != http.StatusOK || attempts != 2 || seen.Load() != 2 {
		t.Fatalf("status %d, attempts %d, requests %d, err %v; want 200 on the second attempt", status, attempts, seen.Load(), err)
	}
	if len(slept) != 1 || slept[0] != twoPauses[0] {
		t.Errorf("paused %v, want exactly the first pause %v before the retry", slept, twoPauses[0])
	}
	if msg := proxyVerdict("v9.9.9", "u", status, attempts, err); msg != "" {
		t.Errorf("a timeout followed by a 200 failed: %q", msg)
	}
}

// 5xx is the proxy saying it could not answer: retried, and the third attempt can succeed.
func TestServerErrorsAreRetriedUntilTheProxyAnswers(t *testing.T) {
	srv, seen := stubProxy(t, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusOK)
	status, attempts, err, slept := ask(t, srv, twoPauses...)
	if err != nil || status != http.StatusOK || attempts != 3 || seen.Load() != 3 || len(slept) != 2 {
		t.Fatalf("status %d, attempts %d, requests %d, err %v, slept %v; want 200 on the third attempt after two pauses", status, attempts, seen.Load(), err, slept)
	}
}

// A proxy that NEVER answers is bounded and FAILS: never a skip, never a pass. The first and the last cause are both reported when they differ.
func TestAProxyThatNeverAnswersFailsAfterTheBoundedAttempts(t *testing.T) {
	for name, script := range map[string][]int{
		"always a timeout": {0},
		"always a 503":     {http.StatusServiceUnavailable},
		"a 502 then 503":   {http.StatusBadGateway, http.StatusServiceUnavailable},
	} {
		srv, seen := stubProxy(t, script...)
		status, attempts, err, slept := ask(t, srv, twoPauses...)
		if err == nil || status != 0 {
			t.Fatalf("%s: status %d, err %v; want no definite answer and an error", name, status, err)
		}
		if attempts != len(twoPauses)+1 || seen.Load() != int64(attempts) || len(slept) != len(twoPauses) {
			t.Errorf("%s: attempts %d, requests %d, pauses %v; want exactly %d attempts (the bound) and %d pauses", name, attempts, seen.Load(), slept, len(twoPauses)+1, len(twoPauses))
		}
		msg := proxyVerdict("v9.9.9", "u", status, attempts, err)
		if msg == "" {
			t.Fatalf("%s passed: an unreachable proxy must FAIL the check, not skip it", name)
		}
		if !strings.Contains(msg, "could not ask the module proxy") || !strings.Contains(msg, "after 3 attempts") {
			t.Errorf("%s failed with %q, want the 'could not ask ... after 3 attempts' message", name, msg)
		}
	}
	// the two causes differ: both are named, the first one is not lost
	srv, _ := stubProxy(t, http.StatusBadGateway, http.StatusServiceUnavailable)
	_, _, err, _ := ask(t, srv, twoPauses...)
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") || !strings.Contains(err.Error(), "first attempt: HTTP 502") {
		t.Errorf("error %v, want the last cause (503) with the first (502) named", err)
	}
}

// A 200 on the first attempt asks once and never pauses.
func TestAnImmediateSuccessAsksOnce(t *testing.T) {
	srv, seen := stubProxy(t, http.StatusOK)
	status, attempts, err, slept := ask(t, srv, twoPauses...)
	if err != nil || status != http.StatusOK || attempts != 1 || seen.Load() != 1 || len(slept) != 0 {
		t.Fatalf("status %d, attempts %d, requests %d, err %v, slept %v; want one request and no pause", status, attempts, seen.Load(), err, slept)
	}
	if msg := proxyVerdict("v9.9.9", "u", status, attempts, err); msg != "" {
		t.Errorf("a 200 failed: %q", msg)
	}
}

// The production bound is the one the comment on proxyAttemptTimeout states: three attempts, 20s each, paused 2s and 5s, so at most 67s.
func TestTheProductionRetryBoundIsWhatItsCommentSays(t *testing.T) {
	var pauses time.Duration
	for _, p := range proxyRetryPauses {
		pauses += p
	}
	worst := time.Duration(len(proxyRetryPauses)+1)*proxyAttemptTimeout + pauses
	if len(proxyRetryPauses)+1 != 3 || worst != 67*time.Second {
		t.Errorf("%d attempts, worst case %v; the comment on proxyAttemptTimeout says 3 attempts and 67s", len(proxyRetryPauses)+1, worst)
	}
}
