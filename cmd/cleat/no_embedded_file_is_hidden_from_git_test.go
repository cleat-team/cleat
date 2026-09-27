package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNoEmbeddedFileIsHiddenFromGit pins the property cleat#2485 is about: a
// file the COMPILER reads must be a file git will let you COMMIT.
//
// THE DEFECT. `.gitignore` carried `dist/`. A pattern with no slash anywhere
// inside it is not anchored -- git applies it at EVERY depth -- so it also
// matched `cmd/cleat-worker/web/dist/`, the bundle `cmd/cleat-worker/server.go`
// compiles into the worker with `//go:embed web/dist`. That bundle is generated
// by `cd web && npm run build`, and vite names its chunks by CONTENT HASH
// (`assets/index-vw5X-b8v.js`), so every regeneration writes files under names
// no `.gitignore` could have anticipated.
//
// Measured 2026-09-27, simulating a rebuild -- the old chunk removed, a new
// hash written -- with the old unanchored rule in place:
//
//	git status --porcelain cmd/cleat-worker/web/dist/   ->  empty
//	git add -A cmd/cleat-worker/web/dist/
//	    D  assets/index-vw5X-b8v.js        <- the DELETION, staged
//	    (the replacement is ignored, so it is NOT staged)
//
// So the only commit a developer could produce there REMOVES the JS chunk and
// adds nothing in its place. `//go:embed web/dist` compiles happily against
// whatever survived, so the build stays green and the binary ships an
// index.html referencing a file that is not inside it. `release.yml`'s
// "Validate no dirty dist/" step then diffs that same path after rebuilding and
// reports a dirty tree whose stated remedy -- "and commit
// cmd/cleat-worker/web/dist/" -- is the one thing the tree made impossible.
//
// THE EXPOSURE IS TO NEW FILES ONLY, and that is worth being exact about
// because it decides which embed targets this guard is protecting. Git exempts
// TRACKED files from ignore rules, so measured the same day, still under the
// old rule: modifying and `git add`-ing the already-tracked chunk staged it
// normally, while creating a sibling `assets/index-NEWHASH123.js` staged ZERO
// paths. Content-hashed filenames are therefore the whole hazard here -- a
// stable-named tracked file under an ignored directory commits fine, which is
// why `crates/cleat-sdk/src/bin/inject_metadata.rs` (tracked, under the `bin/`
// rule, bumps every release) has never been affected. A newly added file under
// any embed target would be, which is the wider hazard this checks.
//
// WHY NOT "no tracked file may be reported ignored", the obvious assertion.
// Measured the same day: that predicate is ALREADY FALSE on `develop`, for two
// files that have nothing to do with this defect --
//
//	.claude/settings.local.json                   ignored deliberately
//	crates/cleat-sdk/src/bin/inject_metadata.rs   a tracked source file under
//	                                              .gitignore's `bin/` rule
//
// so a guard written that way is RED before it lands, and the first person to
// meet it mutes it. The scoping here is what makes the assertion both true and
// worth having: anything `//go:embed` resolves to is by definition shipped
// inside a binary, so it has to be committable. `inject_metadata.rs` is not
// embedded; this bundle is.
//
// WHAT IT CATCHES, STATED SO IT IS NOT MISTAKEN FOR MORE. At CI time every
// embedded file is tracked, and git exempts tracked files from ignore rules --
// so what this reports is not "a file is missing from the checkout", it is the
// CONDITION under which the next regeneration could not be committed. See the
// note on --no-index below: without that flag the check is vacuous.
func TestNoEmbeddedFileIsHiddenFromGit(t *testing.T) {
	root := repoRootFor(t)

	// ---- (1) What does the compiler embed? ----
	//
	// `go list`, deliberately, rather than a scan for `//go:embed` in the
	// source. This tree writes the directive in three different shapes --
	// `//go:embed web/dist` (a directory), `//go:embed templates/agent/*` (a
	// glob), and `//go:embed postgres mysql mssql` (three targets on one line,
	// migrations/embed.go) -- so any single pattern models one of the three and
	// under-reports the rest silently. `go list` is the compiler's own answer,
	// with globs and directory targets already expanded.
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go list -json ./... failed: %v\n%s\n\n"+
			"This is a failure of the CHECK, not a finding about the tree: the "+
			"embedded files were never enumerated, so nothing was measured.", err, stderr)
	}

	type listedPackage struct {
		ImportPath string
		Dir        string
		EmbedFiles []string
	}

	var embedded []string
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decoding `go list` output: %v\n\n"+
				"This is a failure of the CHECK, not a finding about the tree.", err)
		}
		for _, f := range p.EmbedFiles {
			embedded = append(embedded, filepath.Join(p.Dir, f))
		}
	}

	// ---- (2) Anti-vacuity, read two ways ----
	//
	// A census that enumerates nothing agrees with every tree, including one
	// with the defect restored, so an empty or near-empty result has to be a
	// failure rather than a pass. A loose floor alone would not notice the
	// narrower failure -- `go list` still returning the migrations and the CLI
	// templates while quietly dropping the worker's bundle -- so the second
	// reading names the target this guard was written for. If the dashboard
	// ever stops being embedded, this fires and someone updates it on purpose;
	// that is the intended repair, not lowering it.
	if len(embedded) < 10 {
		t.Fatalf("`go list` reported only %d embedded file(s); there were 44 across "+
			"cmd/cleat-worker, cmd/cleat and migrations on 2026-09-27.\n\n"+
			"A census that finds almost nothing passes vacuously. Re-derive with:\n"+
			"    go list -f '{{$d := .Dir}}{{range .EmbedFiles}}{{$d}}/{{.}}{{\"\\n\"}}{{end}}' ./...\n"+
			"If you did not deliberately remove embeds, fix the enumeration rather "+
			"than lowering this floor.", len(embedded))
	}
	const dashboard = "/cmd/cleat-worker/web/dist/"
	var sawDashboard bool
	for _, f := range embedded {
		if strings.Contains(filepath.ToSlash(f), dashboard) {
			sawDashboard = true
			break
		}
	}
	if !sawDashboard {
		t.Fatalf("the enumeration no longer includes %s, which is the embed target "+
			"cleat#2485 is about.\n\n"+
			"Either the dashboard is embedded from somewhere new -- in which case "+
			"point this check at where it went -- or the enumeration has stopped "+
			"seeing it, in which case this guard is now green on exactly the tree "+
			"it exists to check.", strings.TrimSuffix(strings.TrimPrefix(dashboard, "/"), "/"))
	}

	// ---- (3) Ask git about the RULES, not about these files ----
	//
	// --no-index IS THE ENTIRE CHECK. Without it git exempts tracked files from
	// ignore rules, and in a fresh checkout every file here is tracked, so the
	// command answers "nothing is ignored" on every tree -- including one with
	// `dist/` restored. The flag is what turns the question into "would a file
	// created here be addable by the next regeneration", which is the one that
	// matters, and it is the obvious thing for a future reader to remove as
	// redundant. It is not.
	rel := make([]string, 0, len(embedded))
	for _, abs := range embedded {
		r, err := filepath.Rel(root, abs)
		if err != nil || r == ".." || strings.HasPrefix(r, "../") {
			t.Fatalf("embedded file %q is not under the repo root %q (rel=%q).\n\n"+
				"This is a failure of the CHECK, not a finding about the tree.", abs, root, r)
		}
		rel = append(rel, filepath.ToSlash(r))
	}
	sort.Strings(rel)

	args := append([]string{"check-ignore", "-v", "--no-index", "--"}, rel...)
	cmd = exec.Command("git", args...)
	cmd.Dir = root
	out, err = cmd.Output()

	// Three outcomes, and they must not be collapsed. exit 1 is git's "none of
	// these matched", which is the clean result; exit 0 means at least one did;
	// anything else is git refusing to answer, which is a broken check rather
	// than a clean tree. Reading the last as a pass is the failure this repo
	// has recorded most often, so it is spelled out here rather than inferred
	// from an empty stdout.
	var matches []string
	switch e := err.(type) {
	case nil:
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if line != "" {
				matches = append(matches, line)
			}
		}
	case *exec.ExitError:
		if e.ExitCode() == 1 {
			t.Logf("checked %d embedded file(s); .gitignore hides none of them", len(rel))
			return
		}
		t.Fatalf("git check-ignore exited %d: %s\n\n"+
			"This is a failure of the CHECK, not a finding about the tree.",
			e.ExitCode(), strings.TrimSpace(string(e.Stderr)))
	default:
		t.Fatalf("git check-ignore: %v\n\n"+
			"This is a failure of the CHECK, not a finding about the tree.", err)
	}

	// Name every file and the rule that matched it. A count cannot say which
	// one to fix, and the whole value here is knowing which path went dark.
	var b strings.Builder
	for _, m := range matches {
		rule, path, _ := strings.Cut(m, "\t")
		fmt.Fprintf(&b, "\n  %s\n      matched by %s", path, rule)
	}

	t.Errorf("%d of the %d file(s) the compiler embeds are reported ignored by "+
		".gitignore:%s\n\n"+
		"A NEW file git will not let you add is a file the next regeneration "+
		"cannot commit (git exempts TRACKED files, so edits to existing ones are "+
		"unaffected -- it is newly created paths that vanish). For the worker's "+
		"dashboard bundle that is silent: vite writes a new content-hashed chunk, "+
		"`git add -A` stages the DELETION of the old one and not the addition, and "+
		"`//go:embed web/dist` still compiles -- so the build stays green and the "+
		"shipped index.html points at a chunk that is not in the binary.\n\n"+
		"The repair is on the .gitignore side, not here: an ignore pattern with no "+
		"internal slash matches at every depth, so `dist/` reaches "+
		"cmd/cleat-worker/web/dist/. Anchor it (`/dist/`) or name the path. If a "+
		"new pattern is what introduced this, narrow THAT pattern -- do not relax "+
		"this check. See cleat#2485.", len(matches), len(rel), b.String())
}
