#!/usr/bin/env python3
"""Fold changelog.d/ fragments into CHANGELOG.md at release prep (cleat's fragment scheme; see changelog.d/README.md).

    scripts/fold-changelog.py --dry-run                  print what would be folded; change nothing
    scripts/fold-changelog.py                            fold into `## [Unreleased]` and delete the fragments
    scripts/fold-changelog.py --release 0.6.0 --date 2026-12-01
                                                         ... and cut the release: `## [Unreleased]` stays, empty, above `## [0.6.0] - <date>`
    scripts/fold-changelog.py --self-test

Entries go at the TOP of their section, newest issue number first (then slug), so the result is the same on every machine. A section the release does
not have yet is created in the canonical order (UPGRADE NOTES, Added, Changed, Fixed, Removed). A malformed fragment stops the fold: nothing is
written and nothing deleted. Exit 0 folded (or dry run), 1 a refusal (malformed fragment, no `## [Unreleased]`), 2 unmeasured, 3 failed self-test.
"""
import argparse
import os
import pathlib
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import changelog_fragments as cf  # noqa: E402

CHANGELOG = pathlib.Path("CHANGELOG.md")


def fold_text(text, frags):
    """Return the new CHANGELOG text, or raise ValueError."""
    lines = text.split("\n")
    try:
        start = next(i for i, l in enumerate(lines) if l.startswith("## [Unreleased]"))
    except StopIteration:
        raise ValueError("CHANGELOG.md has no `## [Unreleased]` heading to fold into")
    end = next((i for i in range(start + 1, len(lines)) if lines[i].startswith("## [")), len(lines))
    by = {k: cf.order([f for f in frags if f.section == k]) for k in cf.KEYS}
    heading_of = dict(cf.SECTIONS)

    def find(key):
        for i in range(start + 1, end):
            if lines[i].startswith(heading_of[key]):
                return i
        return None

    for key in reversed(cf.KEYS):  # bottom-up so earlier indexes stay valid
        entries = by[key]
        if not entries:
            continue
        block = []
        for f in entries:
            block += f.body.split("\n")
        i = find(key)
        if i is not None:
            lines[i + 1:i + 1] = block
        else:
            # create the section before the first existing section that reads AFTER it, else at the end of Unreleased
            later = [find(k) for k in cf.KEYS[cf.KEYS.index(key) + 1:]]
            later = [x for x in later if x is not None]
            at = min(later) if later else end
            new = [heading_of[key]] + block + [""]
            if not later and at > 0 and lines[at - 1].strip():
                new = [""] + new[:-1] + [""] if at == len(lines) else [""] + new
            lines[at:at] = new
        end = next((j for j in range(start + 1, len(lines)) if lines[j].startswith("## [")), len(lines))
    return "\n".join(lines)


def cut_release(text, release, date):
    marker = "## [Unreleased]"
    i = text.index(marker)
    j = i + len(marker)
    return text[:j] + "\n\n## [%s] - %s" % (release, date) + text[j:]


