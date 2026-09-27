package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// auditedTriggerExclusions records the jobs whose run is restricted to main, a push,
// a tag or a schedule -- so they cannot see the changes they exist for. Each entry
// is a stated decision, and a stale entry fails the test.
//
// It may only be added to deliberately. A NEW job that restricts its trigger this
// way fails until someone writes down why, which is the whole guard: an exclusion is
// silent by construction, because the job does not run and nothing goes red.
var auditedTriggerExclusions = map[string]string{
	"ci.yml|benchmarks": "Main-and-push only, on purpose: a benchmark run per pull request is not worth the runner cost. THE EXPOSURE IS REAL AND IS NOT CLAIMED AWAY -- a PR can move performance and nothing measures it, which is the condition excluding the changes the job exists to measure (cleat#2487). Widening it is a separate decision about runner cost, and this entry is where that decision is recorded rather than implied.",
}

// jobIf is a job-level `if:` and the name of the job carrying it.
var jobIf = regexp.MustCompile(`(?m)^\s{4}if:\s*(.+?)\s*$`)
var jobKey = regexp.MustCompile(`(?m)^  ([A-Za-z0-9_-]+):\s*$`)
var jobName = regexp.MustCompile(`(?m)^\s{4}name:\s*(.+?)\s*$`)

// restrictsToNonPR is the shape this guard audits: a condition that EXCLUDES pull
// requests and names a run context a pull request cannot be -- main, a push, a tag,
// or a schedule. A condition that merely mentions a base branch is a different shape
// and is not matched: `github.event.pull_request.base.ref != 'main'` names a pull
// request, so it can see one. That distinction is why `dco-check.yml`'s exclusion --
// which is deliberate, justified at length in its own comment, and exists so a
// permanently-red release PR does not train readers to merge past red -- is out of
// scope here rather than silently baselined.
var restrictsToNonPR = regexp.MustCompile(`refs/heads/main|event_name == 'push'|refs/tags/|schedule`)

