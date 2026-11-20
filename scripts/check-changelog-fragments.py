#!/usr/bin/env python3
"""The changelog-fragment guard (cleat's fragment scheme; see changelog.d/README.md).

    scripts/check-changelog-fragments.py --files-file changed.tsv [--body-file body.txt | --no-body] [--branch NAME]
    scripts/check-changelog-fragments.py --base origin/develop  [--body-file body.txt | --no-body]
    scripts/check-changelog-fragments.py --self-test

Refuses (exit 1):
  1. a malformed fragment in changelog.d/ (all of them, not only the changed ones);
  2. a PR that edits CHANGELOG.md directly, unless it FOLDS (deletes at least one fragment in the same diff) or comes from a release/* or hotfix/* branch;
  3. a PR that changes shipped code and adds no fragment and carries no `no-changelog: <reason>` line in its body.
Exit 2 (UNMEASURED) when it could not establish what changed: no file list, an unreadable body. A guard that could not look must not say OK.
`--no-body` (a merge-queue run, where the PR body does not exist) runs rules 1 and 2 and reports rule 3 as `checked on the pull request`.

The changed-file list is `status<TAB>path` per line (git's --name-status; the CI step builds it from the pull-request files API, which is exact
where `git diff base...HEAD` needs a merge base the shallow checkout lacks).
"""
import argparse
import os
import re
import subprocess
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import changelog_fragments as cf  # noqa: E402

# Paths whose change ships to users. Matched by prefix, then the exclusions below take paths back out.
SHIPPED_PREFIXES = (
    "engine/", "wasm/", "wasmrw/", "plugin/", "plugins/", "auth/", "pluginapi/", "internal/", "cleat/", "cmd/", "migrations/",
    "crates/", "python-sdk/", "packages/", "web/", "charts/", "deploy/", "k8s/", "packaging/", "schemas/",
)
SHIPPED_FILES = ("go.mod", "go.sum", "Dockerfile", ".goreleaser.yml")
NOT_SHIPPED = (
    re.compile(r"(^|/)tests?/"), re.compile(r"(^|/)testdata/"), re.compile(r"_test\.go$"), re.compile(r"(^|/)test_[^/]*\.py$"),
    re.compile(r"(^|/)tests?_[^/]*\.py$"), re.compile(r"\.test\.(ts|js|tsx)$"), re.compile(r"(^|/)(__tests__|e2e|fixtures|conformance)/"),
    re.compile(r"(^|/)(docs?|examples?|benchmarks?)/"), re.compile(r"\.md$"),
)
NO_CHANGELOG_RE = re.compile(r"(?m)^[ \t]*no-changelog:[ \t]*(\S.*?)[ \t]*$")
# The rare legitimate direct edit (correcting a typo or a duplicate in an existing entry) says so, with a reason, on a line of its own.
CHANGELOG_EDIT_RE = re.compile(r"(?m)^[ \t]*changelog-edit:[ \t]*(\S.*?)[ \t]*$")


# GRANDFATHERED PRs. A pull request numbered BELOW this was opened before the fragment scheme existed and edits CHANGELOG.md directly; making each of
# them rebase and be re-reviewed is the exact cost the scheme removes (about a dozen open PRs on 2026-11-19, several with a FINAL OK already posted).
# Such a PR may edit CHANGELOG.md and needs no fragment. New PRs, and grandfathered ones that rebase anyway, use fragments.
# REMOVE this constant (and its two self-test cases) once those PRs have merged, at the 0.6.0 release fold at the latest: it is a debt with a date,
# not a rule. Rule 1 (a malformed fragment) still applies to everyone.
GRANDFATHER_BELOW = 4191
MERGE_QUEUE_REF_RE = re.compile(r"(?:^|/)pr-(\d+)-")


def pr_number_from_queue_ref(ref):
    """A merge-queue run has no pull_request payload; its head ref is `gh-readonly-queue/<base>/pr-<N>-<sha>`. None when it does not parse."""
    m = MERGE_QUEUE_REF_RE.search(ref or "")
    return int(m.group(1)) if m else None


def is_shipped(path):
    if any(r.search(path) for r in NOT_SHIPPED):
        return False
    return path in SHIPPED_FILES or path.startswith(SHIPPED_PREFIXES)


