#!/usr/bin/env python3
"""Two structural rules for the TypeScript guest's tier-2 scaffolding (cleat#3825).

  1. DOCS CLAIM NO MORE THAN tiers.yaml GRANTS. LANGUAGE_SUPPORT.md's language table is read
     row by row and each row's Tier and Status cells are compared with what tiers.yaml says
     about that language. tiers.yaml is the source of truth for support; a row that says
     Tier 2 or "Shipped" for a language tiers.yaml marks IN DEVELOPMENT or NOT SUPPORTED is
     the defect this repo's CLAUDE.md opens with (prose that claims what the manifest does
     not grant). Nothing checked this before. It fails CLOSED on wording it does not
     recognise: a Tier cell must be a bare 1, 2 or a dash, and a language tiers.yaml does not
     grant must carry a status that says it is not built, in development or not supported.
     README.md is also scanned, line by line, for TypeScript or JavaScript paired with a support
     word or a check mark. That is a LINE HEURISTIC, not a parser: it misses a claim split over two
     lines and one hidden by a negation of another language on the same line. NOT
     covered: the rest of docs/ -- a scan there measured two hits, both comparisons with
     other products, so it is left out rather than made noisy.

  2. TypeScript tests live in ONE tier-2 package. A test function whose name starts with
     TestTypeScript, TestJavaScript, TestJavy or TestQuickJS, and any test file that names the
     toolchain (javy, esbuild, quickjs), must be under tests/typescript/. That package must be
     in tiers.yaml's tier2.packages and must NOT be selected by any tier-1 pattern -- decided by
     asking `go list`, not by comparing strings, because a pattern can reach the package
     without containing its name (`./tests/typescript`, `./tests/typescript/`,
     `./tests/type...`). A tier-2 test in a tier-1 package has leaked three times in this
     repo; the exclusion is structural, so there is no pattern list to rot. The rule also
     refuses to pass over zero tests, and over a `go list` that cannot show engine in tier 1.

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
# A language tiers.yaml does not grant must say so. This is an ALLOWLIST of wording that does
# not claim support, not a denylist of wording that does: a denylist passes "Stable, released"
# and "Beta" and "Available", and every new synonym. An unrecognised status is a finding.
NON_CLAIMING = re.compile(r"^\s*(not built|in development|not supported|planned|not started)\b", re.I)
# ...and the wording must not claim support ANYWHERE in the cell either: a prefix match alone
# passes "In development; supported on Linux". The negated forms are removed first, so "In
# development, not supported" is not a claim.
_NEGATED = re.compile(r"\b(?:not|un)\s*(?:yet\s+)?supported\b", re.I)
_SUPPORT_WORD = re.compile(r"\bsupport(?:s|ed)?\b|\bships?\b|\bshipped\b|\bproduction\b|\bstable\b|"
                           r"\bavailable\b|\breleased\b|\bGA\b|\btier\s*[12]\b|\u2705", re.I)


def status_is_non_claiming(status):
    return bool(NON_CLAIMING.match(status)) and not _SUPPORT_WORD.search(_NEGATED.sub("", status))
# Tier cell: a bare digit, or a dash for none. Anything else ("Tier 2", "Beta", "**2**") is
# refused rather than mapped to "none", which would equal "not granted" and pass.
TIER_CELL = re.compile(r"^(?:\*{0,2})([12]|[\u2014\u2013-])(?:\*{0,2})$")
# README: a line that pairs TypeScript with a support claim, minus the negations.
# A LINE HEURISTIC, not a parser. It reads each README line alone, so it misses a claim split
# over two lines, and a negation of ANOTHER language on the same line ("Rust is not supported;
# TypeScript is") hides a claim. It is a tripwire for the obvious sentence and table row; the
# rest is review's job. The tiers.yaml entry says the same.
README_CLAIM = re.compile(r"typescript|javascript", re.I)
README_SUPPORT = re.compile(r"\bsupport(s|ed)?\b|\bships?\b|\bshipped\b|\bproduction\b|\btier [12]\b|\bstable\b|\u2705", re.I)
README_NEGATION = re.compile(r"not supported|in development|not built|unsupported", re.I)

NAME_PREFIX = re.compile(r"^func (Test(?:TypeScript|JavaScript|Javy|QuickJS)\w*)\(", re.M)
TOOLCHAIN = re.compile(r"\bjavy\b|esbuild|quickjs", re.I)


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


def check_docs(tiers, doc_text, readme_text=""):
    lv = levels(tiers)
    findings = []
    for name, status, tier in doc_rows(doc_text):
        if name not in LANG_KEY:
            findings.append(f"LANGUAGE_SUPPORT.md row '{name}' is not in this guard's language map; "
                            f"add it deliberately so its claim can be checked")
            continue
        key = LANG_KEY[name]
        granted = lv.get(key, 0) if key else 0
        m = TIER_CELL.match(tier.strip())
        if not m:
            findings.append(f"LANGUAGE_SUPPORT.md Tier cell for {name} is '{tier.strip()}', which is not a bare "
                            f"1, 2 or a dash; the guard will not guess what other wording means")
            continue
        claimed = {"1": 1, "2": 2}.get(m.group(1), 0)
        if claimed != granted:
            findings.append(f"LANGUAGE_SUPPORT.md says tier '{tier.strip()}' for {name}, but tiers.yaml grants "
                            f"{'tier ' + str(granted) if granted else 'no support'}")
        if granted == 0 and not status_is_non_claiming(status):
            findings.append(f"LANGUAGE_SUPPORT.md status for {name} reads '{status}', but tiers.yaml grants it no "
                            f"support, so the status must say not built, in development or not supported")
    if lv.get("typescript", 0) == 0:
        for i, line in enumerate(readme_text.splitlines(), 1):
            if README_CLAIM.search(line) and README_SUPPORT.search(line) and not README_NEGATION.search(line):
                findings.append(f"README.md:{i} pairs TypeScript/JavaScript with a support claim, but tiers.yaml grants TypeScript "
                                f"no support: {line.strip()[:100]}")
    return findings


def check_placement(tiers, test_files, resolve, ts_pkg_path):
    """test_files: {path: source text} for every tracked *_test.go.
    resolve(patterns) -> set of package import paths, as `go list` would answer.
    ts_pkg_path: the import path of tests/typescript."""
    findings = []
    t2 = (tiers.get("tier2") or {}).get("packages") or []
    t1 = (tiers.get("tier1") or {}).get("packages") or []
    if TS_PATTERN not in t2:
        findings.append(f"tiers.yaml tier2.packages does not list {TS_PATTERN}")
    else:
        got = resolve([TS_PATTERN])
        if got != {ts_pkg_path}:
            findings.append(f"tier2 pattern {TS_PATTERN} resolves to {sorted(got)}, want exactly {ts_pkg_path}")
    t1_set = resolve(t1)
    # The positive control: a resolver that returns nothing, or cannot see engine, would make
    # "the TypeScript package is not in tier 1" true of everything.
    if not any(p.endswith("/engine") for p in t1_set):
        raise Unmeasured("resolving tier1.packages did not include engine, so the resolver cannot be trusted to "
                         "say the TypeScript package is absent")
    if ts_pkg_path in t1_set:
        via = [p for p in t1 if ts_pkg_path in resolve([p])]
        findings.append(f"tier1.packages selects the TypeScript package (via {via}); its tests would run in the "
                        f"Tier 1 gate, where a skip is a failure")
    inside = 0
    for path, src in sorted(test_files.items()):
        in_pkg = path.startswith(TS_PKG + "/")
        for m in NAME_PREFIX.finditer(src):
            if in_pkg:
                inside += 1
            else:
                findings.append(f"{path}: {m.group(1)} is outside {TS_PKG}/ (a TypeScript test in another "
                                f"package can run in Tier 1, where a skip is a failure)")
        if not in_pkg and TOOLCHAIN.search(src):
            findings.append(f"{path} names the TypeScript toolchain (javy, esbuild or quickjs) but is outside "
                            f"{TS_PKG}/; a test that needs it cannot run in Tier 1")
    if inside == 0:
        raise Unmeasured(f"{TS_PKG}/ holds no TestTypeScript* function, so the placement rule examined nothing "
                         f"and 'must run' would be satisfied by an empty package")
    return findings


def go_resolver(root):
    def resolve(patterns):
        if not patterns:
            return set()
        try:
            out = subprocess.run(["go", "list"] + list(patterns), cwd=root, capture_output=True, text=True)
        except FileNotFoundError:
            raise Unmeasured("`go` is not on PATH; the check cannot ask which packages a tier-1 pattern selects")
        if out.returncode != 0:
            raise Unmeasured(f"`go list` failed: {out.stderr.strip()[:200]}")
        return {l for l in out.stdout.split("\n") if l}
    # The package's own import path, from go itself. `go list -m` is not usable here: in this
    # repo's workspace (go.work, several modules) it prints EVERY module, one per line.
    own = resolve(["./" + TS_PKG])
    if len(own) != 1:
        raise Unmeasured(f"`go list ./{TS_PKG}` returned {sorted(own)}, want exactly one package")
    return resolve, next(iter(own))


def repo_inputs(root):
    tiers = yaml.safe_load(open(os.path.join(root, "tiers.yaml")))
    doc = open(os.path.join(root, "LANGUAGE_SUPPORT.md")).read()
    readme = open(os.path.join(root, "README.md")).read()
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
    return tiers, doc, readme, files


def run(tiers, doc, readme, files, resolve, ts_pkg_path):
    return check_docs(tiers, doc, readme) + check_placement(tiers, files, resolve, ts_pkg_path)


# ---------------------------------------------------------------------------------------------
# --self-test: each rule against a case that must be refused and one that must pass.

_MOD = "example.com/m"
_UNIVERSE = ["engine", "cmd/cleat", "plugins/x", "tests/integrity", "tests/typescript", "tests/typeahead"]


def _fake_resolve(patterns):
    """Mimics `go list` for the three pattern spellings that matter: `./a/...` (a and below),
    `./a` or `./a/` (exactly a), and `./a/pre...` (a wildcard over the final element)."""
    out = set()
    for p in patterns:
        q = p[2:] if p.startswith("./") else p
        if q.endswith("/..."):
            base = q[:-4]
            out |= {f"{_MOD}/{d}" for d in _UNIVERSE if d == base or d.startswith(base + "/")}
        elif q.endswith("..."):
            pre = q[:-3]
            out |= {f"{_MOD}/{d}" for d in _UNIVERSE if d.startswith(pre)}
        else:
            q = q.rstrip("/")
            out |= {f"{_MOD}/{d}" for d in _UNIVERSE if d == q}
    return out


_TS = _MOD + "/" + TS_PKG
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
_README = "# cleat\nA durable workflow engine. Go and Python are tier 1.\n"
_TESTS = {"tests/typescript/a_test.go": "package typescript\nfunc TestTypeScriptLabel(t *testing.T) {}\n",
          "engine/x_test.go": "package engine\nfunc TestSomething(t *testing.T) {}\n"}


def self_test():
    import copy
    bad = 0

    def expect(name, findings, want):
        nonlocal bad
        ok = (want is None and not findings) or (want is not None and any(want in f for f in findings))
        print(f"  {'ok  ' if ok else 'FAIL'}  {name}" + ("" if ok else f"   got: {findings}"))
        bad += 0 if ok else 1

    def expect_unmeasured(name, fn):
        nonlocal bad
        try:
            fn()
        except Unmeasured as e:
            print(f"  ok    {name}   ({str(e)[:90]})")
            return
        print(f"  FAIL  {name}   did not raise Unmeasured")
        bad += 1

    def docs(d=_DOC, r=_README, t=_TIERS):
        return check_docs(t, d, r)

    def place(t=_TIERS, f=_TESTS):
        return check_placement(t, f, _fake_resolve, _TS)

    print("-- negative control")
    expect("a consistent tree passes both rules", docs() + place(), None)

    print("-- rule 1: the doc table")
    d = _DOC.replace("| In development, not supported | ~1.1 MB | — |", "| Shipped | ~1.1 MB | 2 |")
    expect("known-positive: tier 2 and Shipped", docs(d), "tier '2' for TypeScript")
    expect("known-positive: Shipped with a dash tier", docs(_DOC.replace("In development, not supported", "Shipped")),
           "must say not built")
    for wording in ("Beta", "Available", "Stable, released", "Supported on Linux", "Experimental"):
        expect(f"known-positive: status '{wording}' is not an allowed non-claiming wording",
               docs(_DOC.replace("In development, not supported", wording)), "must say not built")
    expect("known-positive: a bold **2** is read as the digit (bold is formatting) and refused as a tier mismatch",
           docs(_DOC.replace("~1.1 MB | — |", "~1.1 MB | **2** |")), "says tier '**2**' for TypeScript")
    for cell in ("Tier 2", "Beta", "2 (beta)", ""):
        d = _DOC.replace("~1.1 MB | — |", f"~1.1 MB | {cell} |")
        expect(f"known-positive: Tier cell '{cell}' is not a bare 1, 2 or dash", docs(d), "not a bare")
    for wording in ("In development; supported on Linux", "Not built, but shipped in beta", "Planned (production in Q3)",
                    "In development \u2705 supported"):
        expect(f"known-positive: status '{wording}' starts well and claims support later",
               docs(_DOC.replace("In development, not supported", wording)), "must say not built")
    for wording in ("Not built", "In development, not supported", "Planned", "Not supported", "In development; not yet supported"):
        expect(f"negative control: status '{wording}' is fine for an ungranted language",
               docs(_DOC.replace("In development, not supported", wording)), None)
    expect("negative control: a bold dash tier cell is fine", docs(_DOC.replace("~1.1 MB | — |", "~1.1 MB | **—** |")), None)
    d = _DOC.replace("| **Rust** | cargo | Shipped | ~50 KB | 2 |", "| **Rust** | cargo | Shipped | ~50 KB | — |")
    expect("known-positive: the doc UNDER-claims a tier-2 language", docs(d), "tier '—' for Rust")
    d = _DOC.replace("| **TypeScript**", "| **Brainfuck** | x | Shipped | 1 B | 1 |\n| **TypeScript**")
    expect("known-positive: a language the guard does not know", docs(d), "not in this guard's language map")
    t = copy.deepcopy(_TIERS); t["tier2"]["components"][1]["status"] = "supported"
    expect("known-positive: tiers.yaml grants TypeScript tier 2 but the doc says dash", docs(t=t), "tier '—' for TypeScript")
    print("-- rule 1: README")
    for line in ("cleat supports TypeScript workflows.", "TypeScript is production-ready (tier 2).", "TypeScript: stable.",
                 "| TypeScript | \u2705 |", "JavaScript workflows are supported."):
        expect(f"known-positive: README line '{line}'", docs(r=_README + line + "\n"), "README.md:3")
    for line in ("TypeScript is in development and not supported.", "We write the dashboard in TypeScript."):
        expect(f"negative control: README line '{line}'", docs(r=_README + line + "\n"), None)

    print("-- rule 2: placement")
    f = dict(_TESTS); f["engine/ts_test.go"] = "package engine\nfunc TestTypeScriptThing(t *testing.T) {}\n"
    expect("known-positive: TestTypeScript* outside tests/typescript/", place(f=f), "engine/ts_test.go")
    for nm in ("TestJavaScriptGuestBuildsWithJavy", "TestJavyThing", "TestQuickJSThing"):
        f = dict(_TESTS); f["engine/zz_test.go"] = f"package engine\nfunc {nm}(t *testing.T) {{}}\n"
        expect(f"known-positive: {nm} outside tests/typescript/", place(f=f), "engine/zz_test.go")
    f = dict(_TESTS); f["engine/zz_test.go"] = "package engine\n// builds with esbuild\nfunc TestBuilds(t *testing.T) {}\n"
    expect("known-positive: an unprefixed test that names the toolchain", place(f=f), "names the TypeScript toolchain")
    f = dict(_TESTS); f["internal/plugingen/typescript_test.go"] = "package plugingen\nfunc TestGenerateTypeScript_Simple(t *testing.T) {}\n"
    expect("negative control: TestGenerateTypeScript_* (the client generator) is not a guest test", place(f=f), None)
    t = copy.deepcopy(_TIERS); t["tier2"]["packages"].remove(TS_PATTERN)
    expect("known-positive: package missing from tier2.packages", place(t=t), "does not list")
    for pat in ("./tests/...", "./tests/typescript", "./tests/typescript/", "./tests/type...", "./tests/typescript/..."):
        t = copy.deepcopy(_TIERS); t["tier1"]["packages"].append(pat)
        expect(f"known-positive: tier 1 gains '{pat}'", place(t=t), "selects the TypeScript package")
    for pat in ("./tests/integrity/...", "./tests/typea..."):
        t = copy.deepcopy(_TIERS); t["tier1"]["packages"].append(pat)
        expect(f"negative control: tier 1 gains '{pat}', which does not reach it", place(t=t), None)
    expect_unmeasured("UNMEASURED: tests/typescript/ holds no TestTypeScript* test",
                      lambda: place(f={"engine/x_test.go": _TESTS["engine/x_test.go"]}))
    t = copy.deepcopy(_TIERS); t["tier1"]["packages"] = ["./nothing/..."]
    expect_unmeasured("UNMEASURED: tier-1 resolution cannot show engine (the positive control)", lambda: place(t=t))
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
        tiers, doc, readme, files = repo_inputs(root)
        resolve, ts_pkg_path = go_resolver(root)
        findings = run(tiers, doc, readme, files, resolve, ts_pkg_path)
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