def self_test():
    bad = []
    with tempfile.TemporaryDirectory() as d:
        os.chdir(d)
        os.makedirs("changelog.d")
        pathlib.Path("changelog.d/README.md").write_text("x")
        w = lambda n, s, b: pathlib.Path("changelog.d/" + n).write_text("---\nsection: %s\n---\n%s\n" % (s, b))
        w("10-a.md", "fixed", "- **Ten (cleat#10).** line one\n  continued")
        w("30-c.md", "fixed", "- **Thirty (cleat#30).**")
        w("20-b.md", "added", "- **Twenty (cleat#20).**")
        w("40-d.md", "upgrade", "- **Forty (cleat#40).**")
        pathlib.Path("CHANGELOG.md").write_text("# Changelog\n\n## [Unreleased]\n\n### Added\n- **Old (cleat#1).**\n\n### Fixed\n- **Oldfix (cleat#2).**\n\n## [0.5.0] - 2026-10-27\n\n### Fixed\n- **Release (cleat#3).**\n")
        frags, problems = cf.read_all()
        if problems or len(frags) != 4:
            bad.append("reading: %d fragments, problems %r" % (len(frags), problems))
        out = fold_text(CHANGELOG.read_text(), frags)
        want = "# Changelog\n\n## [Unreleased]\n\n### UPGRADE NOTES\n- **Forty (cleat#40).**\n\n### Added\n- **Twenty (cleat#20).**\n- **Old (cleat#1).**\n\n### Fixed\n- **Thirty (cleat#30).**\n- **Ten (cleat#10).** line one\n  continued\n- **Oldfix (cleat#2).**\n\n## [0.5.0] - 2026-10-27\n\n### Fixed\n- **Release (cleat#3).**\n"
        if out != want:
            bad.append("fold output differs from the expected text:\n---got---\n%s\n---want---\n%s" % (out, want))
        # a stable order: folding the fragments in any input order gives the same text
        if fold_text(CHANGELOG.read_text(), list(reversed(frags))) != out:
            bad.append("the folded order depends on the input order")
        # the released section is untouched
        if out.split("## [0.5.0]")[1] != want.split("## [0.5.0]")[1]:
            bad.append("an entry leaked into an already-released section")
        rel = cut_release(out, "0.6.0", "2026-12-01")
        if "## [Unreleased]\n\n## [0.6.0] - 2026-12-01\n\n### UPGRADE NOTES" not in rel:
            bad.append("the release heading was not cut below an empty Unreleased")
        # a malformed fragment is a problem, never a crash and never silently skipped
        pathlib.Path("changelog.d/50-e.md").write_text("no front matter\n")
        pathlib.Path("changelog.d/bad name.md").write_text("x")
        _, problems = cf.read_all()
        if len(problems) != 2:
            bad.append("two malformed fragments gave %d problems" % len(problems))
        # a section created at the END of Unreleased (nothing reads after it) keeps the blank line before the next release heading
        w("60-f.md", "removed", "- **Gone (cleat#60).**")
        f2, _ = cf.read(pathlib.Path("changelog.d/60-f.md"))
        out2 = fold_text(CHANGELOG.read_text(), [f2])
        if "- **Oldfix (cleat#2).**\n\n### Removed\n- **Gone (cleat#60).**\n\n## [0.5.0]" not in out2:
            bad.append("a section created at the end of Unreleased came out as:\n" + out2)
        # no Unreleased heading is a refusal
        try:
            fold_text("# Changelog\n## [0.5.0]\n", frags)
            bad.append("a CHANGELOG with no Unreleased heading did not refuse")
        except ValueError:
            pass
    if bad:
        print("SELF-TEST FAILED:\n  " + "\n  ".join(bad))
        return 3
    print("OK: fold-changelog self-test (stable newest-first order, canonical sections created, released sections untouched, malformed fragments refused)")
    return 0


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--release")
    ap.add_argument("--date")
    ap.add_argument("--self-test", action="store_true")
    a = ap.parse_args(argv)
    if a.self_test:
        return self_test()
    if bool(a.release) != bool(a.date):
        print("--release and --date go together", file=sys.stderr)
        return 2
    frags, problems = cf.read_all()
    if problems:
        print("REFUSED: nothing folded and nothing deleted, because of:\n  " + "\n  ".join(problems))
        return 1
    if not frags:
        print("no fragments in changelog.d: nothing to fold")
        return 0
    try:
        new = fold_text(CHANGELOG.read_text(encoding="utf-8"), frags)
    except (OSError, ValueError) as e:
        print("REFUSED: %s" % e)
        return 1
    if a.release:
        new = cut_release(new, a.release, a.date)
    for k, h in cf.SECTIONS:
        es = cf.order([f for f in frags if f.section == k])
        if es:
            print("%s (%d)" % (h, len(es)))
            for f in es:
                print("  cleat#%-5d %s  %s" % (f.issue, f.slug, f.body.split("\n")[0][:90]))
    if a.dry_run:
        print("dry run: CHANGELOG.md and changelog.d/ unchanged")
        return 0
    CHANGELOG.write_text(new, encoding="utf-8")
    for f in frags:
        os.remove(f.path)
    print("folded %d fragment(s) into CHANGELOG.md and deleted them" % len(frags))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
