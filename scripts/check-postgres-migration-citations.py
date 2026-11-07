#!/usr/bin/env python3
"""Every migrations/<dialect>/NNN[_name.sql] citation must name a migration
that exists now, or appear in the frozen baseline as a known historical one.

cleat#2725: the postgres schema rebaseline (cleat#2059, cleat#2416) collapsed
every numbered migration into four generated files --
migrations/postgres/001_schema.sql, 002_defaults.sql, 003_procedures.sql,
004_scope_plugin_grants_to_the_install.sql. A citation of any other numbered
filename is now dangling by construction, and 43 of them accumulated across
the tree with nothing to catch the next one. This is that catch: it fails on
any NEW dangling citation, not just the ones fixed in the PR that added this
guard.

cleat#2817: this used to be scoped to migrations/postgres/ only, by design --
"MySQL and SQL Server were compacted the same week (cleat#2435, cleat#2438)
and likely carry the same defect ... a second sweep, not something this guard
covers." Confirmed live 2026-09-30, twice: `plugin/migration.go` cited
`migrations/mssql/074` (no filename, no .sql -- the rebaseline deleted it)
twice, uncaught because this guard never ran against migrations/mssql/; and a
rewritten citation on #2795 read "the plugin sweep, migration 073" -- no such
migration exists in the CURRENT tree of ANY dialect -- uncaught even within
the Postgres scope, because the guard only matched the
`migrations/postgres/NNN_name.sql` path form, not a bare "migration NNN".

FACT OVER FORM, extraction side. Both gaps above are the same shape: a
citation shape the pattern did not enumerate. Rather than adding
"migrations/mysql/..." and "migrations/mssql/..." as two more alternatives to
a list that will need a fourth branch the next time someone writes a citation
differently (the same mistake cleat#2807 made once with a punctuation wrapper
list before converging on a prefix class), this extracts the FACT every
citation shape is actually making -- a migration NUMBER, optionally scoped to
a DIALECT, optionally naming a full FILENAME -- and checks that fact against
what exists.

VALIDATION SIDE -- REVISED after a real defect found in review (cleat-review
and coordinator, 2026-09-30, both independently against the same measurement).
The first version of this widening replaced the per-citation ALLOWLIST with a
frozen HISTORICAL_NUMBERS table -- "a citation is not dangling if the NUMBER
it names was ever real for that dialect, not just if it's real now" -- and
that is wrong, not merely incomplete: EVERY stale citation of a retired
migration names a number that was once real, because that is what makes it
stale rather than fictitious. So a predicate keyed on "was this number ever
real" is true of the entire class of citation this guard exists to catch, and
cannot discriminate a single member of it. Measured concretely: at the
widened guard's own head, `plugin/migration.go` still had two live, unfixed
`migrations/mssql/074` citations (074 was genuinely retired, so the guard's
own HISTORICAL_NUMBERS table exempted it) and the guard printed "OK". A
freshly-written PRESENT-TENSE probe -- "migrations/postgres/073_plugin_sweep
.sql defines admin.in_flight_workflow_ids today" -- passed too, though no
such file has ever existed in the current tree; 073 was once a real Postgres
number for a *different* file, and the number-only check could not tell the
two apart. Same shape as cleat#2807 one review round earlier: a class-wide
property cannot discriminate within the class it is drawn from.

The fix restores what actually discriminates: a FROZEN BASELINE of the exact
(file, cited fact) pairs already known to be legitimate -- not a property of
the number, but a pinned set of specific citations. A NEW citation of a
retired number, in a file not already in the baseline, still fails: see
`--self-test` assertions 15-17, which reproduce the three probe sentences
the review finding was measured with, including the case that actually
discriminates -- baselining `plugin/migration.go`'s `migrations/mssql/074`
pair must not excuse a *different* file citing the same number.

Cited-review review also proposed leaving `plugin/migration.go`'s own two
`migrations/mssql/074` citations OUT of the generated baseline entirely, so
the guard's main-mode check would visibly refuse them until cleat#2816 fixed
the citations themselves. That is not what this PR did: `--update`'s output
was written verbatim, including that pair, because withholding it by hand
would have coupled this PR's mergeability to cleat#2816's merge order and
landed this guard with CI red over a citation this PR did not introduce and
was not scoped to fix (CLAUDE.md: "never merge on red", "one PR, one
thing"). Assertions 15-17 exercise the exact regression using that pair
synthetically instead -- they do not depend on `plugin/migration.go`'s
current content, so they held before AND after cleat#2816 landed.

**cleat#2816 merged 2026-09-30 (0ba61d98), and the prediction above was
confirmed rather than assumed: the very next `--update`, run as part of this
PR's own rebase onto that commit, stopped regenerating the entry** --
291 -> 290 baseline entries, the single line removed being exactly
`plugin/migration.go	migrations/mssql/074`. Nothing here needed a hand edit
either way, which was the point of not hand-stripping it in the first
place.

Generating the baseline by hand for the ~289 legitimate historical citations
this widening surfaced (checked individually against each dialect's own git
history) would be the "sweep, not a mechanism" CLAUDE.md warns against, so
it is machine-generated:

  scripts/check-postgres-migration-citations.py --update

writes every currently-dangling (file, cited fact) pair to
scripts/migration-citation-baseline.txt, sorted, one per line, tab-separated
-- the same shape as scripts/deadexports-baseline.txt. A baseline entry that
no longer appears as dangling (the citation was fixed, or removed) is
reported STALE and fails the build, the same as before.

A SEPARATE, PRE-EXISTING DEFECT, found by coordinator's suggested self-test
assertion (2026-09-30) rather than by anything about HISTORICAL_NUMBERS:
`git_tracked_matches()`'s `git grep -o` truncated every real citation to the
substring GREP_PATTERN matched -- which stops at the 3-digit number -- so a
citation's trailing `_name.sql` was invisible to extract_citations() on
EVERY real scan, before this PR and after it, until fixed here. Every
assertion above it in this function injects `matches` tuples directly into
check()/is_real() and never calls git_tracked_matches() at all, so none of
them could have caught it: a fixture that supplies its own input cannot
fail on the input path. When adding a new self-test fixture, ask whether it
INJECTS a match or DISCOVERS one from the real tree -- a suite that is all
injection has this exact blind spot, on whatever pipeline stage nothing
injects past.

cleat#3666 -- WHAT A STALE ENTRY MEANS. The union rule above is deliberate
(`--self-test` case 12), and it has one consequence that is not: a number
another dialect retired and a later migration REUSES makes every citation of
that number resolve to the new, unrelated file. The baseline entry for each
then goes stale, and the guard said "the citation was fixed" about citations
nobody had touched. `--update` believed it and dropped them. That is how six
SQL Server comments citing `migration 012` for the `cleat_admin` role came to
cite MySQL's `012_workflow_names_are_compared_exactly.sql` (cleat#3657,
cleat#3660), and the 004 collisions before it did the same to dialect-scoped
`migrations/postgres/004` (cleat#1986, cleat#3360, f6310d9f).

What discriminates is whether the citation TEXT is still in the tree. A stale
entry whose (file, fact) is gone was fixed. One whose (file, fact) is still
extracted, unchanged, was RESOLVED UNDER: nothing was fixed, a migration was
added beneath it. Over the baseline's whole history on develop
(`git log -- scripts/migration-citation-baseline.txt`, 9 removal events,
measured 2026-11-07) that predicate fires on 6 of the 6 entries #3657's
`--update` dropped, on 0 of the 22 removed because someone fixed the citation,
and on one benign event (the 004 prose in engine/routine_definition_drift_test.go,
which describes the old bare form). Neither of the two rules this replaced
did better: "a number real in exactly one dialect is suspicious" flags
legitimate readers (9 on 2026-11-07, 38 on 7cfc78f8 -- a census, moving), and
"infer the dialect from the calling file's path" reaches 2 of the 6.

The guard reports that class separately, names what it now resolves to, and
`--update` refuses to drop it unless `--accept-resolved` is passed, in which
case it prints a block, every dropped baseline line, for the PR body. The
reviewer's check is mechanical: the block and the baseline lines the diff
removes are the same set. `is_real()` is unchanged. What this cannot reach: a
citation that resolved falsely from the start never entered the baseline, so
nothing flips.

Usage:
  scripts/check-postgres-migration-citations.py --self-test
  scripts/check-postgres-migration-citations.py
  scripts/check-postgres-migration-citations.py --update
  scripts/check-postgres-migration-citations.py --update --accept-resolved
"""
import contextlib
import io
import os
import re
import subprocess
import sys
import tempfile

