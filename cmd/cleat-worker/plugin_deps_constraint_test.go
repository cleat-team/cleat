package main

import "testing"

// cleat#1260: this check compared a workflow's plugin CONSTRAINT to the
// worker's VERSION with `!=`, so every constraint form the documentation shows
// permanently failed a workflow on a worker that satisfied it.
//
// EVERY FORM IS ASSERTED, including the ones that already worked and the ones
// that must still be refused. The bug was that the accepted set was far too
// small; a fix that makes it too large is the obvious way to overshoot, and a
// table with only the previously-failing rows could not tell the two apart.
func TestEveryDocumentedConstraintFormIsHonoured(t *testing.T) {
	worker := map[string]string{"llm": "1.3.0", "blobstore": "2.0.0"}

	cases := []struct {
		dep    map[string]string
		wantOK bool
		why    string
	}{
		// The documented example from cleat/version.go. Both forms failed.
		{map[string]string{"llm": ">=1.2.0"}, true, "the documented example; 1.3.0 satisfies it"},
		{map[string]string{"blobstore": "~2.0.0"}, true, "the documented example; 2.0.0 satisfies it"},

		{map[string]string{"llm": "^1.0.0"}, true, "caret: 1.3.0 is in [1.0.0, 2.0.0)"},
		{map[string]string{"llm": "~1.3.0"}, true, "tilde: 1.3.0 is in [1.3.0, 1.4.0)"},
		{map[string]string{"llm": "=1.3.0"}, true, "explicit exact, matching"},
		{map[string]string{"llm": "1.3.0"}, true, "bare exact, matching -- the one form that always worked"},
		{map[string]string{"llm": ""}, true, "no constraint means any installed version"},
		{map[string]string{"llm": "*"}, true, "explicit wildcard"},

		// Still refused. A constraint the worker genuinely does not satisfy
		// must keep failing, or the repair has replaced one wrong answer with
		// another.
		{map[string]string{"llm": ">=2.0.0"}, false, "1.3.0 does not satisfy >=2.0.0"},
		{map[string]string{"llm": "^2.0.0"}, false, "1.3.0 is not in [2.0.0, 3.0.0)"},
		{map[string]string{"llm": "~1.2.0"}, false, "1.3.0 is not in [1.2.0, 1.3.0)"},
		{map[string]string{"llm": "1.2.0"}, false, "a bare exact that does not match"},
		{map[string]string{"llm": "=1.2.0"}, false, "an explicit exact that does not match"},
		{map[string]string{"absent": "*"}, false, "a plugin the worker does not have, whatever the constraint"},
	}

	for _, tc := range cases {
		var name, constraint string
		for k, v := range tc.dep {
			name, constraint = k, v
		}
		err := checkPluginDeps(worker, tc.dep)
		gotOK := err == nil
		if gotOK != tc.wantOK {
			verdict := "accepted"
			if !gotOK {
				verdict = "rejected: " + err.Error()
			}
			t.Errorf("worker has %s=%s, workflow declares %q -> %s, want ok=%v\n  %s",
				name, worker[name], constraint, verdict, tc.wantOK, tc.why)
		}
	}
}

// The missing-plugin arm is separate from the version arm, and it must not have
// been widened by the repair: a workflow naming a plugin the worker has not
// loaded fails regardless of how permissive its constraint is. An "any version"
// constraint is the case most likely to slip through a change that treats the
// wildcard as "skip the check".
func TestAWildcardDoesNotExcuseAMissingPlugin(t *testing.T) {
	err := checkPluginDeps(map[string]string{"llm": "1.3.0"}, map[string]string{"blobstore": "*"})
	if err == nil {
		t.Fatal("a wildcard constraint accepted a plugin the worker does not have; " +
			"the wildcard means any installed VERSION, not any plugin")
	}
}
