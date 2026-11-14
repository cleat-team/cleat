#!/usr/bin/env python3
"""Two structural rules for the TypeScript guest's tier-2 scaffolding (cleat#3825).

  1. DOCS CLAIM NO MORE THAN tiers.yaml GRANTS. LANGUAGE_SUPPORT.md's language table is read
     row by row and each row's Tier and Status cells are compared with what tiers.yaml says
     about that language. tiers.yaml is the source of truth for support; a row that says
     Tier 2 or "Shipped" for a language tiers.yaml marks IN DEVELOPMENT or NOT SUPPORTED is
     the defect this repo's CLAUDE.md opens with (prose that claims what the manifest does
     not grant). Nothing checked this before.

  2. TypeScript tests live in ONE tier-2 package. Every function named TestTypeScript* must be
     under tests/typescript/, and tests/typescript/ must be in tiers.yaml's tier2.packages and
     in no tier-1 pattern. A tier-2 test in a tier-1 package has leaked three times in this
     repo; the exclusion is structural, so there is no pattern list to rot. The rule also
     refuses to pass over zero tests: a package that holds none satisfies "must run" with
     nothing.

Exit statuses (CLAUDE.md, "give 'I could not look' its own exit status"):
  0  passed
  1  a finding: the tree says more than tiers.yaml grants, or a test is misplaced
  2  UNMEASURED: the check could not establish what it measures (missing file, no table, no
     tests, PyYAML absent). This is a failure of the check, not a finding about the tree.

  usage: scripts/check-typescript-tier.py            check the repository
         scripts/check-typescript-tier.py --self-test  known-positive / negative-control pairs
"""
import os
import re
import subprocess
import sys

try:
    import yaml
except ImportError:
    print("UNMEASURED: PyYAML is not installed (pip install pyyaml); the check cannot read tiers.yaml.\n"
          "(this is a failure of the check, not a finding about the tree)", file=sys.stderr)
    sys.exit(2)

TS_PKG = "tests/typescript"
TS_PATTERN = "./tests/typescript/..."

# The doc's language names, mapped to tiers.yaml's keys. A row the map does not know is a
# finding, not a skip: a language the guard cannot classify is exactly its blind spot, so the
# next one is added to this map on purpose. None means "not in tiers.yaml at all".
LANG_KEY = {
    "Go": "go",
    "Python": "python",
    "Rust": "rust",
    "Java/Kotlin": "java",
    "AssemblyScript": "assemblyscript",
    "TypeScript (real)": "typescript",
    "C": None,
    "Zig": None,
    "C#/.NET": None,
}

NOT_SUPPORTED = re.compile(r"^\s*(IN DEVELOPMENT|NOT SUPPORTED)", re.I)
# A status cell that claims support. "not supported" and "unsupported" do not.
CLAIMS_SUPPORT = re.compile(r"\bshipped\b|(?<!not )(?<!un)\bsupported\b|\bproduction\b", re.I)


class Unmeasured(Exception):
    pass


def levels(tiers):
    """{language key: 1 | 2 | 0}. 0 means tiers.yaml grants no support (absent, or marked
    IN DEVELOPMENT / NOT SUPPORTED)."""
    out = {}
    for lang in (tiers.get("tier1") or {}).get("languages") or []:
        out[str(lang)] = 1
    for comp in (tiers.get("tier2") or {}).get("components") or []:
        cid = str(comp.get("id", ""))
        if not cid.startswith("sdk-"):
            continue
        lang = cid[len("sdk-"):]
        if lang in out:
            continue
        out[lang] = 0 if NOT_SUPPORTED.match(str(comp.get("status", ""))) else 2
    return out


def doc_rows(text):
    """Rows of LANGUAGE_SUPPORT.md's language table: [(name, status, tier)]."""
    lines = text.splitlines()
    start = None
    for i, l in enumerate(lines):
        if re.match(r"^\|\s*Language\s*\|\s*WASM compiler\s*\|\s*Status\s*\|", l):
            start = i
            break
    if start is None:
        raise Unmeasured("LANGUAGE_SUPPORT.md has no `| Language | WASM compiler | Status |` table")
    header = [c.strip() for c in lines[start].strip().strip("|").split("|")]
    try:
        si, ti = header.index("Status"), header.index("Tier")
    except ValueError:
        raise Unmeasured("the language table has no Status and Tier columns")
    rows = []
    for l in lines[start + 2:]:
        if not l.startswith("|"):
            break
        cells = [c.strip() for c in l.strip().strip("|").split("|")]
        if len(cells) <= max(si, ti):
            continue
        name = re.sub(r"\*+", "", cells[0]).strip()
        rows.append((name, cells[si], cells[ti]))
    if not rows:
        raise Unmeasured("the language table has no rows")
    return rows