SELF_PATH = "scripts/check-postgres-migration-citations.py"
BASELINE_PATH = "scripts/migration-citation-baseline.txt"

DIALECTS = ("postgres", "mysql", "mssql")

# Loose, grep -E-compatible: selects candidate LINES only. Precise extraction
# (dialect / number / filename) happens in Python, in extract_citations
# below -- the same two-stage shape check() and git_tracked_matches() already
# had, kept because git grep is fast and its own regex engine's group
# semantics should not be trusted across engines (this file's own history:
# see git_tracked_matches's comment).
GREP_PATTERN = (
    r'migrations/(postgres|mysql|mssql)/[0-9]{3}'
    r'|migration `?[0-9]{3}'
)

# Precise, Python re. Order matters: the filename-bearing alternative for a
# given anchor (path or word) must be tried before that anchor's bare-number
# alternative, so "migrations/mssql/074_x.sql" is captured WITH its filename
# and not also, separately, as a bare "074" -- re.finditer does not re-try an
# already-consumed span, so whichever alternative matches at a start
# position wins outright, and this only matters within one PATTERN object,
# which is why the two anchors below are separate compiled patterns rather
# than one alternation (a single object would still short-circuit correctly,
# but keeping the anchors apart makes the dialect-known/dialect-unknown
# split explicit rather than implicit in match position).
PATH_CITATION = re.compile(
    r'migrations/(?P<dialect>postgres|mysql|mssql)/(?P<num>[0-9]{3})(?P<name>_[A-Za-z_]+\.sql)?'
)
WORD_CITATION = re.compile(
    r'migration `?(?P<num>[0-9]{3})(?P<name>_[A-Za-z_]+\.sql)?`?'
)


def extract_citations(text):
    """Every citation in one line of text, as (dialect_or_None, number, filename_or_None).

    filename is the full NNN_name.sql when the citation names one, else None
    (a bare `migrations/<dialect>/NNN` or bare `migration NNN`).
    """
    out = []
    for m in PATH_CITATION.finditer(text):
        num = m.group("num")
        name = m.group("name")
        out.append((m.group("dialect"), num, (num + name) if name else None))
    for m in WORD_CITATION.finditer(text):
        num = m.group("num")
        name = m.group("name")
        out.append((None, num, (num + name) if name else None))
    return out


# Whole files exempted: dated journals/narrative entries describing history
# at the time they were written (the same way CLAUDE.md's "Ground rules"
# section treats a past-tense justification), plus scripts whose numbered
# filenames are synthetic test fixtures, not citations of real files. This
# script's own path is exempted too -- its docstring quotes dangling
# filenames and numbers by name, on purpose, and is not a citation of them.
EXEMPT_FILE_PREFIXES = (
    "IMPROVEMENT-PLAN.md",
    "IMPROVEMENT-PLAN-CLOSED.md",
    "IMPROVEMENT-PLAN.d/",
    "WORKSTREAM.md",
    "REVIEW-2026-08-09.md",
    "CLAUDE.md",
    "CHANGELOG.md",
    "docs/playbooks/",
    "scripts/check-migration-versions.sh",
    "scripts/check_migration_numbers.py",
    "scripts/check-entity-contract.py",
    "scripts/gen-postgres-baseline.py",
    SELF_PATH,
    # The baseline file's own content IS 289 lines each containing a
    # dangling citation's cited fact ("migrations/mssql/074", "migration
    # 073", ...) by construction -- without this, becoming a TRACKED file
    # (git grep only scans tracked files, so this was invisible until the
    # first `git add`) turns every entry into a second, self-referential
    # "citation" whose path is the baseline file itself, all newly dangling
    # against an empty second-order baseline. Measured: 108 of them, the
    # moment the baseline file was staged.
    BASELINE_PATH,
)

# NOTE, historical: scripts/migration-citation-baseline.txt used to carry a
# `plugin/migration.go` entry citing `migrations/mssql/074` twice, because
# 074 did not exist in mssql's current tree at the time this guard was
# written. cleat#2816 (merged 2026-09-30, 0ba61d98) fixed those citations in
# plugin/migration.go itself, and the very next `--update` after that merge
# stopped regenerating the entry -- confirmed, not assumed: 291 -> 290
# baseline entries, this exact pair the one line removed. See the module
# docstring's "VALIDATION SIDE -- REVISED" section for why this guard never
# hand-stripped it while it was still live, and --self-test's assertions
# 15-17 for how the regression it stood in for (a once-real number silently
# excusing ANY citation of it) is tested instead, synthetically -- those
# assertions do not depend on this entry's presence and still pass now that
# it is gone.
#
# ALLOWLIST itself is gone as of this PR, replaced by the generated baseline
# above -- the 22-entry hand-maintained dict that used to live here (and the
# 23rd, `docs/reference/worker-config.md`'s "024_cross_tenant_schedules.sql"
# entry, removed by #2795 for the same reason: the citation got fixed for
# real) is now redundant with the baseline `--update` regenerates. This
# rebase is the reason that removal shows up here at all: #2795 landed on
# develop after this branch's original commit and before this rebase, so its
# ALLOWLIST edit had to be reconciled against this PR's much larger rewrite
# of the same region. The underlying fact --
# `docs/reference/worker-config.md` no longer citing that migration -- is
# picked up automatically by the `--update` re-run below; nothing here
# needed a hand edit for it.


