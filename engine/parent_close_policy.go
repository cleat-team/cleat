package engine

import (
	"fmt"
	"sort"
	"strings"
)

// The parent-close policies, as the ONE list the engine agrees on.
//
// Before cleat#936 the three literals existed only inside the dialect queries
// -- 'TERMINATE' and 'REQUEST_CANCEL' in store_lifecycle.go, mysql_lifecycle.go
// and mssql_lifecycle.go, 'ABANDON' as a column DEFAULT and nowhere else -- and
// nothing in the tree said what the legal set WAS. A value arriving from a
// guest was written verbatim and then compared against those literals by three
// different databases.
//
// ABANDON is deliberately in the list even though no query matches it. It is
// implemented as the ABSENCE of an arm: enforceParentClosePolicy updates the
// children of TERMINATE and REQUEST_CANCEL parents and leaves everyone else
// running. That is why an unrecognised value used to mean ABANDON -- not
// because anything decided so, but because nothing decided anything.
const (
	ParentClosePolicyAbandon       = "ABANDON"
	ParentClosePolicyTerminate     = "TERMINATE"
	ParentClosePolicyRequestCancel = "REQUEST_CANCEL"
)

// parentClosePolicies is the canonical set, keyed for lookup.
var parentClosePolicies = map[string]bool{
	ParentClosePolicyAbandon:       true,
	ParentClosePolicyTerminate:     true,
	ParentClosePolicyRequestCancel: true,
}

// ValidateParentClosePolicy reports whether a policy may be written.
//
// The empty string is VALID and means "unset": callers that never set a policy
// send "", the column DEFAULTs to 'ABANDON', and rejecting it would break every
// existing caller of the two-argument ChildWorkflow.
//
// Everything else must match exactly, and the reason is cleat#936 rather than
// tidiness. String equality is collation-dependent, and the dialects disagree:
//
//	mysql>      SELECT 'terminate' = 'TERMINATE';   1     (utf8mb4_0900_ai_ci)
//	postgres=#  SELECT 'terminate' = 'TERMINATE';   f
//
// So a mis-cased value did not merely fall through to ABANDON -- it fell
// through on PostgreSQL and MATCHED on MySQL, giving the same workflow with the
// same policy string opposite child-lifecycle behaviour depending on the
// backing database, with nothing in the workflow, the metadata or the logs to
// show it.
//
// REJECTING RATHER THAN NORMALISING is the deliberate choice. Upper-casing the
// value would also close the divergence and would keep working anyone who is
// currently relying on lower-case matching on MySQL -- which is precisely the
// objection to it: that behaviour is a bug they cannot know they depend on, and
// silently making it official on three databases makes it permanent. A refusal
// is visible at the call that caused it. It is a breaking change for exactly
// the callers who were already getting a different answer on different
// databases.
//
// Validation lives at the WRITE boundary rather than in the queries. A
// COLLATE clause on three predicates would fix the comparison and leave the
// mis-cased value sitting in the column, still meaning different things to
// different readers -- including to a human running a SELECT.
func ValidateParentClosePolicy(policy string) error {
	if policy == "" || parentClosePolicies[policy] {
		return nil
	}
	legal := make([]string, 0, len(parentClosePolicies))
	for p := range parentClosePolicies {
		legal = append(legal, p)
	}
	sort.Strings(legal)

	// Name the near-miss explicitly. The overwhelmingly likely mistake is case,
	// and "must be one of ABANDON, REQUEST_CANCEL, TERMINATE" read next to
	// "terminate" is a sentence people skim past.
	if canonical, ok := caseInsensitiveParentClosePolicy(policy); ok {
		return fmt.Errorf(
			"parent close policy %q is not recognised; did you mean %q? "+
				"policies are matched exactly, because a case-insensitive database "+
				"collation would accept %q while a case-sensitive one silently treats "+
				"it as %s (cleat#936)",
			policy, canonical, policy, ParentClosePolicyAbandon)
	}
	return fmt.Errorf(
		"parent close policy %q is not recognised; must be one of %s, or empty for the default (%s)",
		policy, strings.Join(legal, ", "), ParentClosePolicyAbandon)
}

// caseInsensitiveParentClosePolicy returns the canonical spelling of a policy
// that differs from a legal one only by case.
func caseInsensitiveParentClosePolicy(policy string) (string, bool) {
	for p := range parentClosePolicies {
		if strings.EqualFold(p, policy) {
			return p, true
		}
	}
	return "", false
}