def check_docs(tiers, doc_text):
    lv = levels(tiers)
    findings = []
    for name, status, tier in doc_rows(doc_text):
        if name not in LANG_KEY:
            findings.append(f"LANGUAGE_SUPPORT.md row '{name}' is not in this guard's language map; "
                            f"add it deliberately so its claim can be checked")
            continue
        key = LANG_KEY[name]
        granted = lv.get(key, 0) if key else 0
        claimed = {"1": 1, "2": 2}.get(tier.strip(), 0)
        if claimed != granted:
            findings.append(f"LANGUAGE_SUPPORT.md says tier '{tier.strip()}' for {name}, but tiers.yaml grants "
                            f"{'tier ' + str(granted) if granted else 'no support'}")
        if granted == 0 and CLAIMS_SUPPORT.search(status):
            findings.append(f"LANGUAGE_SUPPORT.md status for {name} reads '{status}', which claims support "
                            f"tiers.yaml does not grant")
    return findings


def check_placement(tiers, test_files):
    """test_files: {path: source text} for every tracked *_test.go."""
    findings = []
    t2 = (tiers.get("tier2") or {}).get("packages") or []
    t1 = (tiers.get("tier1") or {}).get("packages") or []
    if TS_PATTERN not in t2:
        findings.append(f"tiers.yaml tier2.packages does not list {TS_PATTERN}")
    covering = [p for p in t1 if p == TS_PATTERN or p == "./..." or
                (p.endswith("/...") and ("./" + TS_PKG + "/").startswith(p[:-3]))]
    if covering:
        findings.append(f"tiers.yaml tier1.packages covers the TypeScript package via {covering}; "
                        f"its tests would run in the Tier 1 gate")
    inside = 0
    for path, src in sorted(test_files.items()):
        for m in re.finditer(r"^func (TestTypeScript\w*)\(", src, re.M):
            if path.startswith(TS_PKG + "/"):
                inside += 1
            else:
                findings.append(f"{path}: {m.group(1)} is outside {TS_PKG}/ (a TypeScript test in another "
                                f"package can run in Tier 1, where a skip is a failure)")
    if inside == 0:
        raise Unmeasured(f"{TS_PKG}/ holds no TestTypeScript* function, so the placement rule examined nothing "
                         f"and 'must run' would be satisfied by an empty package")
    return findings


def repo_inputs(root):
    tiers = yaml.safe_load(open(os.path.join(root, "tiers.yaml")))
    doc = open(os.path.join(root, "LANGUAGE_SUPPORT.md")).read()
    # git ls-files, not a directory walk: a walk descends into .claude/worktrees/ (a second
    # copy of the repo) and attributes a test to a package that exists only in a scratch checkout.
    out = subprocess.run(["git", "ls-files", "*_test.go"], cwd=root, capture_output=True, text=True)
    if out.returncode != 0:
        raise Unmeasured(f"git ls-files failed: {out.stderr.strip()}")
    files = {}
    for p in out.stdout.split("\n"):
        if p:
            try:
                files[p] = open(os.path.join(root, p)).read()
            except OSError:
                pass  # a tracked file deleted in the working tree: nothing to read
    if not files:
        raise Unmeasured("git ls-files found no *_test.go files")
    return tiers, doc, files


def run(tiers, doc, files):
    return check_docs(tiers, doc) + check_placement(tiers, files)


# ---------------------------------------------------------------------------------------------
# --self-test: each rule against a case that must be refused and one that must pass.

_TIERS = {
    "tier1": {"languages": ["go", "python"], "packages": ["./engine/...", "./cmd/..."]},
    "tier2": {"packages": ["./plugins/...", TS_PATTERN],
              "components": [
                  {"id": "sdk-rust"},
                  {"id": "sdk-typescript", "status": "IN DEVELOPMENT -- NOT SUPPORTED"},
              ]},
}
_DOC = """# Language support
| Language | WASM compiler | Status | Binary size | Tier |
|----------|---------------|--------|-------------|------|
| **Go** | go | Shipped | ~4 MB | 1 |
| **Python** | componentize-py | Shipped | 18 MB | 1 |
| **Rust** | cargo | Shipped | ~50 KB | 2 |
| **TypeScript** (real) | Javy/QuickJS | In development, not supported | ~1.1 MB | — |

after the table
"""
_TESTS = {"tests/typescript/a_test.go": "package typescript\nfunc TestTypeScriptLabel(t *testing.T) {}\n",
          "engine/x_test.go": "package engine\nfunc TestSomething(t *testing.T) {}\n"}


