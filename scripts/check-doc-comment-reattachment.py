#!/usr/bin/env python3
"""A declaration inserted between a doc comment and the declaration it documents.

    // validateReclaimTimeout refuses a --reclaim-timeout that would reap runs
    // ... twenty lines ...
    // no test. cleat#1717.
    func flushRetryWindowAdvice(...) string {   <-- inserted here
        ...
    }

    func validateReclaimTimeout(...) error {    <-- now undocumented

Go attaches a doc comment by ADJACENCY, so the whole block silently becomes the
newcomer's documentation and the original is left with none. Nothing complains:
gofmt accepts it, `go vet` accepts it, and this repo's golangci-lint config
enables no doc-comment linter (revive and stylecheck are not in the enable list,
and the two rules that would apply -- `exported` and ST1020 -- only cover
EXPORTED names, which neither instance below is).

Measured 2026-09-16 under the repo's own .golangci.yml, with an unused variable
as the known-positive so the silence is a result rather than a vacuous run:

    known-positive (declared and not used)   rc=1, reported
    a doc comment on the wrong declaration   rc=0, silent

WHY IT IS WORTH A GUARD. It is invisible in review -- the diff shows a function
being added, with correct content, in a plausible place -- and invisible in
`git log`, because the comment's text does not change. What changes is which
declaration it is adjacent to. Two independent instances exist in this repo:

  * cleat#1717 / PR #1734, caught by reading the diff by hand.
  * `wasm/exports.go`, merged in f01e3973 and found by this script: a 40-line
    doc block about `emitDispatchBindFailure`'s error channel now documents
    `absentParamMessage`, a string builder, and `emitDispatchBindFailure` has
    no doc at all.

The harm is the one CLAUDE.md names for stale prose: not dead weight, but a
confident, well-formed, wrong answer to the next reader's question.

WHY IT IS DIFF-SCOPED rather than a whole-tree scan, which every other guard in
scripts/ is. A whole-tree predicate has to ask "does this comment look like it
is about a different function", and this repo documents heavily enough that the
answer is routinely yes for legitimate reasons -- a doc that names a collaborator
in its first line is normal here. Measured over the tree: 107 hits for a loose
name-match and 35 for the tightest whole-tree form I could write, and the two I
hand-checked were both legitimate (`callIntentResolver`'s doc discusses the
`ResolveCallIntent` method it declares; `absentParamMessage`'s block genuinely
mentions its collaborator).

The diff form asks a question with a fact behind it instead: did a declaration
that HAD a doc comment end up with none? Over the last 60 commits on develop
that fires once, and the once is a real defect.

WHAT IT DOES NOT CATCH, stated so nobody reads a pass as more than it is:

  * an insertion above a declaration that never had a doc comment -- there is
    nothing to detach, so there is no defect to see.
  * a comment that re-attaches while the original ALSO keeps a doc, which can
    happen if the block is split rather than jumped. Only total loss is checked,
    because "the doc got shorter" is not separable from ordinary editing.
  * anything outside the diff. A defect already on the base branch is invisible
    here by construction; that is what found the `wasm/exports.go` one, run over
    history rather than over a PR.

EXIT STATUS
    0  nothing lost a doc comment
    1  a finding: this tree has one
    2  UNMEASURED -- a ref did not resolve, so nothing was compared. Separate
       from 1 on purpose; see the comment at the check.
"""
import re
import subprocess
import sys

# Anchored at column 0: a top-level declaration. A method's receiver is skipped
# so `func (s *Store) Foo` keys on Foo. Anchoring is what keeps this out of
# strings and nested funcs without needing a Go parser -- the same "anchor to
# where the artifact lives, not to what it is called" move CLAUDE.md prescribes.
#
# `var` AND `const` ARE HERE BECAUSE THEY WERE MISSING, and the guard's own
# author walked into the gap. This read `func|type` until 2026-09-17, so a
# package-level `var` that lost its doc comment was invisible -- and the guard
# ran green over #1824, a PR that did exactly that to `forbiddenJavaPatterns`
# in cmd/cleat/vet_java.go. Go attaches doc comments to a `var` by the same
# adjacency rule it uses for a `func`; nothing about the hazard stops at the
# keyword, only this pattern did.
#
# The widening was measured rather than assumed, because a scan that grows can
# start refusing things that are fine. Over the last 80 commits on develop:
#
#     func|type only              0 findings
#     with var|const              1 finding -- #1824, the true positive
#
# So the whole of the difference is the defect. A name inside a grouped
# `var (` / `const (` block is indented and still does not register, which is
# the conservative direction: untracked, never misreported.
DECL = re.compile(r'^(?:func\s+(?:\([^)]*\)\s*)?(\w+)|(?:type|var|const)\s+(\w+))\b')