def real_files_by_dialect():
    """{dialect: set of migrations/<dialect>/*.sql basenames that exist right now}.

    Derived, not hardcoded: a hardcoded set gives a false positive on the
    first new numbered migration docs/contributor/migrations.md itself
    prescribes adding, since a correct citation of it would still read as
    'not one of the baseline files'.
    """
    out = subprocess.run(
        ["git", "ls-files", "migrations/"],
        capture_output=True, text=True, cwd=".",
    )
    if out.returncode != 0:
        print("UNMEASURED: git ls-files failed: " + out.stderr, file=sys.stderr)
        sys.exit(2)
    files = {d: set() for d in DIALECTS}
    for path in out.stdout.splitlines():
        if not path.endswith(".sql"):
            continue
        parts = path.split("/")
        if len(parts) < 3 or parts[0] != "migrations" or parts[1] not in files:
            continue
        files[parts[1]].add(parts[-1])
    return files


def real_numbers_by_dialect(real_files):
    """{dialect: set of the 3-digit prefixes of that dialect's real files}."""
    numbers = {}
    for dialect, files in real_files.items():
        numbers[dialect] = {f[:3] for f in files if len(f) >= 3 and f[:3].isdigit()}
    return numbers


def git_tracked_matches():
    # git grep enumerates candidate lines fast, over tracked files only (no
    # .gitignore surprises, no rglob descending into a scratch worktree --
    # see CLAUDE.md's "prefer git ls-files over rglob"). Precise extraction
    # (extract_citations) re-parses each candidate line in Python rather
    # than trust git grep -E's own group boundaries across engines. Excludes
    # this script's own path and the baseline file's (see EXEMPT_FILE_PREFIXES's
    # comment on the baseline file -- its content IS 289 lines of cited
    # facts, so leaving it in scope makes it a self-referential input to its
    # own generation).
    #
    # NO -o/--only-matching, and that is load-bearing, not a style choice --
    # found by coordinator's suggested real-discovery self-test assertion
    # (23c below), independent of the HISTORICAL_NUMBERS review round. With
    # -o, git grep prints only the SUBSTRING GREP_PATTERN matched, and that
    # pattern stops at the 3-digit number (it has no filename alternative,
    # unlike PATH_CITATION/WORD_CITATION) -- so `cite` below never contains
    # any text past the number, and a real citation's trailing `_name.sql`
    # is silently invisible to extract_citations() on every real scan,
    # collapsing e.g. "migrations/postgres/005_app_role.sql" to a bare
    # "migrations/postgres/005" fact. Confirmed live: with -o, ALL 289
    # entries this guard generated for its own baseline lost their filename
    # (`grep -c '\.sql$' scripts/migration-citation-baseline.txt` was 0);
    # without it, the well-known engine/rls_check.go citation of
    # 005_app_role.sql extracts with its filename intact. The docstring
    # above ("selects candidate LINES only") already described the intended
    # behavior; -o silently contradicted it.
    raw = subprocess.run(
        ["git", "grep", "-nE", GREP_PATTERN, "--", ".",
         f":!{SELF_PATH}", f":!{BASELINE_PATH}"],
        capture_output=True, text=True, cwd=".",
    )
    if raw.returncode not in (0, 1):
        print("UNMEASURED: git grep failed: " + raw.stderr, file=sys.stderr)
        sys.exit(2)
    results = []
    for line in raw.stdout.splitlines():
        m = re.match(r"^(.*?):(\d+):(.*)$", line)
        if not m:
            continue
        path, lineno, cite = m.group(1), int(m.group(2)), m.group(3)
        for dialect, num, fname in extract_citations(cite):
            results.append((path, lineno, dialect, num, fname))
    return results


def cited_fact(dialect, num, fname):
    """The human-readable key a fixer would search for, and the baseline's
    key. Keyed on the cited fact, not the line number: a line number shifts
    on any unrelated edit above it in the same file, and 25 sites across 18
    files made that a real hazard when this guard was first added (cleat#2725
    review, one comment line added above a citation gave a false positive).
    Keying on the cited fact also collapses a file that legitimately cites
    the same dangling name twice into one entry naturally."""
    if fname:
        return fname
    if dialect:
        return f"migrations/{dialect}/{num}"
    return f"migration {num}"


def is_real(dialect, num, fname, real_files, real_numbers):
    """Does the fact this citation makes -- a filename, or just a number,
    scoped to one dialect or left to any -- match a migration that EXISTS
    RIGHT NOW? (Whether it used to exist is the baseline's question, not
    this function's -- see the module docstring on why a number-keyed
    historical exemption cannot discriminate within the class of citation
    this guard exists to catch.)

    A dialect-less citation is checked against the union across all three
    dialects (FACT OVER FORM): the prose does not say which dialect it
    means, so a number real in ANY current dialect is not a false positive
    -- and a number real in none of them fails regardless of which dialect
    the author had in mind.
    """
    dialects = (dialect,) if dialect else DIALECTS
    for d in dialects:
        if fname:
            if fname in real_files[d]:
                return True
        elif num in real_numbers[d]:
            return True
    return False


def load_baseline(path=BASELINE_PATH):
    """{(file, cited fact): True} from the tab-separated baseline file.

    Missing file reads as empty rather than erroring -- a repo that has
    never run --update yet (or a self-test's synthetic fixtures, which pass
    their own baseline dict directly to check()) has nothing to load.
    """
    entries = {}
    try:
        with open(path) as f:
            lines = f.read().splitlines()
    except FileNotFoundError:
        return entries
    for line in lines:
        if not line:
            continue
        path_part, _, fact = line.partition("\t")
        if not fact:
            print(f"UNMEASURED: {path} has a line with no tab separator: {line!r}",
                  file=sys.stderr)
            sys.exit(2)
        entries[(path_part, fact)] = True
    return entries


def compute_dangling(matches, real_files, real_numbers, exempt_prefixes=EXEMPT_FILE_PREFIXES):
    dangling = set()
    for path, _lineno, dialect, num, fname in matches:
        if is_real(dialect, num, fname, real_files, real_numbers):
            continue
        if path.startswith(exempt_prefixes):
            continue
        dangling.add((path, cited_fact(dialect, num, fname)))
    return dangling


