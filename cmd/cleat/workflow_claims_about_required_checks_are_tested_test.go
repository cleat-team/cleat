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

// requiredCheckClaim is the convention this guard enforces: a comment stating that a
// check is or is not required must NAME it, on one line, ending at the name.
//
//	# NOT A REQUIRED CHECK: Release Dry Run
//	# A REQUIRED CHECK: Closing References
//
// The name must end the line because the alternative — "the name is the text after
// `CHECK:` up to the next period or comma" — cannot parse
// `Crash Shutdown Scenarios (MySQL, SQL Server)`, which has a comma inside
// parentheses. A grammar that needs a parser to read a check name is the wrong
// grammar; a line that ends at the name needs none.
var requiredCheckClaim = regexp.MustCompile(`(?i)((?:NOT\s+)?A REQUIRED CHECK):\s*(\S.*?)\s*$`)

// negativeClaimWithoutName matches the harmful form the convention exists for: a
// comment saying a check is NOT required, with no name. That is the shape
// cleat#2510 was, and a reader acts on it by not acting.
var negativeClaimWithoutName = regexp.MustCompile(`(?i)NOT\s+A\s+REQUIRED\s+CHECK`)

// doubleQuoted strips `"..."` spans. A QUOTATION IS NOT A CLAIM, and this is not a
// hypothetical: the first run of this guard failed on the comment that records
// cleat#2510's history, because that comment quotes the stale sentence verbatim.
// Reading a quotation as a live assertion is the same defect as a name scan scoring
// a source file by matching the sentence denying the name -- and it is worth being
// explicit that the false positive arrived on the guard written to prevent a class
// whose whole subject is text that means something other than what it says.
var doubleQuoted = regexp.MustCompile(`"[^"\n]*"`)

// unnamedNegativeClaimsAccepted baselines the sites that deliberately do not name a
// check. It may only shrink: a stale entry fails the test.
var unnamedNegativeClaimsAccepted = map[string]string{
	".github/workflows/tier1-push-failure-notifier.yml": "the comment's subject IS that its job name must not look like a required context: \"NOT A REQUIRED CHECK, AND DELIBERATELY NOT NAMED LIKE ONE\". Naming it here would contradict the paragraph it introduces.",
}