def documented(src):
    """Map each top-level declaration name to whether a // block sits directly above it.

    RAW STRING LITERALS ARE SKIPPED, because Go embeds SQL and generated Go in
    backticks and a `func ...` at column 0 inside one is not a declaration. The
    self-test carries that case; it was written as a limitation to document and
    turned out to be worth fixing, which is the argument for writing the awkward
    self-test case before deciding it does not matter.

    Backtick parity is counted on NON-COMMENT lines only. A comment quoting a
    single backtick is common in this repo's prose, and letting one flip the
    parity would swallow every declaration after it -- a silent false NEGATIVE,
    which is the direction a guard must not fail in.
    """
    lines = src.split('\n')
    out = {}
    in_raw = False
    for i, line in enumerate(lines):
        stripped = line.lstrip()
        is_comment = stripped.startswith('//')
        if not in_raw and not is_comment:
            m = DECL.match(line)
            if m:
                name = next(g for g in m.groups() if g)
                out[name] = i > 0 and lines[i - 1].startswith('//')
        if not is_comment and line.count('`') % 2 == 1:
            in_raw = not in_raw
    return out


def _show(rev, path):
    r = subprocess.run(['git', 'show', f'{rev}:{path}'],
                       capture_output=True, text=True)
    return r.stdout if r.returncode == 0 else None


def findings(base, head):
    """Declarations that had a doc comment at `base` and have none at `head`."""
    files = subprocess.run(
        ['git', 'diff', '--name-only', f'{base}..{head}', '--', '*.go'],
        capture_output=True, text=True, check=True).stdout.split()
    out = []
    for f in files:
        old, new = _show(base, f), _show(head, f)
        if old is None or new is None:
            continue  # added or deleted outright; nothing was detached
        o, n = documented(old), documented(new)
        for name, had_doc in o.items():
            if had_doc and name in n and not n[name]:
                out.append((f, name))
    return out


SELF_TEST_BEFORE = '''package p

// alpha does the alpha thing, at length, with a paragraph about why.
//
// A SEPARATE FUNCTION so it can be tested.
func alpha() int { return 1 }
'''

# The defect: a function inserted between alpha's doc and alpha.
SELF_TEST_BROKEN = '''package p

// alpha does the alpha thing, at length, with a paragraph about why.
//
// A SEPARATE FUNCTION so it can be tested.
func beta() int { return 2 }

func alpha() int { return 1 }
'''

# The control: the same function added, correctly, below alpha.
SELF_TEST_OK = '''package p

// alpha does the alpha thing, at length, with a paragraph about why.
//
// A SEPARATE FUNCTION so it can be tested.
func alpha() int { return 1 }

// beta does the beta thing.
func beta() int { return 2 }
'''


# A `var` is the case this guard was blind to until 2026-09-17, so it carries a
# known-positive of its own rather than relying on the `func` arms to stand for
# every declaration kind. Reverting DECL to `func|type` fails exactly this arm,
# which is the falsification the widening is worth.
SELF_TEST_VAR_BEFORE = """package p

// table is the list of things, with a paragraph explaining each column.
var table = []string{"a"}
"""

SELF_TEST_VAR_BROKEN = """package p

// table is the list of things, with a paragraph explaining each column.
func helper() int { return 1 }

var table = []string{"a"}
"""

# The control pairs a `const` with a `var` so both spellings are exercised in
# the quiet direction too: a widening that starts reporting correctly-documented
# declarations is the failure mode a known-positive alone cannot see.
SELF_TEST_CONST_OK = """package p

// limit is the ceiling.
const limit = 10

// table is the list of things.
var table = []string{"a"}
"""


