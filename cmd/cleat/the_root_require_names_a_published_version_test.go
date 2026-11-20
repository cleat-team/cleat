package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTheRootRequireNamesAPublishedVersion pins the property cleat#2452 broke:
// `cleat/go.mod`'s require of the root module must name a version that EXISTS.
//
// The file states the rule itself, at its own comment: the require is "at the
// CURRENT published root version, not a placeholder". A version that has not
// been tagged yet is a placeholder in every sense that matters -- a consumer
// resolving it gets `unknown revision`, the failure cleat#1888 recorded for
// v0.0.0 and cleat#2452 reintroduced with v0.3.0.
//
// IT READS THIS CHECKOUT'S FILE, NOT THE DEFAULT BRANCH'S, and that is what
// makes it able to stop the change that breaks the rule. The template tests
// used to resolve the SDK through the module proxy, whose @latest for an
// untagged module is a pseudo-version of the DEFAULT BRANCH -- so a bad require
// failed there on the commit AFTER the one that introduced it, and the
// introducing PR stayed green. cleat#2452 is the worked example: green itself,
// red for the two commits following it, and not gatable by the PR that caused
// it. (The past tense is deliberate, and it has been earned twice in one day:
// the sentence used to be present tense while the paragraph below said those
// same tests built against this checkout -- cleat#3078 -- and the helper that
// made them do so was itself removed in cleat#3083, once the tag removed the
// pseudo-version this file is about.)
//
// IT ALSO CARRIED WHAT A CHANGE GAVE UP, AND THAT HAS SINCE BEEN GIVEN BACK.
// For a while the template tests built against this checkout rather than the
// proxy, because a pull request cannot validate a property that depends on the
// default branch's content. They no longer do -- cleat#3083 removed that helper,
// once the submodule was tagged and `@latest` stopped being a moving head -- so
// a bad require fails in the template tests again. This test is kept because it
// fails EARLIER and reads this checkout's own cleat/go.mod rather than a pushed
// head, not because nothing else would catch the version any more.
func TestTheRootRequireNamesAPublishedVersion(t *testing.T) {
	mod := filepath.Join(repoRoot(t), "cleat", "go.mod")
	data, err := os.ReadFile(mod)
	if err != nil {
		t.Fatalf("read cleat/go.mod: %v", err)
	}

	re := regexp.MustCompile(`(?m)^\s*(?:require\s+)?github\.com/cleat-team/cleat\s+(v\S+)`)
	m := re.FindSubmatch(data)
	if m == nil {
		t.Fatalf("cleat/go.mod names no version for github.com/cleat-team/cleat. "+
			"That require is what makes the module consumable outside this repo; "+
			"removing it is not a way to pass this test.\n%s", mod)
	}
	version := string(m[1])

	// The proxy, not `go list -m ...@<version>`: run from inside this repo the
	// main module IS that path, so `go list` resolves it from the working tree
	// and SUCCEEDS whatever the version says. The obvious check cannot fail
	// from the only place anyone would run it.
	url := fmt.Sprintf("https://proxy.golang.org/github.com/cleat-team/cleat/@v/%s.info", version)
	client := &http.Client{Timeout: proxyAttemptTimeout}
	status, attempts, err := askProxy(client, url, proxyRetryPauses, proxyMaxRetryAfter, time.Sleep)
	if verdict := proxyVerdict(version, url, status, attempts, err); verdict != "" {
		t.Error(verdict)
	}
}

// proxyAttemptTimeout is ONE attempt's timeout, and proxyRetryPauses the pauses between attempts, so the call is made at most len+1 = 3 times. A pause is the
// configured one, or the proxy's own Retry-After when that is longer, capped at proxyMaxRetryAfter, so the worst case is 3*20s + 10s + 10s = 80s (it used to be
// one 30s attempt). The shorter per-attempt timeout is deliberate: the failure that motivated the retry
// (cleat#4264) was a proxy that stalled awaiting headers for the whole 30s, and three independent tries beat one long wait on a proxy that is stuck.
const proxyAttemptTimeout = 20 * time.Second

