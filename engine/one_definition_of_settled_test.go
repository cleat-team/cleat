package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// settledSet is the set this guard compares against, derived from the shipped
// definition rather than retyped -- a guard that hardcodes the answer cannot
// notice the definition changing underneath it.
func settledSet() map[string]bool {
	// Parsed from the shipped constant rather than retyped. A guard that
	// hardcodes the answer cannot notice the definition changing underneath it,
	// which is the whole reason this guard exists.
	m := map[string]bool{}
	for _, v := range strings.Split(settledStatusList, ",") {
		m[strings.Trim(strings.TrimSpace(v), "'")] = true
	}
	return m
}

// narrowerThanSettled declares every status predicate that deliberately names
// FEWER statuses than "settled", keyed by "<file>:<sorted values>" so the entry
// covers a specific statement rather than a whole file.
//
// AN ENTRY IS NOT A GRANT THAT THE PREDICATE IS CORRECT. It records that the
// narrowing was seen and why. Where the honest answer is "this may be wrong and
// I am not deciding it here", the entry must say so -- an exemption whose reason
// is invented reads as review having happened when it has not.
//
// I wrote one of those and it survived until the tracker was checked. The first
// version of the done,failed entry below said 'terminated' had "no arm and no
// stated reason". It has both: the done,failed,terminated entry IS that arm, and
// cleat#1023 measured the whole picture a year earlier. The reason now cites it.
// A plausible reason written from the code alone is the failure mode this map
// exists to prevent, and the author is not exempt from it.
var narrowerThanSettled = map[string]string{
	"engine/retention_predicates.go:done,failed": "" +
		"DeleteExpiredEvents -- the on-by-default --retention-days sweep. The narrowing " +
		"is DELIBERATE and measured, in cleat#1023: 'done' and 'failed' rows have no " +
		"event history left to collect (finalize_workflow_status purges it, and its " +
		"condition is p_final_status = 'done' OR 'failed'), while 'terminated' and " +
		"'dead_lettered' fall outside that condition and KEEP their events. So this " +
		"predicate names exactly the two statuses with nothing to delete. That is the " +
		"inversion cleat#1023 is about; it is recorded there, not a gap here.",

	"engine/retention_predicates.go:done,failed,terminated": "" +
		"--completed-workflow-retention-days, which is OFF BY DEFAULT (cleat#1023). " +
		"This is the arm that does reach 'terminated'. 'dead_lettered' is excluded on " +
		"purpose and the flag's own help text says so.",

	"engine/mysql_ops.go:done,failed,terminated": "" +
		"MySQL's arm of the same --completed-workflow-retention-days sweep; same set, " +
		"same reason as the entry above.",
}