def self_test():
    bad = 0

    def expect(name, findings, want_substr):
        nonlocal bad
        ok = (want_substr is None and not findings) or \
             (want_substr is not None and any(want_substr in f for f in findings))
        print(f"  {'ok  ' if ok else 'FAIL'}  {name}" + ("" if ok else f"   got: {findings}"))
        bad += 0 if ok else 1

    def expect_unmeasured(name, fn):
        nonlocal bad
        try:
            fn()
        except Unmeasured as e:
            print(f"  ok    {name}   ({e})")
            return
        print(f"  FAIL  {name}   did not raise Unmeasured")
        bad += 1

    import copy
    expect("negative control: a consistent tree passes", run(_TIERS, _DOC, _TESTS), None)

    # known-positives, one per way the rule can be violated
    d = _DOC.replace("| In development, not supported | ~1.1 MB | — |", "| Shipped | ~1.1 MB | 2 |")
    expect("known-positive: doc says TypeScript is tier 2 and Shipped", check_docs(_TIERS, d), "tier '2' for TypeScript")
    d = _DOC.replace("In development, not supported", "Shipped")
    expect("known-positive: doc says Shipped, tier cell still dash", check_docs(_TIERS, d), "claims support")
    d = _DOC.replace("In development, not supported", "Supported on Linux")
    expect("known-positive: doc says Supported", check_docs(_TIERS, d), "claims support")
    d = _DOC.replace("| Rust | cargo | Shipped | ~50 KB | 2 |", "| Rust | cargo | Shipped | ~50 KB | — |").replace(
        "| **Rust** | cargo | Shipped | ~50 KB | 2 |", "| **Rust** | cargo | Shipped | ~50 KB | — |")
    expect("known-positive: doc UNDER-claims a tier-2 language (disagreement either way is a finding)",
           check_docs(_TIERS, d), "tier '—' for Rust")
    d = _DOC.replace("after the table", "| **Brainfuck** | none | Shipped | 1 B | 1 |\nafter the table")
    d = _DOC.replace("| **TypeScript**", "| **Brainfuck** | x | Shipped | 1 B | 1 |\n| **TypeScript**")
    expect("known-positive: a language the guard does not know", check_docs(_TIERS, d), "not in this guard's language map")

    t = copy.deepcopy(_TIERS); t["tier2"]["components"][1]["status"] = "supported"
    expect("known-positive: tiers.yaml itself grants TypeScript tier 2 but the doc says dash",
           check_docs(t, _DOC), "tier '—' for TypeScript")
    expect("negative control: 'not supported' wording does not trip the support regex",
           [f for f in check_docs(_TIERS, _DOC) if "claims support" in f], None)

    f = dict(_TESTS); f["engine/ts_test.go"] = "package engine\nfunc TestTypeScriptThing(t *testing.T) {}\n"
    expect("known-positive: TestTypeScript* outside tests/typescript/", check_placement(_TIERS, f), "engine/ts_test.go")
    t = copy.deepcopy(_TIERS); t["tier2"]["packages"].remove(TS_PATTERN)
    expect("known-positive: package missing from tier2.packages", check_placement(t, _TESTS), "does not list")
    t = copy.deepcopy(_TIERS); t["tier1"]["packages"].append("./tests/...")
    expect("known-positive: tier 1 covers the package by a parent pattern", check_placement(t, _TESTS), "covers")
    t = copy.deepcopy(_TIERS); t["tier1"]["packages"].append("./tests/integrity/...")
    expect("negative control: an unrelated tests/ tier-1 pattern does not cover it", check_placement(t, _TESTS), None)

    expect_unmeasured("UNMEASURED: tests/typescript/ holds no TestTypeScript* test",
                      lambda: check_placement(_TIERS, {"engine/x_test.go": _TESTS["engine/x_test.go"]}))
    expect_unmeasured("UNMEASURED: the doc has no language table", lambda: doc_rows("# nothing here\n"))
    expect_unmeasured("UNMEASURED: the table has no rows",
                      lambda: doc_rows("| Language | WASM compiler | Status | Binary | Tier |\n|---|---|---|---|---|\n"))

    print(f"self-test: {'FAILED' if bad else 'every known-positive refused, every negative control passed'}")
    return 1 if bad else 0


def main():
    if len(sys.argv) > 2 or (len(sys.argv) == 2 and sys.argv[1] != "--self-test"):
        print(__doc__, file=sys.stderr)
        return 2  # a usage error is not a pass and not a finding
    if len(sys.argv) == 2:
        return self_test()
    root = subprocess.run(["git", "rev-parse", "--show-toplevel"], capture_output=True, text=True).stdout.strip() \
        or os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    try:
        tiers, doc, files = repo_inputs(root)
        findings = run(tiers, doc, files)
    except Unmeasured as e:
        print(f"UNMEASURED: {e}\n(this is a failure of the check, not a finding about the tree)", file=sys.stderr)
        return 2
    except FileNotFoundError as e:
        print(f"UNMEASURED: {e}\n(this is a failure of the check, not a finding about the tree)", file=sys.stderr)
        return 2
    if findings:
        print(f"{len(findings)} finding(s):", file=sys.stderr)
        for f in findings:
            print(f"  - {f}", file=sys.stderr)
        return 1
    rows = doc_rows(doc)
    n = sum(1 for p in files if p.startswith(TS_PKG + "/"))
    print(f"OK: {len(rows)} language row(s) in LANGUAGE_SUPPORT.md agree with tiers.yaml; "
          f"TestTypeScript* tests are confined to {TS_PKG}/ ({n} file(s) there).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
