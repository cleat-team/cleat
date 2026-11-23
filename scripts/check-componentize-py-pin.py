#!/usr/bin/env python3
"""componentize-py is pinned in ONE place, and nothing else installs it unpinned or names another version (cleat#4355).

    scripts/check-componentize-py-pin.py             scan the tracked tree
    scripts/check-componentize-py-pin.py --self-test

Which standard-library modules a Python component contains is decided by componentize-py at build time (cleat#4266), and the README's tables were
measured on one version. So the version users build with has to be the version CI tests, and the way to keep that true is to make it a property
CI checks rather than a sentence someone wrote. `python-sdk/componentize-py-requirements.txt` states the version; `scripts/install-componentize-py.sh`
installs from it. This refuses, in a tracked workflow, Makefile, script or doc:

  1. a `pip install` line that names componentize-py without `==<the pinned version>`;
  2. a range or floor on it (`>=0.12.0`, `~=`, `<`), anywhere;
  3. `componentize-py==<some other version>`, anywhere;
  4. `PINNED_COMPONENTIZE_PY` in python-sdk/scripts/build_wasm.py disagreeing with the requirements file.

Exit 0 clean, 1 a finding, 2 UNMEASURED: it could not establish the pin or list the tree, which is a failure of the check, not a finding about the tree.
Comment lines are not read (a comment may say what a version used to be); a `pip install` continued with a backslash is read as one line.
"""
import re
import subprocess
import sys

REQ = "python-sdk/componentize-py-requirements.txt"
BUILD = "python-sdk/scripts/build_wasm.py"
# Where a reader is told to install it. History (changelog, the plan, design notes) is not instruction and may name any version.
# Wide on purpose: a guard whose scope is narrower than "anywhere a user or a job is told to install it" certifies a tree it did not read
# (cleat-review on #4357 found three such places outside the first version of this pattern: Go that prints the hint, a Dockerfile, a test plan).
SCOPE = re.compile(
    r"^(\.github/.*\.(ya?ml|sh|py)|Makefile|python-sdk/Makefile|python-sdk/scripts/.*|python-sdk/.*\.md|README\.md|"
    r"docs/.*\.md|scripts/.*\.(sh|py|ya?ml)|scripts/.*[Dd]ockerfile.*|(.*/)?[Dd]ockerfile(\..*)?|.*\.[Dd]ockerfile|"
    r"(cmd|wasm|engine|cleat|internal|tests)/.*\.go|tests/.*\.(sh|py|ya?ml)|.*/(skip|SKIP)[^/]*\.md)$"
)
SKIP = {REQ, "scripts/check-componentize-py-pin.py", "scripts/install-componentize-py.sh"}
NAME = r"(?<![\w/.-])componentize[-_]py(?![\w-])"
RANGE = re.compile(NAME + r"\s*(>=|<=|~=|!=|>|<)")
OTHER = re.compile(NAME + r"==([0-9][0-9A-Za-z.+!-]*)")
# A message that interpolates the constant checked by check_constant() is as pinned as the literal is.
CONSTANT = "{PINNED_COMPONENTIZE_PY}"
PIP = re.compile(r"\bpip3?\s+install\b")


def pinned_version(text):
    found = re.findall(r"(?m)^componentize-py==([0-9][0-9A-Za-z.+!-]*)\s*(?:\\)?\s*$", text)
    return found[0] if len(found) == 1 else None


def logical_lines(text):
    """Source lines with backslash continuations joined; each is (first line number, text)."""
    out, cur, start = [], "", 0
    for n, raw in enumerate(text.splitlines(), 1):
        if not cur:
            start = n
        if raw.rstrip().endswith("\\"):
            cur += raw.rstrip()[:-1] + " "
            continue
        out.append((start, cur + raw))
        cur = ""
    if cur:
        out.append((start, cur))
    return out


def scan(path, text, want):
    findings = []
    for n, line in logical_lines(text):
        if line.lstrip().startswith("#") and not path.endswith(".md"):  # a Markdown "#" line is a heading, which is read
            continue
        if RANGE.search(line):
            findings.append((path, n, "a range or floor on componentize-py; pin it exactly to %s" % want))
        for m in OTHER.finditer(line):
            if m.group(1) != want:
                findings.append((path, n, "componentize-py==%s, but the pin is %s" % (m.group(1), want)))
        pinned = re.search(NAME + "==" + re.escape(want) + r"(?![0-9A-Za-z.+!-])", line) or re.search(NAME + "==" + re.escape(CONSTANT), line)
        if PIP.search(line) and re.search(NAME, line) and not pinned:
            if not RANGE.search(line):  # already reported as a range
                findings.append((path, n, "a pip install of componentize-py that is not ==%s" % want))
    return findings


def check_constant(text, want):
    m = re.findall(r'(?m)^PINNED_COMPONENTIZE_PY\s*=\s*"([^"]+)"', text)
    if len(m) != 1:
        return [(BUILD, 0, "expected exactly one PINNED_COMPONENTIZE_PY = \"...\" line, found %d" % len(m))]
    if m[0] != want:
        return [(BUILD, 0, "PINNED_COMPONENTIZE_PY is %s but %s pins %s" % (m[0], REQ, want))]
    return []