def self_test():
    """Both directions: it must FIRE on a known defect and stay quiet on a control.

    The known-positive is the half that is skipped and the half that matters. A
    guard that only proves "it passes a good tree" is satisfied by every broken
    version of itself -- three permissive bugs shipped in one repo guard that way
    (CLAUDE.md, #749).
    """
    ok = True

    broken = documented(SELF_TEST_BROKEN)
    before = documented(SELF_TEST_BEFORE)
    if not (before.get('alpha') and broken.get('alpha') is False):
        print('SELF-TEST FAILED: the detector does not see alpha losing its doc comment')
        ok = False

    clean = documented(SELF_TEST_OK)
    if not clean.get('alpha') or not clean.get('beta'):
        print('SELF-TEST FAILED: the detector reports a correctly documented pair as undocumented')
        ok = False

    # A `var` that lost its doc comment: the case DECL could not see until
    # 2026-09-17. Asserted by NAME rather than by a count, so a widening that
    # happens to report something else does not satisfy it.
    vb, vbroken = documented(SELF_TEST_VAR_BEFORE), documented(SELF_TEST_VAR_BROKEN)
    if not (vb.get('table') and vbroken.get('table') is False):
        print('SELF-TEST FAILED: the detector does not see a var losing its doc comment')
        ok = False

    # ...and the quiet direction, for both spellings.
    c = documented(SELF_TEST_CONST_OK)
    if not c.get('limit') or not c.get('table'):
        print('SELF-TEST FAILED: a correctly documented const/var reads as undocumented')
        ok = False

    # A method's receiver must not become part of the name, or every method
    # reads as a declaration nothing else refers to.
    m = documented('package p\n\n// Foo does a thing.\nfunc (s *Store) Foo() {}\n')
    if not m.get('Foo'):
        print('SELF-TEST FAILED: a method with a receiver is not keyed on its own name')
        ok = False

    # A `func` inside a string or indented must not register as a declaration.
    n = documented('package p\n\nvar s = `\nfunc notReal() {}\n`\n')
    if 'notReal' in n:
        print('SELF-TEST FAILED: an indented/quoted func registered as a declaration')
        ok = False

    print('self-test passed' if ok else 'self-test FAILED')
    return 0 if ok else 1


def main():
    if '--self-test' in sys.argv:
        return self_test()

    args = [a for a in sys.argv[1:] if not a.startswith('-')]
    if len(args) == 2:
        base, head = args
    else:
        base, head = 'origin/develop', 'HEAD'

    # UNMEASURED rather than a verdict. A base ref that does not resolve makes
    # every question below unanswerable, and a check that cannot establish its
    # precondition must not report the reassuring answer (CLAUDE.md).
    #
    # EXIT 2, NOT 1, and that distinction was paid for. Both are failures, so
    # CI fails either way -- but while testing this guard under a shallow clone
    # an unresolvable ref printed UNMEASURED and returned 1, and the harness
    # checking "rc=1 means it fired" passed a run that had measured nothing. A
    # guard whose "I could not look" is indistinguishable from "I found
    # something" sends the next author to inspect a comment that is fine.
    for ref in (base, head):
        if subprocess.run(['git', 'rev-parse', '--verify', '--quiet', f'{ref}^{{commit}}'],
                          capture_output=True).returncode != 0:
            print(f'UNMEASURED: {ref} does not resolve to a commit, so no comparison was made.')
            print('This is a failure of the check, not a finding about the tree.')
            print('Fetch the base commit (git fetch --depth=1 origin <sha>), or pass BASE HEAD.')
            return 2

    # Prefer a real merge-base when one is computable.
    #
    # `github.event.pull_request.base.sha` is the base branch TIP, not the merge
    # base. Paired with the merge ref from the same event those are consistent
    # and `base..head` is exactly the PR's changes -- but they are two values
    # from two sources, and if they ever drift the guard reports somebody else's
    # commit against this author's PR. A guard that blames the wrong person is
    # worse than no guard, because the next occurrence is read as noise.
    #
    # In the shallow clone CI uses there is no common history to compute one, so
    # this falls back rather than failing: `git merge-base` errors, and `base` as
    # given is what the caller meant. Degrading is right here -- the fallback is
    # the value that is correct in the normal case, not a guess.
    mb = subprocess.run(['git', 'merge-base', base, head], capture_output=True, text=True)
    if mb.returncode == 0 and mb.stdout.strip():
        base = mb.stdout.strip()

    hits = findings(base, head)
    if not hits:
        print(f'ok: no declaration lost its doc comment between {base} and {head}')
        return 0

    for f, name in hits:
        print(f'{f}: `{name}` had a doc comment at {base} and has none at {head}.')
    print()
    print('A declaration was very likely inserted between that comment and the declaration')
    print('it documents, so the comment now documents the newcomer. Go attaches doc comments')
    print('by adjacency; gofmt and go vet both accept this.')
    print()
    print('Move the new declaration below the documented one, or give it its own doc comment')
    print('and restore the original\'s. If the doc was deleted deliberately, say so in the')
    print('commit message -- this guard has no way to tell that from the accident.')
    return 1


if __name__ == '__main__':
    sys.exit(main())
