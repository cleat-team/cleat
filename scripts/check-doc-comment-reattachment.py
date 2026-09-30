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
    2  UNMEASURED -- a ref did not resolve, or the caller's arguments could not
       be told apart from "run it locally with no arguments", so nothing was
       compared. Separate from 1 on purpose; see the comment at the check.
"""
import re
import subprocess
import sys

# Anchored at column 0: a top-level declaration. Anchoring is what keeps this
# out of strings and nested funcs without needing a Go parser -- the same
# "anchor to where the artifact lives, not to what it is called" move
# CLAUDE.md prescribes.
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
#
# A METHOD'S RECEIVER TYPE IS ALSO CAPTURED, separately from the receiver
# VARIABLE, because a bare method name is not unique in Go -- `Unwrap` is
# implemented once per error type by design, and this repo has three separate
# `ClaimWorkflows` for the same reason. Keying on the name alone means a
# second, unrelated method with the same name overwrites the first's entry in
# `documented()`'s map (Python dicts have one slot per key), so the ORIGINAL,
# untouched declaration is reported as having lost its comment when in fact
# the newcomer just collided with it. Found 2026-09-27 (cleat#2480) on
# `engine/runtime.go`'s two `Unwrap` methods, one of the thirteen findings the
# guard produced on the 0.3.0 release PR -- the only one that was not real.
#
# The receiver pattern accepts every shape the repo actually has: a named
# receiver (`s *Store`), an unnamed one (`*fakeDriver`, `auth/fake_driver_test.go`),
# and a generic one (`c *Container[T]`, `testdata/generics/generics.go`;
# `pf *PluginFunc[Req, Resp]`, `cleat/plugin_caller.go`) -- verified present
# with `grep -rn '^func ([a-zA-Z_][a-zA-Z0-9_]* \*[A-Za-z_][A-Za-z0-9_]*\['`
# and `grep -rn '^func (\*[A-Za-z_]'` before assuming only the common form
# needed handling.
DECL = re.compile(
    r'^func\s+(?:\(\s*(?:\w+\s+)?\*?(\w+)(?:\[[^\]]*\])?\s*\)\s*)?(\w+)\b'
    r'|^(?:type|var|const)\s+(\w+)\b'
)


def documented(src):
    """Map each top-level declaration to whether a // block sits directly above it.

    THE MAP KEY is the bare name for a function, type, var or const -- those
    occupy one flat namespace in Go, so the compiler already refuses two of
    them sharing a name and a bare-name key cannot collide by construction.
    A METHOD's key is `(receiver_type, name)` instead, because a method's
    namespace is scoped to its receiver and two methods sharing a name on
    different receivers are not just legal, they are the normal way to
    implement an interface method per type (cleat#2480).

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
                recv, fname, other = m.groups()
                if fname is not None:
                    key = (recv, fname) if recv is not None else fname
                else:
                    key = other
                out[key] = i > 0 and lines[i - 1].startswith('//')
        if not is_comment and line.count('`') % 2 == 1:
            in_raw = not in_raw
    return out


def display_name(key):
    """Render a documented() key back into something a reader recognises."""
    if isinstance(key, tuple):
        recv, name = key
        return f'({recv}).{name}' if recv else name
    return key


def lost_docs(old_src, new_src):
    """Keys documented in old_src that are undocumented (or renamed away) in new_src.

    Factored out of findings() so the self-test can exercise the actual
    comparison the guard makes without going through git at all -- the same
    move CLAUDE.md asks for when a check's setup can obscure what is being
    measured.
    """
    o, n = documented(old_src), documented(new_src)
    return [key for key, had_doc in o.items() if had_doc and key in n and not n[key]]


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
        for key in lost_docs(old, new):
            out.append((f, key))
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

    # A method is keyed on (receiver type, name), not on the bare name --
    # that is the fix for cleat#2480, so this is the positive half of it.
    m = documented('package p\n\n// Foo does a thing.\nfunc (s *Store) Foo() {}\n')
    if not m.get(('Store', 'Foo')):
        print('SELF-TEST FAILED: a method with a receiver is not keyed on (receiver, name)')
        ok = False

    # Two receiver shapes the bare-name fix has to keep working: unnamed
    # (auth/fake_driver_test.go) and generic (testdata/generics/generics.go,
    # cleat/plugin_caller.go) -- verified present in the tree, not assumed.
    unnamed = documented('package p\n\n// Open opens a connection.\nfunc (*fakeDriver) Open() {}\n')
    if not unnamed.get(('fakeDriver', 'Open')):
        print('SELF-TEST FAILED: an unnamed receiver is not keyed on its type')
        ok = False

    generic = documented(
        'package p\n\n// Process runs the container.\nfunc (c *Container[T]) Process() {}\n')
    if not generic.get(('Container', 'Process')):
        print('SELF-TEST FAILED: a generic receiver is not keyed on its bare type name')
        ok = False

    # THE DECIDING TEST (cleat#2480): a second method sharing a NAME but not a
    # RECEIVER must not read as the first one losing its doc comment. This is
    # `engine/runtime.go`'s actual shape -- two `Unwrap` methods, one per error
    # type -- reduced to the minimum that reproduces it. Revert the (receiver,
    # name) keying and this must fail, because the bare-name map overwrites
    # wasmTrapError's entry with GuestReturnedError's when the second is added.
    collision_base = '''package p

// Unwrap lets errors.Is/As see through wasmTrapError to its cause.
func (e *wasmTrapError) Unwrap() error { return e.cause }
'''
    collision_head = '''package p

// Unwrap lets errors.Is/As see through wasmTrapError to its cause.
func (e *wasmTrapError) Unwrap() error { return e.cause }

func (e *GuestReturnedError) Unwrap() error { return e.cause }
'''
    collided = lost_docs(collision_base, collision_head)
    if collided:
        print(f'SELF-TEST FAILED: a same-named method on a NEW receiver was read as an '
              f'EXISTING receiver losing its doc comment: {[display_name(k) for k in collided]!r}')
        ok = False

    # And the fix must not overcorrect: a genuine loss on one receiver has to
    # keep being reported even with a same-named sibling method sitting right
    # beside it -- the known-positive CLAUDE.md requires for exactly this
    # shape of fix, since "reports nothing" is what a broken widening would
    # also do.
    real_loss_base = '''package p

// Unwrap lets errors.Is/As see through wasmTrapError to its cause.
func (e *wasmTrapError) Unwrap() error { return e.cause }

func (e *GuestReturnedError) Unwrap() error { return e.cause }
'''
    real_loss_head = '''package p

func inserted() int { return 0 }

func (e *wasmTrapError) Unwrap() error { return e.cause }

func (e *GuestReturnedError) Unwrap() error { return e.cause }
'''
    real_loss = lost_docs(real_loss_base, real_loss_head)
    if real_loss != [('wasmTrapError', 'Unwrap')]:
        print(f'SELF-TEST FAILED: a genuine loss on one receiver, beside an untouched '
              f'same-named method on another receiver, was not reported correctly: '
              f'{[display_name(k) for k in real_loss]!r}')
        ok = False

    # A `func` inside a string or indented must not register as a declaration.
    n = documented('package p\n\nvar s = `\nfunc notReal() {}\n`\n')
    if 'notReal' in n:
        print('SELF-TEST FAILED: an indented/quoted func registered as a declaration')
        ok = False

    # Known-positive for cleat#2479: a caller that supplied exactly one
    # argument must be refused as UNMEASURED, not silently read as "use the
    # defaults". This needs no git state at all -- the args check runs before
    # any subprocess call -- so it belongs in the self-test rather than in a
    # live-repo probe.
    saved_argv = sys.argv
    try:
        sys.argv = ['check-doc-comment-reattachment.py', 'HEAD']
        rc = main()
        if rc != 2:
            print(f'SELF-TEST FAILED: a lone positional argument returned {rc}, want 2 (UNMEASURED)')
            ok = False
    finally:
        sys.argv = saved_argv

    print('self-test passed' if ok else 'self-test FAILED')
    return 0 if ok else 1


def main():
    if '--self-test' in sys.argv:
        return self_test()

    args = [a for a in sys.argv[1:] if not a.startswith('-')]
    if len(args) == 2:
        base, head = args
    elif len(args) == 0:
        base, head = 'origin/develop', 'HEAD'
    else:
        # Exactly zero positional arguments is "run it by hand from a
        # checkout" -- a real, legitimate caller. Anything else -- one
        # argument, or three or more -- is not a caller who wants the
        # defaults; it is a caller who tried to supply BASE and HEAD and
        # failed. The one that bit CI (cleat#2479): a step invoked with
        #     ${{ github.event.pull_request.base.sha }} HEAD
        # on a push event, where there is no pull_request object, so the
        # expression expands to the empty string and the shell drops it --
        # leaving one argument, "HEAD". That used to fall into the same
        # "anything but 2" branch as a genuine zero-argument call and
        # silently compare origin/develop against HEAD, which are the same
        # commit on a push, so it printed "ok" having compared nothing.
        # Treating "not 0 and not 2" as its own UNMEASURED case closes that
        # regardless of which caller mis-invokes it next.
        print(f'UNMEASURED: expected 0 or 2 positional arguments (BASE HEAD), got '
              f'{len(args)}: {args!r}.')
        print('This is a failure of the check, not a finding about the tree.')
        print('A lone argument usually means a CI expression expanded to the empty')
        print('string (e.g. github.event.pull_request.base.sha on a non-pull_request')
        print('event) and was silently read as "use the defaults" here. Pass BASE')
        print('and HEAD explicitly, or nothing at all for the local default.')
        return 2

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

    for f, key in hits:
        print(f'{f}: `{display_name(key)}` had a doc comment at {base} and has none at {head}.')
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