// TestWorkflowClaimsAboutRequiredChecksAreTrue pins the property cleat#2510 is about.
//
// THE DEFECT. `.github/workflows/closing-reference-check.yml:16` said *"NOT A
// REQUIRED CHECK (as of 2026-09-23). A red here blocks nothing on its own"*. The
// check HAS been required since cleat#2100 — the comment was written at
// 2026-09-23T16:21:15Z and the check was made required at 20:32:29Z, four hours
// later — and the stale claim talked a stream out of a repair before anyone caught
// it. A stale "not required" reads as permission not to act, which is why that
// direction is the one worth guarding.
//
// WHY THE CHECK MUST BE NAMED, and this is measured rather than assumed. The first
// design resolved a claim to a check by POSITION — the next `name:` below it. Run
// against the live tree it resolved `closing-reference-check.yml:16` to
// `Closing Reference Check`, the WORKFLOW's name, not `Closing References`, the
// JOB's — and the workflow name is not a required context, so the guard reported
// the claim as true on the tree containing the defect. A false negative on the one
// instance it exists for. The census's class-2 measurement reached the same place
// from the other side: of three apparent contradictions resolved by hand, **2 of 3
// were wrong**. Position cannot resolve a claim, so the claim names its check.
//
// WHAT IT CHECKS. Each claim against `.github/required-checks.txt`, in the
// direction it asserts — a "NOT required" claim fails if the name IS in the file,
// and a required claim fails if it is not.
//
// WHAT IT DOES NOT DO. It does not read the live branch protection. That list lives
// in GitHub and 33 contexts are on it; `.github/required-checks.txt` mirrors it, and
// `scripts/check-required-contexts.py` is what keeps the mirror honest against
// tiers.yaml. This guard tests a comment against the in-tree copy of one fact, which
// is the same fact — not against GitHub, which a unit test cannot reach.
func TestWorkflowClaimsAboutRequiredChecksAreTrue(t *testing.T) {
	root := repoRootFor(t)
	required := requiredContexts(t, root)

	// Anti-vacuity: an empty required set agrees with every claim of "not required".
	if len(required) < 20 {
		t.Fatalf("read %d required context(s) from .github/required-checks.txt; there were 33\n"+
			"on 2026-09-27.\n\n"+
			"This is a failure of the CHECK, not a finding about the workflows: against an\n"+
			"empty list every \"NOT a required check\" claim reads as true.", len(required))
	}

	// The self-test runs the same parser the scan uses, in both directions, so it
	// proves the branch it exercises.
	for _, c := range []struct {
		line, name string
		neg        bool
	}{
		{"# NOT A REQUIRED CHECK: Some Job", "Some Job", true},
		{"# A REQUIRED CHECK: Some Job", "Some Job", false},
		{"# release branch, so this is deliberately NOT A REQUIRED CHECK: Release smoke", "Release smoke", true},
	} {
		got, neg := parseRequiredCheckClaim(c.line)
		if got != c.name || neg != c.neg {
			t.Fatalf("self-test failed on %q: got (%q, neg=%v), want (%q, neg=%v).\n\n"+
				"This is a failure of the CHECK, not a finding about the workflows.",
				c.line, got, neg, c.name, c.neg)
		}
	}

	type claim struct {
		file string
		line int
		name string
		neg  bool
	}
	var claims []claim
	seenUnnamed := map[string]struct{}{}
	unnamed := map[string][]int{}
	candidateNames := map[string]struct{}{}

	for _, rel := range trackedFiles(t, root, ".github/workflows/*.yml") {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		for _, m := range workflowNameRe.FindAllStringSubmatch(string(data), -1) {
			candidateNames[strings.Trim(strings.TrimSpace(m[1]), `"'`)] = struct{}{}
		}
		for i, l := range strings.Split(string(data), "\n") {
			if !strings.Contains(l, "#") {
				continue
			}
			if name, neg := parseRequiredCheckClaim(l); name != "" {
				claims = append(claims, claim{rel, i + 1, name, neg})
				continue
			}
			// Quote first, then match: a comment that RECORDS the old wording is
			// not making the claim, and without this the guard fires on the
			// comment explaining the very defect it exists for.
			if negativeClaimWithoutName.MatchString(doubleQuoted.ReplaceAllString(l, "")) {
				if _, ok := unnamedNegativeClaimsAccepted[rel]; ok {
					seenUnnamed[rel] = struct{}{}
					continue
				}
				unnamed[rel] = append(unnamed[rel], i+1)
			}
		}
	}

	// Anti-vacuity for the name universe, on the same reasoning: an empty universe
	// would make every claim "not a job or workflow name".
	if len(candidateNames) < 20 {
		t.Fatalf("collected %d job/workflow name(s) from .github/workflows/; there were\n"+
			"more than 100 on 2026-09-27.\n\n"+
			"This is a failure of the CHECK, not a finding about the workflows.", len(candidateNames))
	}

	if len(claims) < 4 {
		t.Fatalf("found %d named claim(s) in the workflow headers; there were 5 on 2026-09-27.\n\n"+
			"This is a failure of the CHECK, not a finding about the workflows: a parser that\n"+
			"finds no claims agrees with every one of them.", len(claims))
	}

	var stale []string
	for rel := range unnamedNegativeClaimsAccepted {
		if _, ok := seenUnnamed[rel]; !ok {
			stale = append(stale, rel)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%d baselined entr(y|ies) no longer match:\n  %s\n\n"+
			"Delete the entry in the same change that fixes it; left in place it is a\n"+
			"standing exemption for the very form this guard exists to catch.",
			len(stale), strings.Join(stale, "\n  "))
	}

	var problems []string
	for rel, lines := range unnamed {
		sort.Ints(lines)
		for _, ln := range lines {
			problems = append(problems, fmt.Sprintf(
				"%s:%d says a check is NOT required without naming it.\n"+
					"      Write it as `# NOT A REQUIRED CHECK: <name>` on one line.", rel, ln))
		}
	}
	for _, c := range claims {
		// A CLAIM MUST NAME A CHECK THAT EXISTS. Without this the parser accepts a
		// sentence as a name and a NEGATIVE claim passes silently, because a name
		// that matches nothing is, by construction, not in the required list. The
		// hole was live: `# NOT a required check: .github/required-checks.txt
		// mirrors branch protection,` parsed as a claim whose name was
		// `.github/required-checks.txt mirrors branch protection,`, and reported
		// clean. That is this repo's "reads cleanest where it measured least",
		// arriving in the guard written to prevent exactly that shape.
		if _, ok := candidateNames[c.name]; !ok {
			problems = append(problems, fmt.Sprintf(
				"%s:%d names %q, which is not a job or workflow name anywhere in\n"+
					"      .github/workflows/. Either it is a typo, or the claim is not in the\n"+
					"      convention's form -- `# <NOT >A REQUIRED CHECK: <name>` on one line,\n"+
					"      ending at the name.", c.file, c.line, c.name))
			continue
		}
		_, inRequired := required[c.name]
		switch {
		case c.neg && inRequired:
			problems = append(problems, fmt.Sprintf(
				"%s:%d claims %q is NOT a required check, and it IS one.\n"+
					"      A stale \"not required\" reads as permission not to act -- this is the\n"+
					"      shape of cleat#2510, which talked a stream out of a repair.", c.file, c.line, c.name))
		case !c.neg && !inRequired:
			problems = append(problems, fmt.Sprintf(
				"%s:%d claims %q IS a required check, and it is not.\n"+
					"      A merge gate that is not a gate is worse than a slow check.", c.file, c.line, c.name))
		}
	}

	if len(problems) == 0 {
		t.Logf("tested %d named claim(s) against %d required context(s); all true",
			len(claims), len(required))
		return
	}
	sort.Strings(problems)
	t.Errorf("%d problem(s) with the workflow headers:\n\n  %s\n\n"+
		"Fix the comment or the config -- whichever is wrong -- not this test.",
		len(problems), strings.Join(problems, "\n\n  "))
}

// workflowNameRe collects every `name:` value in a workflow file -- job names at
// four spaces and the workflow's own name at column zero. A claim about a check must
// name one of these; anything else is a sentence that parsed as a name.
var workflowNameRe = regexp.MustCompile(`(?m)^\s{0,4}name:\s*(\S.*?)\s*$`)

// parseRequiredCheckClaim returns the check name a line claims, and whether the
// claim is that it is NOT required. An empty name means the line is not a claim in
// the convention's form.
func parseRequiredCheckClaim(line string) (string, bool) {
	m := requiredCheckClaim.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[2], strings.Contains(strings.ToUpper(m[1]), "NOT")
}

// requiredContexts reads the in-tree mirror of develop's branch protection.
func requiredContexts(t *testing.T, root string) map[string]struct{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".github", "required-checks.txt"))
	if err != nil {
		t.Fatalf("reading .github/required-checks.txt: %v\n\nThis is a failure of the CHECK.", err)
	}
	out := map[string]struct{}{}
	for _, l := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(l)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		out[s] = struct{}{}
	}
	return out
}
