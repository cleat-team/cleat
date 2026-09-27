#!/usr/bin/env python3
"""The published Python wheel ships `cleat_sdk` and nothing else at top level.

WHY THIS EXISTS. Until 2026-09-27 the built wheel carried SIX top-level names:

    cleat_sdk/                    correct
    cleat_sdk-0.3.0.dist-info/    correct
    tests/                        a top-level `tests` package, in every
    examples/                     consumer's site-packages
    scripts/
    wit/

Two different routes put them there, which is why this asserts the wheel rather
than reading the config. `tests/` and `scripts/` carry an `__init__.py` and are
found by ordinary package discovery; `examples/` and `wit/` have no
`__init__.py` and are found by PEP 420 NAMESPACE discovery, because
`namespaces` defaults to true for `[tool.setuptools.packages.find]` in
pyproject. The fix is an `include` allowlist there. MANIFEST.in carries
`prune tests` and `prune examples` and did NOT prevent any of it: MANIFEST.in
shapes the SDIST; a wheel's contents come from discovery plus package data.

WHY THE WHEEL AND NOT THE CONFIG. A config assertion checks that a line is
present; this checks what a consumer actually receives. ci.yml installs the
SOURCE tree (`pip install .[dev]`), so nothing on a normal PR ever reads the
artefact that ships -- which is the whole reason release-smoke.yml exists.

EXIT STATUS, and the third one matters:
    0   the wheel carries only the allowed top-level entries
    1   a finding -- it carries something else
    2   could not measure -- no wheel, or not a readable zip
0 and 2 must differ, because a check that measured nothing agrees with every
wheel, correct or not.

Usage:
    scripts/check-python-wheel-contents.py path/to/cleat_sdk-*.whl
    scripts/check-python-wheel-contents.py --self-test
"""

import argparse
import sys
import zipfile
from pathlib import Path

# The package, plus the wheel's own `.dist-info`, which is checked separately
# because its name carries the version.
PACKAGE = "cleat_sdk"

EXIT_OK = 0
EXIT_FINDING = 1
EXIT_UNMEASURED = 2


def top_level(names):
    """The set of first path segments in a wheel's namelist."""
    return sorted({n.split("/")[0] for n in names})


def classify(tops):
    """Return the entries that are neither the package nor a .dist-info dir."""
    return [t for t in tops if t != PACKAGE and not t.endswith(".dist-info")]


def check(wheel):
    """-> (exit status, message). Never raises."""
    path = Path(wheel)
    if not path.exists():
        return EXIT_UNMEASURED, f"UNMEASURED: no wheel at {path}"
    try:
        names = zipfile.ZipFile(path).namelist()
    except zipfile.BadZipFile as exc:
        return EXIT_UNMEASURED, f"UNMEASURED: {path} is not a readable zip: {exc}"

    tops = top_level(names)
    unexpected = classify(tops)
    detail = f"{path.name}: {len(names)} entries, top-level {tops}"
    if unexpected:
        return EXIT_FINDING, (
            f"{detail}\n\nUNEXPECTED top-level entries: {unexpected}\n\n"
            f"A wheel may carry only `{PACKAGE}` and its own .dist-info. Anything "
            f"else is installed into the consumer's site-packages beside the SDK. "
            f"See the `include` in pyproject.toml's [tool.setuptools.packages.find] "
            f"-- it is an allowlist deliberately, and removing it readmits "
            f"`tests`, `examples`, `scripts` and `wit` by two separate routes."
        )
    return EXIT_OK, detail


def _fixture(entries):
    """An in-memory wheel-shaped zip with the given namelist."""
    import io

    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        for e in entries:
            z.writestr(e, "")
    buf.seek(0)
    return buf


def self_test():
    """The known-positive is the PRE-FIX wheel, spelled out.

    A guard written for a defect must be shown reporting THAT defect, not
    merely passing on a tree that happens to be fine.
    """
    import tempfile

    clean = ["cleat_sdk/__init__.py", "cleat_sdk/version.py", "cleat_sdk-0.3.0.dist-info/METADATA"]
    # The six top-level names measured on 2026-09-27, before the fix.
    pre_fix = clean + [
        "tests/__init__.py",
        "tests/test_client.py",
        "examples/hello_workflow.py",
        "scripts/__init__.py",
        "wit/cleat.wit",
    ]

    cases = [
        ("clean wheel", clean, EXIT_OK),
        ("KNOWN-POSITIVE: the pre-fix wheel", pre_fix, EXIT_FINDING),
        ("a single stray, the general case", clean + ["straysentinel/__init__.py"], EXIT_FINDING),
        ("dist-info alone is not a stray", clean, EXIT_OK),
    ]

    failures = []
    with tempfile.TemporaryDirectory() as td:
        for label, entries, want in cases:
            p = Path(td) / "cleat_sdk-0.3.0-py3-none-any.whl"
            p.write_bytes(_fixture(entries).getvalue())
            got, msg = check(p)
            ok = got == want
            print(f"  {'ok  ' if ok else 'FAIL'} {label}: exit={got} want={want}")
            if not ok:
                failures.append(f"{label}: exit={got} want={want}\n{msg}")

        # The third status is the one a two-valued guard cannot express.
        got, msg = check(Path(td) / "does-not-exist.whl")
        ok = got == EXIT_UNMEASURED
        print(f"  {'ok  ' if ok else 'FAIL'} missing wheel reports UNMEASURED: exit={got} want={EXIT_UNMEASURED}")
        if not ok:
            failures.append(f"missing wheel: exit={got} want={EXIT_UNMEASURED}")

        (Path(td) / "garbage.whl").write_bytes(b"not a zip")
        got, _ = check(Path(td) / "garbage.whl")
        ok = got == EXIT_UNMEASURED
        print(f"  {'ok  ' if ok else 'FAIL'} unreadable wheel reports UNMEASURED: exit={got} want={EXIT_UNMEASURED}")
        if not ok:
            failures.append(f"unreadable wheel: exit={got} want={EXIT_UNMEASURED}")

    if failures:
        print("\nSELF-TEST FAILED:\n" + "\n".join(failures))
        return EXIT_FINDING
    print("\nself-test passed")
    return EXIT_OK


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("wheels", nargs="*", help="path(s) to a built .whl")
    ap.add_argument("--self-test", action="store_true")
    args = ap.parse_args(argv)

    if args.self_test:
        return self_test()
    if not args.wheels:
        print("usage: check-python-wheel-contents.py <wheel>... | --self-test")
        return EXIT_UNMEASURED

    # The first non-zero wins. A finding is reported even if a later path is
    # unmeasurable, so a genuine finding is never masked by a missing file.
    worst = EXIT_OK
    for wheel in args.wheels:
        status, message = check(wheel)
        print(("  " if status == EXIT_OK else "") + message,
              file=None if status == EXIT_OK else sys.stderr)
        if status != EXIT_OK and worst == EXIT_OK:
            worst = status
    return worst


if __name__ == "__main__":
    raise SystemExit(main())