var proxyRetryPauses = []time.Duration{2 * time.Second, 5 * time.Second}

// proxyMaxRetryAfter caps how long a Retry-After can make the test wait. The proxy's word is honoured because it knows how long it needs, and capped because a
// header the test cannot bound would let a misbehaving proxy hold a Tier 1 shard for as long as it liked.
const proxyMaxRetryAfter = 10 * time.Second

// askProxy returns the proxy's FIRST DEFINITE answer, retrying only what is not one: a transport error (no answer at all), a 5xx (the proxy saying it could
// not answer) or a 429 (the proxy saying to come back later, transient by definition). A 200 and a 404 or 410 ("unknown revision") are definite and final; so
// is any other status below 500 (a 403, say), which is not retried because it says something about the request and not about the proxy's health.
//
// When a retried response carries a Retry-After in seconds, the next pause is the longer of the configured one and that, capped at maxRetryAfter. The
// HTTP-date form is not honoured (the configured pause applies): the module proxy's CDN sends seconds, and parsing a date adds a clock to a test whose
// whole point is to have none. It never turns a transport failure into a pass: when every
// attempt is exhausted it returns the error, and the caller fails (cleat#4264: a skip here would reopen the hole cleat#2452 is about).
//
// status is 0 when there was no definite answer; attempts is how many calls were made; err carries the FIRST cause and the last one, since a retry
// that hides the first failure makes the second hard to read.
func askProxy(client *http.Client, url string, pauses []time.Duration, maxRetryAfter time.Duration, sleep func(time.Duration)) (status, attempts int, err error) {
	var first, last error
	var retryAfter time.Duration // the proxy's own request for how long to wait before the NEXT attempt, from the last response
	for i := 0; i <= len(pauses); i++ {
		if i > 0 {
			sleep(max(pauses[i-1], min(retryAfter, maxRetryAfter)))
		}
		retryAfter = 0
		attempts++
		resp, getErr := client.Get(url)
		if getErr != nil {
			last = getErr
		} else {
			code := resp.StatusCode
			_ = resp.Body.Close()
			if code < 500 && code != http.StatusTooManyRequests {
				return code, attempts, nil
			}
			if secs, convErr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); convErr == nil && secs > 0 {
				retryAfter = time.Duration(secs) * time.Second
			}
			last = fmt.Errorf("HTTP %d", code)
		}
		if first == nil {
			first = last
		}
	}
	if attempts > 1 && first.Error() != last.Error() {
		return 0, attempts, fmt.Errorf("%v (first attempt: %v)", last, first)
	}
	return 0, attempts, last
}

// proxyVerdict is the test's decision, separated so it can be pinned without a network: "" passes, anything else is the failure message.
func proxyVerdict(version, url string, status, attempts int, err error) string {
	if err != nil {
		return fmt.Sprintf("cleat/go.mod requires github.com/cleat-team/cleat %s, and this test could not "+
			"ask the module proxy whether that version exists after %d attempts: %v\n"+
			"This is a failure of the check, not necessarily a finding about the tree -- "+
			"the version may be fine. Re-run where the proxy is reachable.", version, attempts, err)
	}
	if status != http.StatusOK {
		return fmt.Sprintf("cleat/go.mod requires github.com/cleat-team/cleat %s, and the module proxy "+
			"does not serve it (HTTP %d).\n\n"+
			"A require must name a version that is ALREADY PUBLISHED. A package's own version is "+
			"bumped before the tag -- publish-pypi.yml refuses when the tag and pyproject.toml "+
			"disagree -- but a DEPENDENCY PIN is the opposite: it moves after the tag, because "+
			"until then there is nothing to resolve. The two rules read as contradictory until "+
			"you separate them, which is how cleat#2452 got this wrong.\n\n"+
			"Check with:\n    curl -s -o /dev/null -w '%%{http_code}\\n' %s\n"+
			"and take a version you know exists as a control.", version, status, url)
	}
	return ""
}