// TestAConditionalJobStatesWhatItCannotSee pins the property cleat#2487 is about.
//
// THE CLASS. A check that cannot see the changes it exists for: a trigger or filter
// excluding the case the job was written for. Instances are SILENT BY CONSTRUCTION —
// the job does not run, so nothing goes red, and the absence looks like a pass.
//
// THE ONE LIVE INSTANCE. `benchmarks` (`ci.yml:1991`) runs on
// `github.ref == 'refs/heads/main' && github.event_name == 'push'`, so it cannot
// measure what a pull request changes. Its header comment says what it does and where
// the numbers go; it does not say why the PR case is excluded, so the exclusion is
// unstated rather than decided. That is what this guard changes -- not the trigger.
//
// WHY THIS ROW IS GUARDED AT ALL, GIVEN ITS N IS SMALL. The census's measured N here
// is 4, and WS-1's prior -- *if the invariant has to hold at N call sites, it is in
// the wrong place* -- reads a small N as arguing against a guard. **The prior does
// not apply, and it is because the prior has two parts.** It carries a measurement
// (N) and a remedy (move the invariant into the type). Small N argues against a guard
// only where the remedy EXISTS. GitHub offers no construct to consolidate a trigger
// into -- measured: 0 reusable-workflow invocations against 4 inline job-level
// `if:`s -- so the remedy is absent and the prior is silent rather than opposed.
//
//	axis       question                              decides
//	predicate  can a mechanical check be written?    whether a guard can exist
//	remedy     is there a structural repair?         whether guarding is worth it
//
// Recommended on the predicate axis being satisfied and the remedy axis being empty,
// NOT on the size of N.
func TestAConditionalJobStatesWhatItCannotSee(t *testing.T) {
	root := repoRootFor(t)

	// The self-test runs the same predicate the scan uses, in both directions. The
	// negative half is the one that matters: it is what keeps `dco`'s justified
	// base-branch exclusion out of the population.
	for _, c := range []struct {
		cond string
		want bool
	}{
		{"github.ref == 'refs/heads/main' && github.event_name == 'push'", true},
		{"github.event_name == 'schedule'", true},
		{"github.event_name == 'pull_request' || github.ref == 'refs/heads/main'", false},
		{"github.event_name == 'merge_group' || github.event.pull_request.base.ref != 'main'", false},
		{"always()", false},
	} {
		if got := restrictsToNonPR.MatchString(c.cond) && !strings.Contains(c.cond, "pull_request"); got != c.want {
			t.Fatalf("self-test failed on %q: got %v, want %v.\n\n"+
				"This is a failure of the CHECK, not a finding about the workflows.", c.cond, got, c.want)
		}
	}

	type exclusion struct{ file, job, name, cond string }
	var found []exclusion
	jobsScanned := 0

	for _, rel := range trackedFiles(t, root, ".github/workflows/*.yml") {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		src := string(data)
		jobsScanned += len(jobKey.FindAllStringSubmatch(src, -1))

		for _, m := range jobIf.FindAllStringSubmatchIndex(src, -1) {
			cond := src[m[2]:m[3]]
			if strings.Contains(cond, "pull_request") || !restrictsToNonPR.MatchString(cond) {
				continue
			}
			// the job this `if:` belongs to: the nearest `key:` above, and the
			// nearest `name:` below. Deliberately only used to LABEL the finding,
			// never to decide whether it is one -- resolving a claim by position
			// is the mistake the sibling guard measured at 2-of-3 wrong.
			before := src[:m[0]]
			jobs := jobKey.FindAllStringSubmatch(before, -1)
			job := "?"
			if len(jobs) > 0 {
				job = jobs[len(jobs)-1][1]
			}
			// The name PRECEDES the `if:` -- a job is `key:`, `name:`, then its
			// conditions -- so searching forward finds the NEXT job's name and
			// labels the finding with a job that is not the one at fault. That is
			// what the first version did: it reported `coverage` as
			// "Cluster Integration Tests".
			nm := job
			if n := jobName.FindAllStringSubmatch(before, -1); len(n) > 0 {
				nm = strings.Trim(strings.TrimSpace(n[len(n)-1][1]), `"'`)
			}
			found = append(found, exclusion{rel, job, nm, cond})
		}
	}

	// Anti-vacuity, both halves: a scan that reads no jobs, or finds no conditional
	// job at all, agrees with every tree.
	if jobsScanned < 30 {
		t.Fatalf("scanned %d job(s) across .github/workflows/; there are far more than\n"+
			"that on 2026-09-27.\n\n"+
			"This is a failure of the CHECK, not a finding about the workflows.", jobsScanned)
	}
	if len(found) == 0 {
		t.Fatalf("found no job restricted away from pull requests; there was one on\n" +
			"2026-09-27 (`benchmarks`).\n\n" +
			"This is a failure of the CHECK, not a finding about the workflows: a scan\n" +
			"that finds no exclusions agrees with every one of them.")
	}

	seen := map[string]struct{}{}
	var problems []string
	for _, e := range found {
		key := filepath.Base(e.file) + "|" + e.job
		seen[key] = struct{}{}
		if _, ok := auditedTriggerExclusions[key]; ok {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s  job `%s` (%q) cannot see a pull request.\n"+
				"      Its condition restricts the run to main, a push, a tag or a schedule, so a\n"+
				"      PR can change what this job measures and nothing will report it. Either\n"+
				"      widen the trigger, or add `%s` to auditedTriggerExclusions with the\n"+
				"      reason the exclusion is worth its cost.",
			e.file, e.job, e.name, key))
	}

	var stale []string
	for key := range auditedTriggerExclusions {
		if _, ok := seen[key]; !ok {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%d audited exclusion(s) no longer match a conditional job:\n  %s\n\n"+
			"Delete the entry in the same change that widens the trigger. Left in place\n"+
			"it records a decision about a condition that is no longer there -- and the\n"+
			"next reader cannot tell a live justification from a historical one.",
			len(stale), strings.Join(stale, "\n  "))
	}

	if len(problems) == 0 {
		t.Logf("scanned %d job(s); %d restricted away from pull requests, all audited",
			jobsScanned, len(found))
		return
	}
	sort.Strings(problems)
	t.Errorf("%d unaudited trigger exclusion(s):\n\n  %s",
		len(problems), strings.Join(problems, "\n\n  "))
}
