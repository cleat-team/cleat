package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

// rootModulePath is the module five go.mod files in this repo require. The
// SUBMODULE `github.com/cleat-team/cleat/cleat` is a different path and is
// deliberately not covered here -- see the note at the bottom.
const rootModulePath = "github.com/cleat-team/cleat"

// TestEveryRootRequireNamesTheSameVersion pins the property cleat#2497 is
// about, and it is a DIFFERENT QUESTION from the two guards that were already
// passing when the defect existed.
//
// THE DEFECT. On 2026-09-27 `cleat/backendkit/go.mod` required the root at
// v0.2.0 while the other four required v0.3.1. Neither existing guard could
// see it, and neither was wrong:
//
//   - `scripts/check-go-mod-tidy.sh` asks "would `go mod tidy` change this?".
//     Every one of the five carries a `replace` on that path, so the declared
//     version has NO effect on what tidy produces. The module is tidy at any
//     version.
//   - `TestTheRootRequireNamesAPublishedVersion` asks "is this version
//     published?". v0.2.0 IS published. It simply is not current.
//
// So both guards were answering their own question correctly while the tree
// held a pin two releases behind.
//
// WHERE IT CAME FROM, because it is the reason this asserts AGREEMENT rather
// than freshness. cleat#2466 moved all five to v0.2.0 together. The PR that
// bumped them again (cleat#2470) changed FOUR, because the tidy guard flagged
// only the modules tidy wanted to change -- and the fifth was invisible to it.
// The divergence was created by a guard's SCOPE being read as the scope of the
// change. Agreement is the property that was violated, and it is checkable at
// the PR that violates it.
//
// WHY NOT "IS IT CURRENT", which is the question you would rather ask. It
// cannot be a failing check, and the reason is structural rather than
// cautious: the release ordering is require-THEN-tag, so between a root tag
// and the follow-up bump the requires are legitimately one release behind.
// That window is not hypothetical -- on 2026-09-27 v0.3.2 was tagged and every
// require still read v0.3.1 until the bump PR merged. A guard demanding the
// latest reachable tag would be red on `main` for the whole of that window,
// every release, which is how a check gets muted. What IS enforceable is that
// the five agree with each other, since they move together in one PR.
//
// AND THE PUBLISHED CHECK IS ALREADY TRANSITIVE. This does not query the
// proxy: it establishes that all five name the SAME version, and
// TestTheRootRequireNamesAPublishedVersion establishes that `cleat/go.mod`'s
// version is published. Together those cover all five, with one network call
// in the repo rather than five.
func TestEveryRootRequireNamesTheSameVersion(t *testing.T) {
	root := repoRootFor(t)

	// `git ls-files`, not a directory walk: it enumerates TRACKED files, so it
	// cannot pick up a vendored or scratch go.mod, and -- the reason this
	// repo settled on it -- it is immune to `.gitignore`, which a bare
	// recursive `grep` here is not. `.gitignore` carries `bin/`, and
	// `crates/cleat-sdk/src/bin/` holds a tracked file that a repo-root
	// `grep -r` silently skips (cleat#2497's sweep found that the hard way).
	out, err := exec.Command("git", "-C", root, "ls-files", "*go.mod").Output()
	if err != nil {
		t.Fatalf("git ls-files '*go.mod': %v\n"+
			"This is a failure of the check, not a finding about the tree.", err)
	}
	var modFiles []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			modFiles = append(modFiles, line)
		}
	}

	// Anti-vacuity. A scan that finds no go.mod files agrees with every tree,
	// including one where the version is wrong, so it must say so rather than
	// pass. **Measured 2026-09-27: 7 tracked go.mod files, of which 5 require
	// the root** -- re-derive with `git ls-files '*go.mod' | wc -l`, because
	// this number has already been written down wrong once: it was recorded as
	// 9 by conflating the NINE REQUIRES (5 root + 4 submodule) with the files
	// that hold them. A count of a different population, in the paragraph
	// explaining why counts need floors.
	//
	// Adding a module does not trip this. DELETING modules deliberately does,
	// and then the floor is the thing to update -- which is a different
	// instruction from the one below, so say which case you are in.
	if len(modFiles) < 5 {
		t.Fatalf("found only %d tracked go.mod file(s); there were 7 on 2026-09-27, "+
			"of which 5 require the root.\n\n"+
			"A scan that enumerates almost nothing passes vacuously. If you did not "+
			"deliberately remove modules, fix the enumeration rather than lowering "+
			"this floor.", len(modFiles))
	}

	// version -> the files that require the root at it. Using the PARSER, not
	// a pattern: this repo writes a require in TWO shapes, and a pattern that
	// anchors on the module path matches only the `require ( ... )` form while
	// one anchored on `require ` matches only the single-line form. That
	// mistake is how the count came out as four when five existed.
	byVersion := map[string][]string{}
	var required []string
	for _, rel := range modFiles {
		path := filepath.Join(root, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			// Fatal, never a bare return: a file this test cannot read is a
			// failure of the check, and returning early would report the tree
			// as agreeing on the strength of the files it happened to read.
			t.Fatalf("reading %s: %v", rel, err)
		}
		f, err := modfile.Parse(path, data, nil)
		if err != nil {
			t.Fatalf("parsing %s: %v\n\nA go.mod that does not parse is a finding about the tree.", rel, err)
		}
		for _, r := range f.Require {
			if r.Mod.Path == rootModulePath {
				byVersion[r.Mod.Version] = append(byVersion[r.Mod.Version], rel)
				required = append(required, rel)
				break
			}
		}
	}

	if len(required) == 0 {
		t.Fatalf("no go.mod file requires %s.\n\n"+
			"That require is what makes the modules consumable outside this repo; "+
			"removing every one of them is not a way to pass this test.", rootModulePath)
	}

	if len(byVersion) == 1 {
		for v, files := range byVersion {
			t.Logf("%d module(s) require %s at %s: %s",
				len(files), rootModulePath, v, strings.Join(files, ", "))
		}
		return
	}

	// Report the NAME of each disagreeing file, not just the count: a count
	// cannot say which one is wrong, and the whole value here is knowing which
	// module was left behind.
	var versions []string
	for v := range byVersion {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	var b strings.Builder
	for _, v := range versions {
		files := byVersion[v]
		sort.Strings(files)
		b.WriteString("\n  " + v + ":\n")
		for _, f := range files {
			b.WriteString("      " + f + "\n")
		}
	}

	t.Errorf("%d different versions of %s are required by the %d module(s) that require it:%s\n"+
		"\nThese modules are linked by `replace` directives, so they are built "+
		"together and must name the same released version. A disagreement means "+
		"one of them was left behind by a change that moved the others -- which is "+
		"exactly what happened in cleat#2470, where a guard's SCOPE (the modules "+
		"`go mod tidy` wanted to change, which is four) was read as the scope of "+
		"the change (which was five).\n"+
		"\nFix by bumping the lagging file(s), not by relaxing this check. See cleat#2497.",
		len(byVersion), rootModulePath, len(required), b.String())
}

// NOT COVERED HERE, DELIBERATELY: `github.com/cleat-team/cleat/cleat`, the
// submodule. Four modules require it at `v0.0.0`, which the proxy does not
// serve (cleat#1888's placeholder shape). They are inert because all of those
// modules also carry `replace github.com/cleat-team/cleat/cleat => ...` in a
// `replace ( ... )` block, so that require never resolves to the proxy, and
// none of them is a published module. They are consistent with each other
// today. Whether those test modules should pin the submodule at all -- rather
// than at a real `cleat/vX.Y.Z` -- is a separate question with its own
// reasoning, so it is not folded in here.