def check(matches, real_files, real_numbers, baseline, exempt_prefixes=EXEMPT_FILE_PREFIXES):
    dangling = compute_dangling(matches, real_files, real_numbers, exempt_prefixes)
    unallowed = sorted(dangling - set(baseline.keys()))
    stale = sorted(set(baseline.keys()) - dangling)
    return unallowed, stale


def present_citations(matches, exempt_prefixes=EXEMPT_FILE_PREFIXES):
    """{(path, cited fact): [(lineno, dialect, num, fname), ...]} for every
    citation the scan extracts from a non-exempt file, resolved or not.

    This is the other half of what `check` compares: `dangling` says which
    citations do not resolve, this says which citation TEXT is still there.
    Their difference is what separates a stale baseline entry whose citation
    was edited from one whose citation was left alone and has since started
    to resolve (cleat#3666).
    """
    out = {}
    for path, lineno, dialect, num, fname in matches:
        if path.startswith(exempt_prefixes):
            continue
        out.setdefault((path, cited_fact(dialect, num, fname)), []).append(
            (lineno, dialect, num, fname))
    return out


def resolves_to(dialect, num, fname, real_files):
    """The migration files, as 'dialect/NNN_name.sql', that this citation
    resolves to right now -- the same predicate as is_real(), but naming what
    satisfied it instead of saying that something did."""
    out = []
    for d in ((dialect,) if dialect else DIALECTS):
        for f in sorted(real_files[d]):
            if (f == fname) if fname else (f[:3] == num):
                out.append(f"{d}/{f}")
    return out


def classify_stale(stale, matches, real_files, exempt_prefixes=EXEMPT_FILE_PREFIXES):
    """Split stale baseline entries (cleat#3666) into
      fixed    -- the citation text is gone: someone edited it.
      resolved -- the citation text is STILL in the tree and now resolves.
                  {(file, fact): [files it resolves to]}; nothing was fixed,
                  a migration was added under an unchanged citation.

    The second class is the event the union rule in is_real() can cause: a
    number another dialect retires and a later migration reuses makes every
    bare citation of it resolve to an unrelated file. Reporting it as "the
    citation was fixed" told the author the opposite of what happened.
    """
    present = present_citations(matches, exempt_prefixes)
    fixed, resolved = [], {}
    for entry in stale:
        cites = present.get(entry)
        if not cites:
            fixed.append(entry)
            continue
        targets = sorted({x for _l, d, n, f in cites
                          for x in resolves_to(d, n, f, real_files)})
        if not targets:
            # A stale entry is one that is NOT dangling, so a citation still
            # in the tree must resolve to something. If it does not, this
            # function was handed an entry that is not stale, or is_real()
            # and resolves_to() have drifted apart. Say that rather than
            # report an acceptance that names nothing.
            print(f"UNMEASURED: {entry[0]}: {entry[1]} is neither dangling nor "
                  "resolvable, so it cannot be classified.", file=sys.stderr)
            print("this is a failure of the check, not a finding about the tree",
                  file=sys.stderr)
            sys.exit(2)
        resolved[entry] = targets
    return fixed, resolved


def line_text(path, lineno):
    """The source line a citation sits on, for the acceptance block; empty if
    the file cannot be read (the block is evidence for a reviewer, not input)."""
    try:
        with open(path) as f:
            return f.read().splitlines()[lineno - 1].strip()[:140]
    except (OSError, IndexError, UnicodeDecodeError):
        return ""


def format_acceptance_block(dropped, resolved, matches, exempt_prefixes=EXEMPT_FILE_PREFIXES):
    """The block `--update --accept-resolved` prints for the PR body.

    Every dropped baseline line appears, in baseline format in its first two
    tab-separated fields, so a reviewer can compare the block with the lines
    the diff removes one for one. Entries that were plain fixes are tagged
    `citation-removed`; accepted ones carry what they now resolve to and the
    line that still cites them -- the part a reviewer has to judge.
    """
    present = present_citations(matches, exempt_prefixes)
    lines = ["--- paste this block into the PR body "
             "(review: the baseline lines this diff removes must match it) ---",
             f"dropped from {BASELINE_PATH}:"]
    for path, fact in sorted(dropped):
        if (path, fact) in resolved:
            lineno = present[(path, fact)][0][0]
            lines.append("\t".join([
                path, fact, "resolves-to: " + ", ".join(resolved[(path, fact)]),
                f"{path}:{lineno}: {line_text(path, lineno)}"]))
        else:
            lines.append("\t".join([path, fact, "citation-removed"]))
    lines.append("--- end ---")
    return "\n".join(lines)


def print_resolved(resolved, matches, exempt_prefixes=EXEMPT_FILE_PREFIXES):
    present = present_citations(matches, exempt_prefixes)
    for (path, fact), targets in sorted(resolved.items()):
        lineno = present[(path, fact)][0][0]
        print(f"  {path}\t{fact}\tresolves now to {', '.join(targets)}")
        print(f"      still cited at {path}:{lineno}: {line_text(path, lineno)}")


RESOLVED_ADVICE = (
    "Nothing was fixed here: the citation text is unchanged, and a migration was "
    "added under it.\n"
    "Either that migration is NOT what the comment meant (a number another dialect "
    "retired was reused) -- then reword the citation to name the file it means, or "
    "what carries the behaviour, and rerun --update.\n"
    "Or it IS what was meant -- then run `{cmd} --update --accept-resolved`, which "
    "drops the entries and prints a block; paste that block into the PR body."
)


