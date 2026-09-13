package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// engine.ErrScheduleIdempotentReplay is a SUCCESS travelling as an error, and
// this is the guard that keeps that from being a trap. cleat#1495.
//
// CreateSchedule returns only `error`, and widening it would touch thirteen
// implementations to give one endpoint a capability -- so a successful replay
// arrives at the caller looking exactly like a failure. That is safe today
// because the sentinel is returned ONLY on branches gated by
// `sch.IdempotencyKey != ""`, and only the HTTP handler sets that field. It
// stops being safe the moment a second caller sets a key without learning what
// comes back.
//
// So the rule is asserted rather than described: a function that sets
// Schedule.IdempotencyKey must also mention ErrScheduleIdempotentReplay. The
// population is DERIVED from the source rather than listed, for the same reason
// the replay-flag guard derives its own -- a list covers what its author
// remembered, and the whole failure mode here is a caller nobody remembered.
//
// Note this scans the WHOLE repository, not this package. `go test` caches on
// files opened inside the package directory, so an edit to cmd/cleat/main.go
// does not invalidate this test's cached result and a local `go test ./...` can
// report a stale pass. CI passes -count=1 to every matrix package, so the gate
// is sound there; locally, the tell is the word `(cached)` where a duration
// should be.
func TestOnlyAKeyBearingCallerCanReceiveTheReplaySentinel(t *testing.T) {
	root := repoRoot(t)
	files := trackedGoFilesUnder(t, root)

	funcRe := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)\(`)

	var setsKey, handlesSentinel []string
	callers := 0

	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		src := stripGoComments(string(b))
		if !strings.Contains(src, ".CreateSchedule(") {
			continue
		}
		locs := funcRe.FindAllStringSubmatchIndex(src, -1)
		for i, loc := range locs {
			end := len(src)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			body := src[loc[1]:end]
			if !strings.Contains(body, ".CreateSchedule(") {
				continue
			}
			callers++
			where := rel + ":" + src[loc[2]:loc[3]]
			// The store implementations are not callers in the sense that
			// matters: they PRODUCE the sentinel rather than receive it.
			if strings.HasPrefix(rel, "engine/") && strings.Contains(body, "ValidateForCreate") {
				continue
			}
			if strings.Contains(body, "IdempotencyKey") {
				setsKey = append(setsKey, where)
			}
			if strings.Contains(body, "ErrScheduleIdempotentReplay") {
				handlesSentinel = append(handlesSentinel, where)
			}
		}
	}

	// NON-VACUITY. If the scan stops finding callers, every assertion below is
	// satisfied by an empty set -- which reads exactly like success. There are
	// at least the HTTP handler, `cleat schedule create` and the sharded
	// delegate, so a count under three means the scan is broken, not that the
	// callers are gone.
	if callers < 3 {
		t.Fatalf("found %d functions calling CreateSchedule; at least 3 exist "+
			"(cmd/cleat-worker, cmd/cleat, engine/sharded_store.go). The scan has "+
			"stopped seeing call sites and this test is vacuous rather than passing.",
			callers)
	}

	sort.Strings(setsKey)
	sort.Strings(handlesSentinel)

	var unhandled []string
	for _, f := range setsKey {
		found := false
		for _, h := range handlesSentinel {
			if h == f {
				found = true
				break
			}
		}
		if !found {
			unhandled = append(unhandled, f)
		}
	}

	if len(unhandled) > 0 {
		t.Errorf("these functions set Schedule.IdempotencyKey and never mention "+
			"engine.ErrScheduleIdempotentReplay:\n  %s\n\n"+
			"That sentinel is what CreateSchedule returns when the presented key has "+
			"already created this exact schedule -- a SUCCESS. A caller that sets a key "+
			"and does not handle it reports a successful retry to its user as a failure, "+
			"which is the opposite of what an idempotency key is for.",
			strings.Join(unhandled, "\n  "))
	}

	t.Logf("CreateSchedule callers: %d; key-setting: %v", callers, setsKey)
}

// repoRoot locates the repository so this guard can scan outside its package.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// trackedGoFilesUnder lists tracked non-test .go files for the whole repo.
//
// git ls-files rather than a walk: a walk descends into .claude/worktrees/ --
// a second copy of the repo -- and would attribute a caller to a scratch
// checkout, or vouch for one that is not in the repository at all.
func trackedGoFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "--", "*.go")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, f := range strings.Fields(string(out)) {
		if !strings.HasSuffix(f, "_test.go") {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatal("no tracked .go files found: the scan would pass vacuously")
	}
	return files
}
