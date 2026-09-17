package plugin_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// docs/contributor/plugins/plugin-contract.md names every total-coverage guard
// in plugin/ and plugins/, and every such guard is named by it. cleat#1828.
//
// # Why bidirectional, and why the second direction is the point
//
// A document listing rules goes stale the moment a fourteenth rule is added,
// and it goes stale SILENTLY: the page still reads as the contract, and the new
// obligation is discoverable only the way all of them used to be -- by writing a
// plugin and watching CI reject it.
//
// So this checks both:
//
//   - every guard the document names exists (it cannot cite a test that is gone)
//   - every guard that exists is named by the document, as a clause or as
//     explicitly out of scope (it cannot omit a rule an author must satisfy)
//
// The first direction is the cheap one. The second is what the issue asked for.
//
// # On the count, because both prior numbers were wrong
//
// cleat#1828 said five guards. priorities-2026-09-17.md said eleven. Measured:
// 16 total-coverage test functions across 12 files, of which 13 are author
// obligations and 3 are framework invariants. Neither number came from a scan.
// That is precisely why this test derives the set rather than asserting a total:
// a count in prose is a claim nobody re-runs.
//
// # What counts as a total-coverage guard
//
// A test function named TestEvery*, TestAll* or TestNo*. That is the naming this
// repository already uses for "this property holds across the whole population",
// and it is the convention a plugin author would have to grep for today.
//
// The pattern is DELIBERATELY BROADER than the clause list: it will match a new
// TestNoSomething that is not really a contract rule at all. That is the correct
// failure -- it forces a decision and a line in the document, rather than
// letting a new guard land unnamed.

var (
	coverageGuardRe = regexp.MustCompile(`(?m)^func (Test(?:Every|All|No[A-Z])[A-Za-z0-9_]*)\(`)
	// The document names each guard as `TestX` in backticks.
	docGuardRe = regexp.MustCompile("`(Test(?:Every|All|No[A-Z])[A-Za-z0-9_]*)`")
	// <!-- external-guard <TestName> <path> -->
	externalGuardRe = regexp.MustCompile(`<!-- external-guard (\S+) (\S+) -->`)
)

func contractRepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("UNMEASURED: locating the repository root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// guardsInTree returns every total-coverage test function under plugin/ and
// plugins/, keyed by name, with the file it lives in.
func guardsInTree(t *testing.T, root string) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, dir := range []string{"plugin", "plugins"} {
		base := filepath.Join(root, dir)
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return err
			}
			// Only the package's own top-level tests. A plugin's internal tests
			// under plugins/<name>/ are that plugin's business, not the
			// contract's -- the contract is what EVERY plugin must satisfy.
			if filepath.Dir(path) != base {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			rel, _ := filepath.Rel(root, path)
			for _, m := range coverageGuardRe.FindAllStringSubmatch(string(src), -1) {
				found[m[1]] = rel
			}
			return nil
		})
		if err != nil {
			t.Fatalf("UNMEASURED: walking %s: %v. This is a failure of the check.", base, err)
		}
	}
	return found
}

func TestThePluginContractNamesEveryGuardAndEveryGuardIsNamed(t *testing.T) {
	root := contractRepoRoot(t)

	inTree := guardsInTree(t, root)
	// A scan that finds nothing agrees with a document listing nothing, so an
	// empty population is a broken check rather than a clean tree.
	if len(inTree) == 0 {
		t.Fatal("UNMEASURED: found no TestEvery*/TestAll*/TestNo* functions under plugin/ or " +
			"plugins/. The contract is known to have more than a dozen, so the scan is broken. " +
			"This is a failure of the check, not a finding about the tree.")
	}

	docPath := filepath.Join(root, "docs", "contributor", "plugins", "plugin-contract.md")
	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("UNMEASURED: reading %s: %v. The contract document is what this compares "+
			"against; without it there is no comparison.", docPath, err)
	}
	inDoc := map[string]bool{}
	for _, m := range docGuardRe.FindAllStringSubmatch(string(doc), -1) {
		inDoc[m[1]] = true
	}
	if len(inDoc) == 0 {
		t.Fatal("UNMEASURED: the contract document names no guards in backticks. Either the " +
			"format changed or the file is empty; either way this comparison is vacuous.")
	}

	// A guard may enforce a plugin obligation from outside these packages. The
	// document declares each with its path, and we verify it is there -- so the
	// citation cannot rot, and the scan below does not read it as dangling.
	external := map[string]string{}
	for _, m := range externalGuardRe.FindAllStringSubmatch(string(doc), -1) {
		external[m[1]] = m[2]
	}
	for name, rel := range external {
		src, rerr := os.ReadFile(filepath.Join(root, rel))
		if rerr != nil {
			t.Errorf("plugin-contract.md declares %s in %s, which cannot be read: %v", name, rel, rerr)
			continue
		}
		if !strings.Contains(string(src), "func "+name+"(") {
			t.Errorf("plugin-contract.md declares %s in %s, but no such function is there. "+
				"A clause resting on a guard that moved is a rule nothing enforces.", name, rel)
		}
	}

	// Direction 1: the document cannot cite a guard that does not exist.
	var missing []string
	for name := range inDoc {
		if _, ok := inTree[name]; !ok && external[name] == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		t.Errorf("plugin-contract.md names %s, which exists in no test file under plugin/ or "+
			"plugins/. Either it was renamed or deleted; a contract citing a guard that is gone "+
			"describes a rule nothing enforces.", name)
	}

	// Direction 2: the tree cannot contain a guard the document does not name.
	// This is the one that keeps the page honest.
	var unnamed []string
	for name, file := range inTree {
		if !inDoc[name] {
			unnamed = append(unnamed, name+"  ("+file+")")
		}
	}
	sort.Strings(unnamed)
	for _, u := range unnamed {
		t.Errorf("%s is a total-coverage guard that plugin-contract.md does not name.\n\n"+
			"A plugin author reading that page would not know this rule exists, which is the "+
			"state cleat#1828 was filed about. Add it as a clause, or — if it is a framework "+
			"invariant rather than an obligation on a plugin — list it under \"Deliberately not "+
			"clauses\" with the reason.", u)
	}

	t.Logf("%d total-coverage guards in tree, %d named by the document", len(inTree), len(inDoc))
}
