package engine

// cleat#936: a mis-cased parent_close_policy terminated children on MySQL and
// abandoned them on PostgreSQL.
//
// Found by the samples-go port in cleat-team/cleat-ports, by running an
// existing test on a second dialect rather than by writing a new one.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestTheThreeLegalPoliciesAreAccepted(t *testing.T) {
	for _, p := range []string{
		ParentClosePolicyAbandon,
		ParentClosePolicyTerminate,
		ParentClosePolicyRequestCancel,
	} {
		if err := ValidateParentClosePolicy(p); err != nil {
			t.Errorf("%q was refused: %v", p, err)
		}
	}
}

// TestAnUnsetPolicyIsAccepted is the compatibility half, and it is the one that
// would break every existing caller if it were wrong.
//
// ChildWorkflow (the two-argument form) sends "" and the column DEFAULTs to
// ABANDON. Rejecting the empty string would refuse every child start that never
// asked for a policy at all.
func TestAnUnsetPolicyIsAccepted(t *testing.T) {
	if err := ValidateParentClosePolicy(""); err != nil {
		t.Errorf("the empty policy was refused: %v\n"+
			"ChildWorkflow sends \"\" for every caller that does not set one", err)
	}
}

// TestAMisCasedPolicyIsRefused is the defect itself. Lower-case "terminate" was
// accepted, stored verbatim, and then matched by MySQL's case-insensitive
// collation while PostgreSQL treated it as ABANDON.
func TestAMisCasedPolicyIsRefused(t *testing.T) {
	for _, p := range []string{"terminate", "Terminate", "TERMinate", "abandon", "request_cancel"} {
		err := ValidateParentClosePolicy(p)
		if err == nil {
			t.Errorf("%q was accepted; on MySQL it matches a policy arm through a "+
				"case-insensitive collation and on PostgreSQL it does not", p)
			continue
		}
		// The message must name the intended spelling. "must be one of ABANDON,
		// REQUEST_CANCEL, TERMINATE" beside "terminate" is a sentence people skim.
		if !strings.Contains(err.Error(), "did you mean") {
			t.Errorf("the error for %q does not offer the canonical spelling: %v", p, err)
		}
	}
}

func TestAnUnrelatedPolicyIsRefusedWithTheLegalSet(t *testing.T) {
	err := ValidateParentClosePolicy("KILL_EVERYTHING")
	if err == nil {
		t.Fatal("an unrelated policy was accepted")
	}
	for _, want := range []string{"ABANDON", "TERMINATE", "REQUEST_CANCEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s as legal: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("a value that is not a near-miss should not suggest one: %v", err)
	}
}

// TestEveryPolicyLiteralInTheDialectQueriesIsOneWeValidate is the guard that
// keeps the two halves from drifting apart.
//
// The validator and the SQL are in different files and neither imports the
// other -- the queries carry bare string literals. So a fourth policy added to
// a dialect query, or a rename, would leave the validator refusing a value the
// engine acts on, which is a worse failure than the one being fixed: the
// feature would be unreachable rather than inconsistent.
//
// Reads the SQL out of source rather than executing it, for the same reason the
// MSSQL tenant-predicate guard does: there is no database in a unit test, and
// the question is about what the source says.
func TestEveryPolicyLiteralInTheDialectQueriesIsOneWeValidate(t *testing.T) {
	// Only the files that enforce the policy. A grep of the whole package would
	// also find this test's own literals and the comments explaining them.
	files := []string{
		"store_lifecycle.go",
		"mysql_lifecycle.go",
		"mssql_lifecycle.go",
	}
	// parent_close_policy compared to a quoted literal, in any of the three
	// dialects' quoting.
	pat := regexp.MustCompile(`parent_close_policy\s*=\s*'([^']*)'`)

	found := map[string][]string{}
	for _, name := range files {
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for _, m := range pat.FindAllStringSubmatch(string(src), -1) {
			found[m[1]] = append(found[m[1]], name)
		}
	}
	if len(found) == 0 {
		t.Fatal("no parent_close_policy comparison found in any dialect file; " +
			"either the queries moved or this guard's pattern no longer matches, " +
			"and a guard that matches nothing passes silently")
	}
	for literal, files := range found {
		if !parentClosePolicies[literal] {
			t.Errorf("the dialect queries act on parent_close_policy %q (%s) but "+
				"ValidateParentClosePolicy refuses it, so no caller can ever set it",
				literal, strings.Join(files, ", "))
		}
	}
	// ABANDON must NOT appear: it is implemented as the absence of an arm, and a
	// query matching it would be a second, contradictory implementation.
	if f, ok := found[ParentClosePolicyAbandon]; ok {
		t.Errorf("%s compares parent_close_policy to %q; ABANDON is meant to be the "+
			"absence of an arm, so an explicit match is a second implementation of it",
			strings.Join(f, ", "), ParentClosePolicyAbandon)
	}
}
