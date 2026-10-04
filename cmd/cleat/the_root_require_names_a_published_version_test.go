package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("cleat/go.mod requires github.com/cleat-team/cleat %s, and this test could not "+
			"ask the module proxy whether that version exists: %v\n"+
			"This is a failure of the check, not necessarily a finding about the tree -- "+
			"the version may be fine. Re-run where the proxy is reachable.", version, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("cleat/go.mod requires github.com/cleat-team/cleat %s, and the module proxy "+
			"does not serve it (HTTP %d).\n\n"+
			"A require must name a version that is ALREADY PUBLISHED. A package's own version is "+
			"bumped before the tag -- publish-pypi.yml refuses when the tag and pyproject.toml "+
			"disagree -- but a DEPENDENCY PIN is the opposite: it moves after the tag, because "+
			"until then there is nothing to resolve. The two rules read as contradictory until "+
			"you separate them, which is how cleat#2452 got this wrong.\n\n"+
			"Check with:\n    curl -s -o /dev/null -w '%%{http_code}\\n' %s\n"+
			"and take a version you know exists as a control.", version, resp.StatusCode, url)
	}
}