def self_test_resolved_under_baseline():
    """cleat#3666, end to end on a throwaway git repo: a baseline entry whose
    citation text does not change, but which a NEW migration makes resolve.

    Runs this script as a subprocess against a tree it discovers itself, and
    asserts on the printed TEXT as well as the exit status: a status-only
    case certifies a guard that has lost exactly the message it is named for
    (the old stale path also exits 1, and also leaves the baseline alone in
    main mode).  Returns a list of failure strings; empty means every case
    held.
    """
    failures = []
    script = os.path.abspath(__file__)
    with tempfile.TemporaryDirectory() as tmp:
        def git(*args):
            r = subprocess.run(["git", *args], cwd=tmp, capture_output=True, text=True)
            if r.returncode != 0:
                print("UNMEASURED: self-test fixture setup failed: git "
                      + " ".join(args) + ": " + r.stderr, file=sys.stderr)
                print("this is a failure of the check, not a finding about the tree",
                      file=sys.stderr)
                sys.exit(2)

        def write(rel, text):
            full = os.path.join(tmp, rel)
            os.makedirs(os.path.dirname(full), exist_ok=True)
            with open(full, "w") as f:
                f.write(text)

        def run(*flags):
            r = subprocess.run([sys.executable, script, *flags], cwd=tmp,
                               capture_output=True, text=True)
            return r.returncode, r.stdout + r.stderr

        def baseline():
            with open(os.path.join(tmp, BASELINE_PATH)) as f:
                return f.read()

        def block_entries(out):
            """The (file, fact) pairs of the paste-into-the-PR-body block."""
            lines = out.splitlines()
            try:
                start = next(i for i, l in enumerate(lines) if l.startswith("--- paste"))
                end = next(i for i, l in enumerate(lines) if l.startswith("--- end"))
            except StopIteration:
                return None
            body = [l for l in lines[start + 1:end] if "\t" in l]
            return sorted(tuple(l.split("\t")[:2]) for l in body)

        git("init", "-q")
        for d in DIALECTS:
            write(f"migrations/{d}/001_schema.sql", "-- schema\n")
        write("src/a.go", "// the role is made by migration 012\n")
        write("src/b.go", "// nothing is cited here any more\n")
        entry_a = ("src/a.go", "migration 012")
        entry_b = ("src/b.go", "migration 013")
        base_a = "src/a.go\tmigration 012\n"
        base_b = "src/b.go\tmigration 013\n"
        write(BASELINE_PATH, base_a)
        git("add", "-A")

        # Precondition: with 012 real nowhere the entry is dangling and baselined.
        rc, out = run()
        if rc != 0:
            failures.append(f"precondition: the fixture tree should be clean, rc={rc}: {out!r}")
            return failures

        # 1. A migration at that number appears in ANOTHER dialect. Nothing in
        #    src/a.go changed.
        write("migrations/mysql/012_x.sql", "-- unrelated\n")
        git("add", "-A")
        rc, out = run()
        if rc != 1:
            failures.append(f"main mode: rc={rc}, want 1: {out!r}")
        if "resolves now to" not in out or "mysql/012_x.sql" not in out:
            failures.append("main mode did not say the citation resolves now, "
                            f"or did not name the file it resolves to: {out!r}")
        if "the citation was fixed" in out:
            failures.append("main mode told the author the citation was fixed, "
                            f"about a citation nobody changed: {out!r}")

        # 2. --update must refuse, write nothing, and show what it would drop.
        before = baseline()
        rc, out = run("--update")
        if rc != 1:
            failures.append(f"--update without the flag: rc={rc}, want 1: {out!r}")
        if baseline() != before:
            failures.append("--update without the flag rewrote the baseline")
        if "--accept-resolved" not in out or "src/a.go\tmigration 012" not in out:
            failures.append("--update's refusal did not name the flag or the entry it "
                            f"would drop: {out!r}")

        # 3. With the flag it drops, and the block names EXACTLY the dropped entries.
        rc, out = run("--update", "--accept-resolved")
        if rc != 0:
            failures.append(f"--update --accept-resolved: rc={rc}, want 0: {out!r}")
        if baseline() != "":
            failures.append(f"the accepted entry was not dropped: {baseline()!r}")
        if block_entries(out) != [entry_a]:
            failures.append("the pasted block does not name exactly the dropped "
                            f"entries: got {block_entries(out)!r}, want {[entry_a]!r}")
        if "mysql/012_x.sql" not in out:
            failures.append(f"the block did not name the file it resolves to: {out!r}")
        rc, out = run()
        if rc != 0:
            failures.append(f"main mode after acceptance: rc={rc}, want 0: {out!r}")

        # 4. Control: the citation really is fixed. No new class, no flag, no block.
        write(BASELINE_PATH, base_b)
        git("add", "-A")
        rc, out = run()
        if rc != 1 or "resolves now to" in out:
            failures.append(f"a fixed citation read as resolved-under-it: rc={rc} {out!r}")
        rc, out = run("--update")
        if rc != 0 or baseline() != "":
            failures.append(f"--update on a fixed citation: rc={rc} baseline={baseline()!r}")
        if "--- paste" in out:
            failures.append(f"a plain fix printed an acceptance block: {out!r}")

        # 5. Mixed: one resolved, one fixed. The resolved one blocks --update
        #    for both (nothing is half-written); with the flag the block lists
        #    BOTH dropped lines, so it matches the diff line for line.
        write(BASELINE_PATH, base_a + base_b)
        git("add", "-A")
        before = baseline()
        rc, out = run("--update")
        if rc != 1 or baseline() != before:
            failures.append(f"mixed --update without the flag: rc={rc}, baseline changed="
                            f"{baseline() != before}")
        rc, out = run("--update", "--accept-resolved")
        if rc != 0 or block_entries(out) != sorted([entry_a, entry_b]):
            failures.append("mixed block does not match the dropped lines: "
                            f"rc={rc} got {block_entries(out)!r}")
    return failures