def unmeasured(why):
    print("UNMEASURED: %s. This is a failure of the check, not a finding about the tree." % why)
    return 2


def main():
    try:
        want = pinned_version(open(REQ, encoding="utf-8").read())
    except OSError as e:
        return unmeasured("cannot read %s (%s)" % (REQ, e))
    if want is None:
        return unmeasured("%s must hold exactly one 'componentize-py==<version>' line" % REQ)
    ls = subprocess.run(["git", "ls-files"], capture_output=True, text=True)
    if ls.returncode != 0 or not ls.stdout.strip():
        return unmeasured("`git ls-files` failed or listed nothing (rc=%s)" % ls.returncode)
    findings, read = [], 0
    for path in ls.stdout.splitlines():
        if path in SKIP or not SCOPE.match(path):
            continue
        try:
            text = open(path, encoding="utf-8").read()
        except (OSError, UnicodeDecodeError):
            continue
        read += 1
        findings += scan(path, text, want)
    if read < 20:
        return unmeasured("only %d tracked files were in scope; the scope pattern no longer matches the tree" % read)
    try:
        findings += check_constant(open(BUILD, encoding="utf-8").read(), want)
    except OSError as e:
        return unmeasured("cannot read %s (%s)" % (BUILD, e))
    if findings:
        print("componentize-py pin: %d finding(s), the pin is %s (%s)" % (len(findings), want, REQ))
        for path, n, why in findings:
            print("  %s:%s: %s" % (path, n, why))
        return 1
    print("OK: componentize-py is pinned to %s and %d tracked files agree" % (want, read))
    return 0


def self_test():
    want, bad = "0.25.1", []

    def expect(label, got, n):
        if len(got) != n:
            bad.append("%s: wanted %d finding(s), got %d: %r" % (label, n, len(got), got))

    expect("pinned install", scan("a.yml", "          pip install componentize-py==0.25.1\n", want), 0)
    expect("pinned and quoted", scan("a.md", "pip install cleat-sdk 'componentize-py==0.25.1'\n", want), 0)
    expect("KNOWN POSITIVE bare install", scan("a.yml", "          pip install componentize-py\n", want), 1)
    expect("KNOWN POSITIVE floor, quoted", scan("a.yml", '          pip install "componentize-py>=0.12.0"\n', want), 1)
    expect("KNOWN POSITIVE other version", scan("a.md", "pip install componentize-py==0.24.0\n", want), 2)
    expect("KNOWN POSITIVE prefix of the pin is not the pin", scan("a.yml", "pip install componentize-py==0.25.10\n", want), 2)
    expect("KNOWN POSITIVE floor in prose, with spaces", scan("a.md", "- **componentize-py >= 0.12.0** is required\n", want), 1)
    expect("the requirements file's own name is not the package", scan("a.yml", "pip install -r python-sdk/componentize-py-requirements.txt\n", want), 0)
    expect("an interpolated pin", scan("a.py", 'print(f"pip install componentize-py=={PINNED_COMPONENTIZE_PY}")\n', want), 0)
    expect("KNOWN POSITIVE a bare install in a string", scan("a.py", 'print("Install with: pip install componentize-py")\n', want), 1)
    expect("floor in prose, no space", scan("a.md", "- componentize-py>=0.12.0 is required\n", want), 1)
    expect("comment naming an old floor", scan("a.yml", '      # was pip install "componentize-py>=0.12.0"\n', want), 0)
    expect("install through the hashed file, no name on the line", scan("a.yml", "pip install --require-hashes -r python-sdk/componentize-py-requirements.txt\n", want), 0)
    expect("KNOWN POSITIVE continued install", scan("a.yml", "pip install \\\n  --upgrade \\\n  componentize-py\n", want), 1)
    expect("KNOWN POSITIVE a Markdown heading is not a comment", scan("a.md", "# pip install componentize-py\n", want), 1)
    expect("a step NAME is not an install", scan("a.yml", "      - name: Install componentize-py\n", want), 0)
    expect("a pip install of something else", scan("a.yml", "pip install wasm-tools\n", want), 0)
    expect("constant agrees", check_constant('PINNED_COMPONENTIZE_PY = "0.25.1"\n', want), 0)
    expect("KNOWN POSITIVE constant disagrees", check_constant('PINNED_COMPONENTIZE_PY = "0.12.0"\n', want), 1)
    expect("KNOWN POSITIVE constant absent", check_constant("x = 1\n", want), 1)
    for text, ver in (('componentize-py==0.25.1 \\\n    --hash=sha256:aa\n', "0.25.1"), ("# componentize-py==9.9.9\n", None), ("componentize-py==1.0\ncomponentize-py==2.0\n", None)):
        if pinned_version(text) != ver:
            bad.append("pinned_version(%r) = %r, wanted %r" % (text, pinned_version(text), ver))
    if bad:
        print("self-test FAILED:\n  " + "\n  ".join(bad))
        return 1
    print("OK: self-test passed")
    return 0


if __name__ == "__main__":
    sys.exit(self_test() if "--self-test" in sys.argv[1:] else main())