// TestEverySettledStatusPredicateUsesOneDefinition fails when a predicate
// spells the settled-status set by hand instead of embedding settledStatusList.
//
// WHY A GUARD RATHER THAN A COMMENT. Two defects, two years apart, were the
// same hand-written list falling behind the statuses the engine writes:
// cleat#1227 (a parent overwrote a dead-lettered child) and the child-quota
// leak in engine/a_terminal_child_does_not_hold_its_parents_quota_test.go. A
// prose warning about that would rot the moment someone fixed it; a test goes
// red when it comes back and stays silent when it does not.
//
// It matters most for whoever adds the NEXT status. cleat#1153 adds 'cancelled'
// as a terminal status, and the question "which of the fifteen predicates must
// learn about it" is the whole risk of that change. After this, the answer is
// "one".
func TestEverySettledStatusPredicateUsesOneDefinition(t *testing.T) {
	// -C ..: run git at the REPO ROOT so the paths come back root-relative.
	// Run from engine/ it lists "adaptive_flush.go" rather than
	// "engine/adaptive_flush.go", and every exemption key -- which embeds the
	// path -- would silently stop matching.
	out, err := exec.Command("git", "-C", "..", "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v -- this guard reasons about THE REPO, so it walks "+
			"tracked files rather than the filesystem: a scratch worktree under "+
			".claude/ is a second copy of the tree and would be scanned as if it "+
			"were this one", err)
	}
	// A literal list of quoted lowercase words after `status IN` / `status NOT IN`.
	// Sites that embed settledStatusList do not match: the text there is
	// "status NOT IN (` + settledStatusList + `)", which has no quoted words.
	pat := regexp.MustCompile(`(?i)\bstatus\s+(NOT\s+)?IN\s*\(\s*('[a-z_]+'(?:\s*,\s*'[a-z_]+')*)\s*\)`)

	settled := settledSet()
	var handWritten []string
	var undeclared []string
	scanned := 0

	for _, f := range strings.Fields(string(out)) {
		if strings.HasSuffix(f, "_test.go") || strings.HasSuffix(f, "status_vocabulary.go") {
			continue
		}
		// git ls-files supplies the LIST; the content comes from the WORKING
		// TREE. Reading HEAD instead would make the guard blind to the change
		// being reviewed, which is the one moment it has to see.
		src, err := os.ReadFile(filepath.Join("..", f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			// Prose ABOUT a predicate is not a predicate. This file's own
			// doc comments quote these lists, and so do several in engine/.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, m := range pat.FindAllStringSubmatch(line, -1) {
				var vals []string
				for _, v := range strings.Split(m[2], ",") {
					vals = append(vals, strings.Trim(strings.TrimSpace(v), "'"))
				}
				sort.Strings(vals)
				overlaps := false
				for _, v := range vals {
					if settled[v] {
						overlaps = true
					}
				}
				if !overlaps {
					continue // a live-status predicate: a different question
				}
				scanned++
				if len(vals) == len(settled) {
					allSettled := true
					for _, v := range vals {
						if !settled[v] {
							allSettled = false
						}
					}
					if allSettled {
						// The right set. Now check it is spelled the canonical
						// way: a reordering is harmless to SQL and NOT harmless
						// to the digest-keyed exemptions in
						// mssql_tenant_predicate_test.go.
						if m[2] != settledStatusList {
							handWritten = append(handWritten, fmt.Sprintf(
								"%s:%d  spelled   %s\n      canonical %s", f, i+1, m[2], settledStatusList))
						}
						continue
					}
				}
				key := f + ":" + strings.Join(vals, ",")
				if _, ok := narrowerThanSettled[key]; !ok {
					undeclared = append(undeclared,
						fmt.Sprintf("%s:%d  (%s)\n      key: %q", f, i+1, strings.Join(vals, ", "), key))
				}
			}
		}
	}

	// UNMEASURED rather than a pass. A regex that stopped matching -- a
	// reformat putting the list on its own line, a rename of the column --
	// reports a clean tree, which is the failure this whole guard is about.
	if scanned == 0 {
		t.Fatal("UNMEASURED: this guard found no status predicate mentioning any settled " +
			"status anywhere in the tree. That is not plausible -- it is the pattern no " +
			"longer matching. Check the regex against engine/retention_predicates.go, " +
			"which is known to contain several.")
	}
	t.Logf("status predicates mentioning a settled status: %d checked, "+
		"%d hand-written full sets, %d undeclared narrower", scanned, len(handWritten), len(undeclared))

	if len(handWritten) > 0 {
		t.Errorf("%d predicate(s) name the settled set but do not spell it canonically:"+
			"\n\n    %s\n\n"+
			"Match settledStatusList in engine/status_vocabulary.go exactly. The set being "+
			"right is not sufficient: engine/mssql_tenant_predicate_test.go keys its "+
			"exemptions on a digest of statement text, so a reordering silently "+
			"invalidates every exemption covering a statement that contains this list.",
			len(handWritten), strings.Join(handWritten, "\n    "))
	}
	if len(undeclared) > 0 {
		t.Errorf("%d status predicate(s) name FEWER statuses than settled and are not "+
			"declared:\n\n    %s\n\n"+
			"A narrower list may be perfectly correct -- retention deliberately treats "+
			"some terminal statuses differently. But it must be written down, because an "+
			"undeclared narrowing is indistinguishable from a list somebody forgot to "+
			"update. Add an entry to narrowerThanSettled saying WHAT the predicate is "+
			"for, and if you do not know whether the narrowing is right, say that "+
			"instead of inventing a reason.\n\n"+
			"IF YOU JUST ADDED A STATUS to settledStatuses() -- cleat#1153 adds "+
			"'cancelled' -- this is the list you are looking for. Every site above now "+
			"names fewer statuses than settled, which is precisely the question that "+
			"had no answer before: these are the predicates that must learn about it.",
			len(undeclared), strings.Join(undeclared, "\n    "))
	}
}