def self_test():
    ok = True
    real_files = {
        "postgres": {"001_schema.sql", "002_defaults.sql", "003_procedures.sql",
                      "004_scope_plugin_grants_to_the_install.sql"},
        "mysql": {"001_schema.sql", "002_defaults.sql", "003_procedures.sql"},
        "mssql": {"001_schema.sql", "002_defaults.sql", "003_procedures.sql"},
    }
    real_numbers = real_numbers_by_dialect(real_files)

    def run(matches, baseline=None):
        return check(matches, real_files, real_numbers, baseline or {})

    # 1. A citation of a real baseline file must not be flagged.
    unallowed, _stale = run([("docs/x.md", 1, "postgres", "001", "001_schema.sql")])
    if unallowed:
        print("self-test FAIL: a real-file citation was flagged", file=sys.stderr)
        ok = False

    # 2. A citation of a dangling file, not in the baseline, must be flagged.
    unallowed, _stale = run([("docs/x.md", 1, "postgres", "099", "099_made_up.sql")])
    if unallowed != [("docs/x.md", "099_made_up.sql")]:
        print("self-test FAIL: a dangling citation was not flagged", file=sys.stderr)
        ok = False

    # 3. A citation of a dangling file that IS in the baseline must not be
    #    flagged.
    matches = [("docs/x.md", 1, "postgres", "099", "099_made_up.sql")]
    unallowed, stale = run(matches, baseline={("docs/x.md", "099_made_up.sql"): True})
    if unallowed:
        print("self-test FAIL: a baselined citation was flagged", file=sys.stderr)
        ok = False
    if stale:
        print("self-test FAIL: a matching baseline entry read as stale", file=sys.stderr)
        ok = False

    # 4. A baseline entry whose (file, name) no longer appears as a dangling
    #    citation must be reported stale.
    matches = [("docs/x.md", 1, "postgres", "001", "001_schema.sql")]
    _unallowed, stale = run(matches, baseline={("docs/x.md", "099_made_up.sql"): True})
    if stale != [("docs/x.md", "099_made_up.sql")]:
        print("self-test FAIL: a stale baseline entry was not caught", file=sys.stderr)
        ok = False

    # 5. A file-prefix exemption must suppress the match entirely (so it is
    #    not counted as dangling AND does not need a baseline entry).
    unallowed, _stale = run([("CHANGELOG.md", 1, "postgres", "099", "099_made_up.sql")])
    if unallowed:
        print("self-test FAIL: an exempt-prefix file was flagged", file=sys.stderr)
        ok = False

    # 6. A line number shifting above a baselined citation (an unrelated edit
    #    elsewhere in the same file) must NOT produce a false positive.
    matches = [("docs/x.md", 99, "postgres", "099", "099_made_up.sql")]
    unallowed, stale = run(matches, baseline={("docs/x.md", "099_made_up.sql"): True})
    if unallowed or stale:
        print("self-test FAIL: a shifted line number broke a baseline match",
              file=sys.stderr)
        ok = False

    # --- cleat#2817: MySQL and SQL Server path-form citations ---

    # 7. A dangling MySQL path citation (dialect known) must be flagged
    #    against MYSQL's real files, not Postgres's.
    unallowed, _stale = run([("docs/x.md", 1, "mysql", "099", "099_made_up.sql")])
    if unallowed != [("docs/x.md", "099_made_up.sql")]:
        print("self-test FAIL: a dangling mysql citation was not flagged", file=sys.stderr)
        ok = False

    # 8. A real MySQL file must not be flagged just because it is not a
    #    Postgres one -- proves the check is genuinely per-dialect, not
    #    Postgres's set applied everywhere.
    unallowed, _stale = run([("docs/x.md", 1, "mysql", "001", "001_schema.sql")])
    if unallowed:
        print("self-test FAIL: a real mysql file was flagged against the wrong dialect's set",
              file=sys.stderr)
        ok = False

    # 9. cleat#2817's own instance 1: a dialect-scoped citation with NO
    #    filename at all (`migrations/mssql/074`) must still be checked, by
    #    number, against that dialect's real numbers -- and, per the review
    #    fix below, must be flagged when it is not in the baseline, even
    #    though 074 was once a real mssql number.
    unallowed, _stale = run([("plugin/migration.go", 944, "mssql", "074", None)])
    if unallowed != [("plugin/migration.go", "migrations/mssql/074")]:
        print("self-test FAIL: a bare dialect+number citation of a deleted "
              "migration was not flagged", file=sys.stderr)
        ok = False

    # 10. The mirror of 9: a bare dialect+number citation of a migration that
    #     DOES exist right now must not be flagged.
    unallowed, _stale = run([("docs/x.md", 1, "mssql", "002", None)])
    if unallowed:
        print("self-test FAIL: a bare dialect+number citation of a real "
              "migration was flagged", file=sys.stderr)
        ok = False

    # --- cleat#2817: bare "migration NNN" phrasing (no dialect, no path) ---

    # 11. cleat#2817's own instance 2: "migration 073", no such migration in
    #     ANY dialect's current tree, must be flagged -- keyed as "migration
    #     073", the text a fixer would search for.
    unallowed, _stale = run([("docs/reference/worker-config.md", 1158, None, "073", None)])
    if unallowed != [("docs/reference/worker-config.md", "migration 073")]:
        print("self-test FAIL: a fabricated bare 'migration NNN' citation was "
              "not flagged", file=sys.stderr)
        ok = False

    # 12. FACT OVER FORM's own case: a dialect-less bare number that exists
    #     in mysql/mssql but NOT in postgres must NOT be flagged -- the
    #     guard cannot tell which dialect the prose meant, so it must not
    #     invent a false positive by assuming Postgres.
    unallowed, _stale = run([("docs/x.md", 1, None, "002", None)])
    if unallowed:
        print("self-test FAIL: a bare number real in some (not all) dialects "
              "was flagged", file=sys.stderr)
        ok = False

    # 13. A dialect-less bare number that is real in EVERY dialect (001) must
    #     also not be flagged -- the ordinary case, not just the edge above.
    unallowed, _stale = run([("docs/x.md", 1, None, "001", None)])
    if unallowed:
        print("self-test FAIL: a bare number real everywhere was flagged", file=sys.stderr)
        ok = False

    # 14. A dialect-less citation that NAMES a filename (backtick form, the
    #     guard's original behavior) must still be checked against the
    #     union of all dialects' filenames, not just Postgres's.
    unallowed, _stale = run([("docs/x.md", 1, None, "001", "001_schema.sql")])
    if unallowed:
        print("self-test FAIL: a real filename (present in every dialect) "
              "cited with no dialect was flagged", file=sys.stderr)
        ok = False

    # --- cleat-review / coordinator, 2026-09-30: the HISTORICAL_NUMBERS
    # regression and its fix. A number-keyed exemption ("this number was
    # once real for this dialect") is true of every genuinely stale citation
    # -- that is what makes it stale rather than fictitious -- so it cannot
    # discriminate a single one of them. These three probes are the exact
    # sentences the finding was measured with, reproduced here as permanent
    # fixtures against re-introducing that design. ---

    # 15. A PRESENT-TENSE claim about a retired file must be flagged, even
    #     though the number it cites (073) was genuinely real once, for a
    #     DIFFERENT file (073_a_schedule_idempotency_key... or similar) --
    #     proves the check is keyed on the exact (file, fact) pair, not on
    #     "was this number ever real anywhere".
    unallowed, _stale = run([("some/file.go", 1, "postgres", "073", "073_plugin_sweep.sql")])
    if unallowed != [("some/file.go", "073_plugin_sweep.sql")]:
        print("self-test FAIL: a present-tense citation of a fabricated "
              "filename, sharing a once-real NUMBER with a different real "
              "file, was not flagged -- the historical-number regression",
              file=sys.stderr)
        ok = False

    # 16. The same regression in the bare dialect-less word form: "the
    #     plugin sweep, migration 073, grants it" -- must be flagged even
    #     though 073 was, again, once a real number (for postgres AND
    #     mssql, in different files).
    unallowed, _stale = run([("some/file.go", 1, None, "073", None)])
    if unallowed != [("some/file.go", "migration 073")]:
        print("self-test FAIL: a bare 'migration NNN' regression case was "
              "not flagged", file=sys.stderr)
        ok = False

    # 17. And in the dialect-scoped bare-number form: "migrations/mssql/074
    #     defines admin.drop_tenant there" -- 074 was a real mssql number
    #     once (it is cleat#2817's own instance 1; plugin/migration.go's own
    #     citation of it IS in the real baseline file -- see the note above
    #     real_files_by_dialect()). A SEPARATE file citing the same number
    #     must ALSO be flagged, proving the exemption is per-citation, not
    #     per-number: baselining plugin/migration.go's specific pair must
    #     never excuse a different file's citation of the same number. This
    #     is the actual mechanism of the regression this revision fixes --
    #     not "withholding one entry", but "the baseline discriminates by
    #     citation, not by number" -- and it is asserted directly here.
    unallowed, _stale = run(
        [("another/file.go", 1, "mssql", "074", None)],
        baseline={("plugin/migration.go", "migrations/mssql/074"): True},
    )
    if unallowed != [("another/file.go", "migrations/mssql/074")]:
        print("self-test FAIL: baselining ONE file's citation of a number "
              "excused a DIFFERENT file's citation of the same number -- "
              "the baseline is not per-citation", file=sys.stderr)
        ok = False

    # --- extract_citations itself: the text-level parser, not the checker ---

    # 18. A full path+filename citation extracts the dialect, number and
    #     filename together, and does not ALSO produce a spurious bare-word
    #     match for the same span (the alternation-ordering property this
    #     file's docstring names as load-bearing).
    got = extract_citations("see migrations/mssql/074_something.sql for detail")
    if got != [("mssql", "074", "074_something.sql")]:
        print(f"self-test FAIL: path+filename extraction gave {got}, "
              "want exactly one dialect-scoped match", file=sys.stderr)
        ok = False

    # 19. A bare dialect+number citation (no filename, no trailing .sql)
    #     extracts with filename=None -- cleat#2817's own instance 1,
    #     verbatim ("migrations/mssql/074's admin.drop_tenant").
    got = extract_citations("migrations/mssql/074's admin.drop_tenant sets the key")
    if got != [("mssql", "074", None)]:
        print(f"self-test FAIL: bare path+number extraction gave {got}", file=sys.stderr)
        ok = False

    # 20. A bare "migration NNN" (no backticks, no dialect, no filename)
    #     extracts with dialect=None and filename=None -- cleat#2817's own
    #     instance 2, verbatim ("the plugin sweep, migration 073").
    got = extract_citations("the plugin sweep, migration 073) still uses")
    if got != [(None, "073", None)]:
        print(f"self-test FAIL: bare word+number extraction gave {got}", file=sys.stderr)
        ok = False

    # 21. A backtick-quoted word+filename citation still extracts correctly
    #     (the guard's pre-#2817 behavior, unchanged).
    got = extract_citations("used to execute migration `020_event_intent.sql`")
    if got != [(None, "020", "020_event_intent.sql")]:
        print(f"self-test FAIL: backtick word+filename extraction gave {got}", file=sys.stderr)
        ok = False

    # 22. Two independent citations on one line both extract -- the scan
    #     must not stop after the first match.
    got = extract_citations("originally migrations/postgres/038_defer_phase_marker.sql, mysql/037, mssql/041")
    if ("postgres", "038", "038_defer_phase_marker.sql") not in got:
        print(f"self-test FAIL: multi-citation line missed the path form: {got}", file=sys.stderr)
        ok = False

    # 23. The real scan, against the real tree: this script's own file must
    #     not appear in the matches at all (EXEMPT_FILE_PREFIXES), which is
    #     what stops this guard flagging itself over its own docstring --
    #     measured broken in review before this exemption existed.
    real_matches = git_tracked_matches()
    if any(path == SELF_PATH for path, _lineno, _d, _n, _f in real_matches):
        print("self-test FAIL: git_tracked_matches() did not exclude "
              "this script's own path", file=sys.stderr)
        ok = False

    # 23b. The baseline file must ALSO be excluded from the scan. Its own
    #      content is 289 lines each containing a dangling citation's cited
    #      fact -- git grep only sees TRACKED files, so this was invisible
    #      until the file was first `git add`ed, at which point it produced
    #      108 second-order "citations" whose path was the baseline file
    #      itself (measured directly while writing this PR, by staging the
    #      file and re-running --self-test before this exemption existed).
    if any(path == BASELINE_PATH for path, _lineno, _d, _n, _f in real_matches):
        print("self-test FAIL: git_tracked_matches() did not exclude "
              "the baseline file's own path -- it will self-referentially "
              "grow on every --update", file=sys.stderr)
        ok = False

    # 23c. Discovery itself, on the real tree -- coordinator, 2026-09-30.
    #      Assertions 1-17 inject synthetic `matches` tuples directly, so
    #      they exercise is_real()/check()'s PREDICATE and baseline logic
    #      but never git_tracked_matches()'s actual walk. A regression there
    #      (a wrong pathspec, a dialect silently dropped from GREP_PATTERN)
    #      would pass every assertion above and still under-scan the tree.
    #
    #      The control is picked from the BASELINE at runtime, not pinned to
    #      one hardcoded citation (cleat-review N2, 2026-09-30): a fixed pin
    #      -- the first version named engine/rls_check.go's 005_app_role.sql
    #      -- fails the moment someone correctly FIXES that citation, with a
    #      message that reads "discovery is broken" about a change that was
    #      actually progress. Any filename-bearing baseline entry works as
    #      the control (the bug this assertion exists to catch -- git grep
    #      -o truncating before the filename -- only manifests on entries
    #      that HAVE one), so picking the sorted-first one is deterministic
    #      and self-updating as the baseline changes.
    discovered = {(p, d, n, f) for p, _l, d, n, f in real_matches}
    filename_entries = sorted((p, f) for (p, f) in load_baseline() if f.endswith(".sql"))
    if not filename_entries:
        print("self-test FAIL: no filename-bearing baseline entry exists to use "
              "as assertion 23c's discovery control -- this is a precondition "
              "failure of the self-test, not evidence discovery works",
              file=sys.stderr)
        ok = False
    else:
        control_path, control_fact = filename_entries[0]
        if not any(p == control_path and cited_fact(d, n, f) == control_fact
                   for p, d, n, f in discovered):
            print(f"self-test FAIL: git_tracked_matches() did not find the "
                  f"baseline's own {control_path!r} citation of {control_fact!r} "
                  "-- discovery itself may be under-scanning the tree",
                  file=sys.stderr)
            ok = False

    # 24. The real scan, against the real tree and the real baseline file,
    #     must be clean: no unallowed dangling citations, no stale baseline
    #     entries. This is the guard's own main-mode check, run here too so
    #     --self-test catches a tree that would fail the second invocation
    #     the CI step also runs.
    real_files_now = real_files_by_dialect()
    real_numbers_now = real_numbers_by_dialect(real_files_now)
    real_baseline = load_baseline()
    unallowed, stale = check(real_matches, real_files_now, real_numbers_now, real_baseline)
    if unallowed:
        print(f"self-test FAIL: {len(unallowed)} unallowed dangling citation(s) "
              "in the real tree", file=sys.stderr)
        for path, fact in unallowed[:20]:
            print(f"    {path}: cites {fact}", file=sys.stderr)
        ok = False
    if stale:
        print(f"self-test FAIL: {len(stale)} stale baseline entries against "
              "the real tree", file=sys.stderr)
        for path, fact in stale[:20]:
            print(f"    {path}: {fact}", file=sys.stderr)
        ok = False

    # 25. cleat#3666, the dialect-SCOPED form. The 004 collisions (cleat#1986,
    #     cleat#3360, f6310d9f) flipped `migrations/postgres/004` itself, not a
    #     bare `migration 004`: the dialect named in the citation reused the
    #     number. Classification must not depend on the form, and the target
    #     is named per dialect.
    unit_matches = [("a.go", 1, "postgres", "004", None)]
    unit_files = dict(real_files)
    unit_files["postgres"] = real_files["postgres"] | {"004_operator_key_scopes.sql"}
    fixed, resolved = classify_stale(
        [("a.go", "migrations/postgres/004"), ("c.go", "migration 099")],
        unit_matches, unit_files)
    want = ["postgres/004_operator_key_scopes.sql",
            "postgres/004_scope_plugin_grants_to_the_install.sql"]
    if resolved != {("a.go", "migrations/postgres/004"): want}:
        print(f"self-test FAIL: a dialect-scoped citation was not classified as "
              f"resolved-under-baseline: {resolved}", file=sys.stderr)
        ok = False
    #    ...and a citation that is gone from the tree is "fixed".
    if fixed != [("c.go", "migration 099")]:
        print(f"self-test FAIL: a citation no longer in the tree was not left in "
              f"the fixed class: {fixed}", file=sys.stderr)
        ok = False
    #    An entry that is present but resolves to nothing is not a state a
    #    stale entry can be in; it must be UNMEASURED (2), not a verdict.
    try:
        with contextlib.redirect_stderr(io.StringIO()):
            classify_stale([("b.go", "004_fix_x.sql")],
                           [("b.go", 1, None, "004", "004_fix_x.sql")], real_files)
        print("self-test FAIL: an unclassifiable entry was classified", file=sys.stderr)
        ok = False
    except SystemExit as e:
        if e.code != 2:
            print(f"self-test FAIL: an unclassifiable entry exited {e.code}, want 2",
                  file=sys.stderr)
            ok = False

    # 26-30. cleat#3666: a baseline entry that goes stale while its citation
    #        is still in the tree (see self_test_resolved_under_baseline).
    for failure in self_test_resolved_under_baseline():
        print(f"self-test FAIL: {failure}", file=sys.stderr)
        ok = False

    if ok:
        print("self-test: OK")
        return 0
    return 1