def evaluate(changes, body, branch, pr_number=None):
    """changes: [(status, path)]; body: str or None (None = merge queue). Returns (findings, notes)."""
    findings, notes = [], []
    if pr_number is not None and pr_number < GRANDFATHER_BELOW:
        return findings, ["PR #%d is GRANDFATHERED (opened before the fragment scheme: below #%d): a direct CHANGELOG.md edit is accepted and no fragment is "
                          "required. Use a fragment if you rebase." % (pr_number, GRANDFATHER_BELOW)]
    added = [p for s, p in changes if s in ("A", "added", "renamed", "R", "C", "copied") and p.startswith("changelog.d/") and p != "changelog.d/README.md"]
    # `modified` on an existing fragment counts as touching one; a deleted one is a fold
    deleted = [p for s, p in changes if s in ("D", "removed") and p.startswith("changelog.d/") and p != "changelog.d/README.md"]
    touched = added + [p for s, p in changes if s in ("M", "modified", "changed") and p.startswith("changelog.d/") and p != "changelog.d/README.md"]
    edits_changelog = [p for s, p in changes if p == "CHANGELOG.md"]

    edit_ok = body is not None and CHANGELOG_EDIT_RE.search(body)
    if edits_changelog and not deleted and not (branch or "").startswith(("release/", "hotfix/")) and edit_ok:
        notes.append("changelog-edit escape: %s" % edit_ok.group(1))
    elif edits_changelog and not deleted and not (branch or "").startswith(("release/", "hotfix/")):
        findings.append("this PR edits CHANGELOG.md directly. Add a changelog.d/<issue>-<slug>.md fragment instead (changelog.d/README.md); only a PR "
                        "that folds fragments (deletes at least one in the same diff) or comes from a release/* or hotfix/* branch edits CHANGELOG.md. To correct an "
                        "existing entry (a typo, a duplicate), put `changelog-edit: <reason>` on a line of its own in the PR body.")
    shipped = sorted(p for s, p in changes if s not in ("D", "removed") and is_shipped(p))
    shipped_incl_deleted = sorted(p for s, p in changes if is_shipped(p))
    if not shipped_incl_deleted:
        notes.append("no shipped-code path changed: no fragment needed")
    elif touched or deleted:
        notes.append("%d shipped path(s) changed and a fragment is present" % len(shipped_incl_deleted))
    elif body is None:
        notes.append("rule 3 (a fragment for shipped code): checked on the pull request, not in a merge-queue run")
    else:
        m = NO_CHANGELOG_RE.search(body)
        if m:
            notes.append("no-changelog escape: %s" % m.group(1))
        else:
            findings.append("this PR changes shipped code and adds no changelog.d fragment and has no `no-changelog: <reason>` line in its body. "
                            "Shipped paths counted (first 8 of %d): %s" % (len(shipped_incl_deleted), ", ".join(shipped_incl_deleted[:8])))
    return findings, notes


def read_changes_file(path):
    out = []
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.rstrip("\n")
            if not line:
                continue
            if "\t" not in line:
                raise ValueError("line %r is not `status<TAB>path`" % line)
            s, p = line.split("\t", 1)
            out.append((s.strip(), p.split("\t")[-1].strip()))
    return out


def changes_from_git(base):
    p = subprocess.run(["git", "diff", "--name-status", "--no-renames", base + "...HEAD"], capture_output=True, text=True)
    if p.returncode != 0:
        raise ValueError("git diff %s...HEAD failed: %s" % (base, p.stderr.strip()[:200]))
    return [tuple(l.split("\t", 1)) for l in p.stdout.splitlines() if "\t" in l]


