package main

// cleat#1315. `cleatctl versions gc` read only --dry-run and then took
// engine.DefaultGCOptions() wholesale, so MinVersionsToKeep=3 and
// MaxVersionAge=30d were compiled in and unreachable from any interface --
// while docs/troubleshooting.md told an operator to "adjust the GC retention
// policy" with two flags that did not exist.
//
// THE REFUSALS MATTER MORE THAN THE OVERRIDES. An unrecognised or malformed
// argument must not be ignored: silently dropping --min-versions=5 would run a
// destructive sweep under a policy the caller did not ask for and print "GC
// complete", which is worse than the inflexibility being fixed.
//
// 0 IS REFUSED, which is the non-obvious case. GarbageCollectVersions treats a
// non-positive MinVersionsToKeep or MaxVersionAge as unset and substitutes the
// default, so accepting 0 would sweep under 3 and 30 days while reporting
// success -- the same silent substitution, reintroduced at the boundary added
// to prevent it.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

func gcPolicyStore() *mockStore {
	old := time.Now().Add(-60 * 24 * time.Hour)
	return &mockStore{
		listWorkflowDefsFn: func(_ context.Context, _ string) ([]engine.WorkflowDef, error) {
			return []engine.WorkflowDef{
				{Name: "wf", Version: 4, Deprecated: false, CreatedAt: time.Now()},
				{Name: "wf", Version: 3, Deprecated: true, CreatedAt: old},
				{Name: "wf", Version: 2, Deprecated: true, CreatedAt: old},
				{Name: "wf", Version: 1, Deprecated: true, CreatedAt: old},
			}, nil
		},
		getActiveInstanceCountsByVersionFn: func(_ context.Context) (map[string]int, error) {
			return map[string]int{}, nil
		},
		purgeWorkflowDefFn: func(_ context.Context, _ string, _ int) error { return nil },
	}
}

func TestGCVersionsEchoesThePolicyItUsed(t *testing.T) {
	// Echoed because the policy is now overridable: a run that removed nothing
	// under --min-versions=50 and one that removed nothing under the default 3
	// are different answers, and the counts alone cannot say which.
	out := captureStdout(t, func() {
		gcVersions(context.Background(), gcPolicyStore(), []string{"--min-versions=2", "--max-age=48h"})
	})
	if !strings.Contains(out, "keep >= 2 versions") {
		t.Errorf("the override is not echoed: %s", out)
	}
	if !strings.Contains(out, "48h") {
		t.Errorf("the max-age override is not echoed: %s", out)
	}
}

func TestGCVersionsUsesTheDefaultPolicyWhenNotOverridden(t *testing.T) {
	// The negative control. Without it, a parser that always applied some
	// override would pass the test above.
	out := captureStdout(t, func() {
		gcVersions(context.Background(), gcPolicyStore(), []string{"--dry-run"})
	})
	if !strings.Contains(out, "keep >= 3 versions") {
		t.Errorf("the documented default (%d) is not what ran: %s",
			engine.DefaultMinVersionsToKeep, out)
	}
	if !strings.Contains(out, "dry run") {
		t.Errorf("--dry-run stopped being honoured: %s", out)
	}
}

func TestGCVersionsRefusesAPolicyItCannotParse(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want string
		why  string
	}{
		{"--min-versions=abc", "invalid --min-versions", "not an integer"},
		{"--min-versions=-1", "invalid --min-versions", "negative keeps nothing"},
		{"--min-versions=0", "treated as unset", "the sweep substitutes the default for 0"},
		{"--max-age=abc", "invalid --max-age", "not a duration"},
		{"--max-age=7", "invalid --max-age", "a bare number is 7ns, not 7 days"},
		{"--max-age=0", "treated as unset", "the sweep substitutes the default for 0"},
		{"--min-verzions=5", "unknown argument", "a typo must not be silently dropped"},
		{"--dryrun", "unknown argument", "a near-miss on the real flag"},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			stderr := withExitPanic(t, func() {
				gcVersions(context.Background(), gcPolicyStore(), []string{tc.arg})
			})
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("`versions gc %s` did not refuse with %q (%s).\n\nstderr: %s",
					tc.arg, tc.want, tc.why, strings.TrimSpace(stderr))
			}
		})
	}
}