def do_update(accept_resolved=False):
    """Regenerate the baseline from the real tree's CURRENT dangling set.

    Mechanical: every dangling (file, fact) pair the scan finds today,
    sorted, written verbatim. It does NOT special-case anything -- the
    plugin/migration.go mssql/074 pair is written here too if it is still
    dangling when you run this, because --update's job is to record what
    IS true of the tree, not what the guard would prefer were true. If you
    are regenerating specifically to grandfather a citation you just wrote,
    read the module docstring's warning about what that pair means first.

    One thing it does refuse (cleat#3666): dropping an entry whose citation is
    still in the tree. That is not a fixed citation, it is a migration added
    under an unchanged one, and dropping it silently is how six SQL Server
    comments came to cite an unrelated MySQL file. Pass --accept-resolved to
    mean it; the block it prints goes in the PR body. Returns the exit status.
    """
    real_files = real_files_by_dialect()
    real_numbers = real_numbers_by_dialect(real_files)
    matches = git_tracked_matches()
    dangling = compute_dangling(matches, real_files, real_numbers)
    dropped = sorted(set(load_baseline()) - dangling)
    _fixed, resolved = classify_stale(dropped, matches, real_files)
    if resolved and not accept_resolved:
        print(f"REFUSED: --update would drop {len(resolved)} baseline "
              f"entr{'y' if len(resolved) == 1 else 'ies'} whose citation is still "
              "in the tree and now resolves to a migration. Nothing was written.")
        print_resolved(resolved, matches)
        print()
        print(RESOLVED_ADVICE.format(cmd=sys.argv[0]))
        return 1
    with open(BASELINE_PATH, "w") as f:
        for path, fact in sorted(dangling):
            f.write(f"{path}\t{fact}\n")
    print(f"Wrote {len(dangling)} entries to {BASELINE_PATH}")
    if accept_resolved and dropped:
        print()
        print(format_acceptance_block(dropped, resolved, matches))
    return 0