def self_test():
    bad = []

    def run(changes, body="", branch="feature/x"):
        return evaluate(changes, body, branch)

    f, _ = run([("M", "engine/x.go")])
    if not f or "shipped code" not in f[0]:
        bad.append("shipped code with no fragment and no escape was not refused: %r" % f)
    f, _ = run([("M", "engine/x.go"), ("A", "changelog.d/1-x.md")])
    if f:
        bad.append("shipped code WITH a fragment was refused: %r" % f)
    f, _ = run([("M", "engine/x.go")], body="Fixes a thing.\nno-changelog: a pure refactor, no behaviour change\n")
    if f:
        bad.append("the no-changelog escape with a reason was refused: %r" % f)
    f, _ = run([("M", "engine/x.go")], body="no-changelog:   \n")
    if not f:
        bad.append("a no-changelog escape with NO reason was accepted")
    f, _ = run([("M", "engine/x_test.go"), ("M", "docs/a.md"), ("M", ".github/workflows/ci.yml"), ("M", "scripts/x.py"), ("M", "tests/chaos/a.go")])
    if f:
        bad.append("a tests/docs/CI-only change was refused: %r" % f)
    f, _ = run([("M", "CHANGELOG.md"), ("A", "changelog.d/1-x.md")])
    if not f or "edits CHANGELOG.md directly" not in f[0]:
        bad.append("a direct CHANGELOG.md edit was not refused: %r" % f)
    f, _ = run([("M", "CHANGELOG.md")], body="changelog-edit: removes a duplicated entry\n")
    if f:
        bad.append("a direct CHANGELOG.md edit WITH a changelog-edit reason was refused: %r" % f)
    f, _ = run([("M", "CHANGELOG.md")], body="changelog-edit:\n")
    if not f:
        bad.append("a changelog-edit escape with NO reason was accepted")
    f, _ = run([("M", "CHANGELOG.md"), ("D", "changelog.d/1-x.md")])
    if f:
        bad.append("a fold (CHANGELOG.md edited, a fragment deleted) was refused: %r" % f)
    f, _ = run([("M", "CHANGELOG.md")], branch="release/0.6")
    if f:
        bad.append("a release/* branch editing CHANGELOG.md was refused: %r" % f)
    f, n = run([("M", "engine/x.go")], body=None)
    if f or not any("checked on the pull request" in x for x in n):
        bad.append("a merge-queue run (no body) did not report rule 3 as checked on the pull request: %r %r" % (f, n))
    # the grandfather cutoff, BOTH sides of it, and the merge-queue number parse
    f, n = evaluate([("M", "CHANGELOG.md"), ("M", "engine/x.go")], "", "feature/x", GRANDFATHER_BELOW - 1)
    if f or not any("GRANDFATHERED" in x for x in n):
        bad.append("a PR just below the cutoff was not grandfathered: %r %r" % (f, n))
    f, _ = evaluate([("M", "CHANGELOG.md"), ("M", "engine/x.go")], "", "feature/x", GRANDFATHER_BELOW)
    if len(f) != 2:
        bad.append("a PR AT the cutoff was grandfathered (it must not be): %r" % f)
    f, _ = evaluate([("M", "CHANGELOG.md"), ("M", "engine/x.go")], "", "feature/x", None)
    if len(f) != 2:
        bad.append("a PR whose number is unknown was grandfathered (unknown must not be a pass): %r" % f)
    if pr_number_from_queue_ref("gh-readonly-queue/develop/pr-4155-0a2e0b52752a20e1d08fb9c295592302d3144474") != 4155:
        bad.append("the merge-queue ref did not parse to its PR number")
    if pr_number_from_queue_ref("refs/heads/feature/x") is not None or pr_number_from_queue_ref("") is not None:
        bad.append("a ref that is not a merge-queue ref parsed to a number")
    # the fragment reader is part of the guard: a bad file is a finding
    with tempfile.TemporaryDirectory() as d:
        cwd = os.getcwd()
        os.chdir(d)
        os.makedirs("changelog.d")
        open("changelog.d/9-ok.md", "w").write("---\nsection: fixed\n---\n- **ok (cleat#9).**\n")
        open("changelog.d/8-bad.md", "w").write("---\nsection: nonsense\n---\n- **x**\n")
        _, problems = cf.read_all()
        os.chdir(cwd)
        if len(problems) != 1 or "nonsense" not in problems[0]:
            bad.append("a fragment with an unknown section gave %r" % problems)
    if bad:
        print("SELF-TEST FAILED:\n  " + "\n  ".join(bad))
        return 3
    print("OK: check-changelog-fragments self-test (refuses shipped code with no fragment, a direct CHANGELOG.md edit and a reasonless escape; "
          "accepts a fragment, a fold, a release branch, the escape, and a tests/docs/CI-only change)")
    return 0


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--files-file")
    ap.add_argument("--base")
    ap.add_argument("--body-file")
    ap.add_argument("--no-body", action="store_true")
    ap.add_argument("--branch", default=os.environ.get("HEAD_REF", ""))
    ap.add_argument("--pr-number", type=int, help="the pull request's number (a pull_request run)")
    ap.add_argument("--merge-queue-ref", help="a merge-queue run's head ref, gh-readonly-queue/<base>/pr-<N>-<sha>, from which the number is read")
    ap.add_argument("--fragments-only", action="store_true", help="rule 1 only: validate every fragment (a merge-queue or push run, with no pull request to read)")
    ap.add_argument("--self-test", action="store_true")
    a = ap.parse_args(argv)
    if a.self_test:
        return self_test()
    if a.fragments_only:
        frags, problems = cf.read_all()
        if problems:
            print("changelog guard: %d malformed fragment(s)" % len(problems))
            print("\n".join("  - " + x for x in problems))
            return 1
        print("OK: changelog guard, fragments only (%d fragment(s) well formed; rules 2 and 3 are checked on the pull request)" % len(frags))
        return 0
    if bool(a.files_file) == bool(a.base):
        print("UNMEASURED: give exactly one of --files-file and --base. This is a failure of the check, not a finding about the PR.")
        return 2
    try:
        changes = read_changes_file(a.files_file) if a.files_file else changes_from_git(a.base)
    except (OSError, ValueError) as e:
        print("UNMEASURED: could not establish what changed (%s). This is a failure of the check, not a finding about the PR." % e)
        return 2
    if not changes:
        print("UNMEASURED: the changed-file list is empty. A PR changes something; an empty list means the lookup failed, not that nothing changed.")
        return 2
    body = None
    if not a.no_body:
        if not a.body_file:
            print("UNMEASURED: no --body-file and no --no-body. This is a failure of the check, not a finding about the PR.")
            return 2
        try:
            body = open(a.body_file, encoding="utf-8").read()
        except OSError as e:
            print("UNMEASURED: could not read the PR body (%s)." % e)
            return 2
    _, problems = cf.read_all()
    pr_number = a.pr_number if a.pr_number is not None else pr_number_from_queue_ref(a.merge_queue_ref)
    findings, notes = evaluate(changes, body, a.branch, pr_number)
    findings = problems + findings
    for n in notes:
        print("note: " + n)
    if findings:
        print("changelog guard: %d finding(s)" % len(findings))
        for f in findings:
            print("  - " + f)
        return 1
    print("OK: changelog guard (%d changed path(s) read)" % len(changes))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