def main():
    if "--self-test" in sys.argv:
        sys.exit(self_test())

    if "--update" in sys.argv:
        sys.exit(do_update(accept_resolved="--accept-resolved" in sys.argv))

    real_files = real_files_by_dialect()
    if not any(real_files.values()):
        print("UNMEASURED: git ls-files migrations/ returned nothing -- "
              "either the directory is gone or this is not a git checkout.",
              file=sys.stderr)
        sys.exit(2)
    real_numbers = real_numbers_by_dialect(real_files)

    matches = git_tracked_matches()
    baseline = load_baseline()
    unallowed, stale = check(matches, real_files, real_numbers, baseline)

    if not unallowed and not stale:
        summary = ", ".join(f"{d}={sorted(real_files[d])}" for d in DIALECTS)
        print(f"OK: every migrations/<dialect>/NNN citation names a migration that "
              f"currently exists ({summary}) or is in the baseline "
              f"({len(baseline)} entries, {BASELINE_PATH}).")
        sys.exit(0)

    if unallowed:
        print("ERROR: dangling migration citation(s), not in the baseline:")
        for path, fact in unallowed:
            print(f"  {path}: cites {fact}")
        print()
        print("Either the citation is wrong (fix it to name a migration that currently")
        print("exists), or it is a deliberate historical reference -- regenerate the")
        print(f"baseline with `{sys.argv[0]} --update` and review the diff before")
        print("committing it.")

    fixed, resolved = classify_stale(stale, matches, real_files)
    if fixed:
        print("ERROR: baseline entries whose (file, cited fact) no longer appears as "
              "a dangling citation (the citation was fixed, or the line changed -- "
              f"regenerate with `{sys.argv[0]} --update`):")
        for path, fact in fixed:
            print(f"  {path}: {fact}")

    if resolved:
        print("ERROR: baseline entries whose citation is STILL in the tree, "
              "unchanged, but now resolves to a migration:")
        print_resolved(resolved, matches)
        print()
        print(RESOLVED_ADVICE.format(cmd=sys.argv[0]))

    sys.exit(1)


if __name__ == "__main__":
    main()
