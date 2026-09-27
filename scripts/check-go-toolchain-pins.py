#!/usr/bin/env python3
"""Every Go toolchain in this tree is the one go.mod names (cleat#2216).

The release is built, tested and linted on the toolchain users will run, so there is ONE source for the
version and nothing else states it:

  1. go.mod and go.work carry `go X.Y.Z` and `toolchain goX.Y.Z`, and every module and the workspace carry
     the SAME pair. A module one patch behind builds fine and lints on a different compiler.
  2. Every `actions/setup-go` step reads its version from `go-version-file: go.mod` (or go.work, for the
     job that spans modules). A `go-version:` input on a setup-go step is refused, whatever it says: that
     is how CI floated on `stable` (which resolved to 1.27.1 the day #2215's panic appeared) and how
     engine-race.yml sat on a hard-coded 1.26.
  3. A matrix key named `go-version` is refused except ONE: ci.yml's test-go matrix, a LABEL that only exists
     because the job name renders it and the job names are required status checks
     (.github/required-checks.txt). It must not be read by a setup-go step, which rule 2 already enforces.
  4. Every `FROM golang:` in a Dockerfile is `golang:<toolchain>-...`, the exact toolchain, not a floating
     minor (`golang:1.26-bookworm` moved under the image whenever a patch shipped).

Exit status, and why there are three:

  0  the tree is consistent
  1  a finding: something states a Go version that is not go.mod's
  2  this check could not establish what it measures (no go.mod, no setup-go step, no Dockerfile that builds
     Go). A scan that finds nothing agrees with every tree, correct or not, so finding nothing is a failure
     of the CHECK, reported as such and never as a pass. See CLAUDE.md, "Is this result real?".

`--self-test` builds fixture trees and asserts on the TEXT of each verdict, not only the status.

Files are read from `git ls-files`, not by walking the tree, so a scratch checkout under .claude/worktrees is
not mistaken for the repository.
"""
import os
import re
import subprocess
import sys
import tempfile

MIN_SETUP_GO = 10  # setup-go steps in the tree today: 25. Fewer means the scan read the wrong thing.
MIN_DOCKER_GO = 1

GO_DIRECTIVE = re.compile(r"^go[ \t]+(\d+\.\d+\.\d+)[ \t]*$", re.M)
TOOLCHAIN = re.compile(r"^toolchain[ \t]+go(\d+\.\d+\.\d+)[ \t]*$", re.M)
SETUP_GO = re.compile(r"^(\s*)-?\s*uses:\s*actions/setup-go@")
VERSION_FILE = re.compile(r"^\s*go-version-file:\s*['\"]?(go\.mod|go\.work)['\"]?\s*$")
VERSION_INPUT = re.compile(r"^\s*go-version:\s*(.*?)\s*$")
FROM_GO = re.compile(r"^\s*FROM\s+(?:--platform=\S+\s+)?golang:(\S+)", re.I)


class Unmeasured(Exception):
    pass


def tracked(root, patterns):
    try:
        out = subprocess.run(["git", "-C", root, "ls-files", "-z", *patterns], capture_output=True, check=True).stdout
    except (subprocess.CalledProcessError, FileNotFoundError) as e:
        raise Unmeasured(f"git ls-files failed in {root}: {e}")
    return [f for f in out.decode().split("\0") if f]


def read(root, rel):
    with open(os.path.join(root, rel), encoding="utf-8") as fh:
        return fh.read()


def step_blocks(lines, i):
    """The lines of the step whose `uses:` is at index i: to the next step at the same or lesser indent."""
    indent = len(lines[i]) - len(lines[i].lstrip())
    # The `- ` that opens the step may be on this line (`- uses:`) or on an earlier `- name:` line. Walk back
    # to it so the `with:` that follows a `name:` is inside the block we return.
    start = i
    while start > 0 and not lines[start].lstrip().startswith("- "):
        start -= 1
    step_indent = len(lines[start]) - len(lines[start].lstrip())
    end = i + 1
    while end < len(lines):
        s = lines[end]
        if s.strip() and not s.lstrip().startswith("#"):
            ind = len(s) - len(s.lstrip())
            if ind <= step_indent and s.lstrip().startswith("- "):
                break
            if ind < step_indent:
                break
        end += 1
    return lines[start:end]


def check(root):
    findings = []

    # 1. go.mod / go.work
    mods = tracked(root, ["go.mod", "*/go.mod", "*/*/go.mod", "*/*/*/go.mod", "go.work"])
    if "go.mod" not in mods:
        raise Unmeasured("no root go.mod is tracked: there is nothing to compare against")
    ref_text = read(root, "go.mod")
    ref_go, ref_tc = GO_DIRECTIVE.search(ref_text), TOOLCHAIN.search(ref_text)
    if not ref_go or not ref_tc:
        raise Unmeasured("go.mod has no `go X.Y.Z` and `toolchain goX.Y.Z` pair to compare against (a bare `go 1.27` does not name a toolchain)")
    want_go, want_tc = ref_go.group(1), ref_tc.group(1)
    for m in sorted(mods):
        text = read(root, m)
        g, t = GO_DIRECTIVE.search(text), TOOLCHAIN.search(text)
        if not g or not t:
            findings.append(f"{m}: needs `go {want_go}` and `toolchain go{want_tc}` (found go={g.group(1) if g else None}, toolchain={t.group(1) if t else None})")
        elif (g.group(1), t.group(1)) != (want_go, want_tc):
            findings.append(f"{m}: go {g.group(1)} / toolchain go{t.group(1)}, but go.mod says go {want_go} / toolchain go{want_tc}")

    # 2 and 3. workflows
    wf = tracked(root, [".github/workflows/*.yml", ".github/workflows/*.yaml"])
    setup_go = 0
    for f in sorted(wf):
        lines = read(root, f).split("\n")
        for i, line in enumerate(lines):
            if line.lstrip().startswith("#"):
                continue
            if SETUP_GO.match(line):
                setup_go += 1
                block = step_blocks(lines, i)
                files = [VERSION_FILE.match(b) for b in block]
                if not any(files):
                    findings.append(f"{f}:{i + 1}: setup-go step has no `go-version-file: go.mod` (or go.work)")
                for b in block:
                    v = VERSION_INPUT.match(b)
                    if v and not b.lstrip().startswith("#"):
                        findings.append(f"{f}:{i + 1}: setup-go step passes `go-version: {v.group(1)}`; the version comes from go-version-file only")
        # matrix keys: the single allowed label
        in_test_go = False
        for i, line in enumerate(lines):
            if re.match(r"^  test-go:\s*$", line):
                in_test_go = True
            elif re.match(r"^  [A-Za-z0-9_-]+:\s*$", line):
                in_test_go = False
            v = VERSION_INPUT.match(line)
            if v and not line.lstrip().startswith("#") and "[" in v.group(1):
                if not (f.endswith("/ci.yml") and in_test_go):
                    findings.append(f"{f}:{i + 1}: a `go-version` matrix key outside ci.yml's test-go: {v.group(1)}")
    if setup_go < MIN_SETUP_GO:
        raise Unmeasured(f"found {setup_go} setup-go steps in {len(wf)} workflow files, expected at least {MIN_SETUP_GO}: the scan read the wrong files")

    # 4. Dockerfiles
    dockerfiles = tracked(root, ["Dockerfile", "*Dockerfile", "*/Dockerfile", "*/*/Dockerfile", "*/*/*.Dockerfile", "*/*.Dockerfile"])
    docker_go = 0
    for d in sorted(set(dockerfiles)):
        for i, line in enumerate(read(root, d).split("\n")):
            m = FROM_GO.match(line)
            if m:
                docker_go += 1
                tag = m.group(1)
                if not (tag == want_tc or tag.startswith(want_tc + "-")):
                    findings.append(f"{d}:{i + 1}: `FROM golang:{tag}` is not the toolchain go.mod names (golang:{want_tc}-...)")
    if docker_go < MIN_DOCKER_GO:
        raise Unmeasured("no `FROM golang:` line found in any tracked Dockerfile: the scan read the wrong files")

    return findings, {"toolchain": want_tc, "modules": len(mods), "setup_go": setup_go, "docker": docker_go}


def main(root):
    try:
        findings, facts = check(root)
    except Unmeasured as e:
        print(f"UNMEASURED: {e}", file=sys.stderr)
        print("This is a failure of the check, not a finding about the tree.", file=sys.stderr)
        return 2
    if findings:
        print(f"check-go-toolchain-pins: {len(findings)} finding(s); go.mod names go{facts['toolchain']}:")
        for f in findings:
            print(f"  {f}")
        return 1
    print(f"check-go-toolchain-pins: OK, go{facts['toolchain']} in {facts['modules']} go.mod/go.work files, "
          f"{facts['setup_go']} setup-go steps and {facts['docker']} Dockerfile FROM lines")
    return 0


# ---------------------------------------------------------------------------------------------------------
# self-test
# ---------------------------------------------------------------------------------------------------------

GOOD_WF = """name: ci
jobs:
  test-go:
    name: Test Go (${{ matrix.package.name }}) on ${{ matrix.go-version }}
    strategy:
      matrix:
        go-version: ["1.26"]
    steps:
      - uses: actions/checkout@v7
      - name: Setup Go
        uses: actions/setup-go@v7
        with:
          go-version-file: go.mod
"""


def fixture(**over):
    files = {
        "go.mod": "module a\n\ngo 1.27.0\n\ntoolchain go1.27.1\n",
        "sub/go.mod": "module b\n\ngo 1.27.0\n\ntoolchain go1.27.1\n",
        "go.work": "go 1.27.0\n\ntoolchain go1.27.1\n\nuse (\n\t.\n\t./sub\n)\n",
        ".github/workflows/ci.yml": GOOD_WF,
        ".github/workflows/other.yml": "jobs:\n  a:\n    steps:\n      - uses: actions/setup-go@v7\n        with:\n          go-version-file: go.work\n",
        "Dockerfile": "FROM golang:1.27.1-bookworm AS builder\nFROM debian:bookworm\n",
    }
    files.update(over)
    return files


def run_fixture(files, min_setup=1):
    global MIN_SETUP_GO
    old = MIN_SETUP_GO
    MIN_SETUP_GO = min_setup
    try:
        with tempfile.TemporaryDirectory() as d:
            subprocess.run(["git", "init", "-q", d], check=True)
            for rel, text in files.items():
                p = os.path.join(d, rel)
                os.makedirs(os.path.dirname(p), exist_ok=True)
                with open(p, "w") as fh:
                    fh.write(text)
            subprocess.run(["git", "-C", d, "add", "-A"], check=True)
            import io
            from contextlib import redirect_stderr, redirect_stdout
            out, err = io.StringIO(), io.StringIO()
            with redirect_stdout(out), redirect_stderr(err):
                rc = main(d)
            return rc, out.getvalue() + err.getvalue()
    finally:
        MIN_SETUP_GO = old


def self_test():
    failures = []

    def expect(name, files, want_rc, want_text, **kw):
        rc, text = run_fixture(files, **kw)
        ok = rc == want_rc and want_text in text
        print(f"  {'ok  ' if ok else 'FAIL'} {name}: rc={rc} (want {want_rc}), text has {want_text!r}: {want_text in text}")
        if not ok:
            failures.append(name)
            print(text)

    print("check-go-toolchain-pins --self-test")
    expect("a consistent tree passes", fixture(), 0, "OK, go1.27.1")
    expect("go-version: stable on a setup-go step",
           fixture(**{".github/workflows/other.yml": "jobs:\n  a:\n    steps:\n      - uses: actions/setup-go@v7\n        with:\n          go-version: stable\n"}),
           1, "go-version: stable")
    expect("a hard-coded 1.26 on a setup-go step",
           fixture(**{".github/workflows/other.yml": "jobs:\n  a:\n    steps:\n      - name: Setup Go\n        uses: actions/setup-go@v7\n        with:\n          go-version: \"1.26\"\n          cache: true\n"}),
           1, 'go-version: "1.26"')
    expect("a setup-go step with no version source",
           fixture(**{".github/workflows/other.yml": "jobs:\n  a:\n    steps:\n      - uses: actions/setup-go@v7\n"}),
           1, "no `go-version-file")
    expect("a go-version matrix key outside test-go",
           fixture(**{".github/workflows/other.yml": "jobs:\n  a:\n    strategy:\n      matrix:\n        go-version: [\"1.25\"]\n    steps:\n      - uses: actions/setup-go@v7\n        with:\n          go-version-file: go.mod\n"}),
           1, "matrix key outside ci.yml's test-go")
    expect("a member one patch behind the toolchain",
           fixture(**{"sub/go.mod": "module b\n\ngo 1.27.0\n\ntoolchain go1.27.0\n"}),
           1, "sub/go.mod: go 1.27.0 / toolchain go1.27.0, but go.mod says go 1.27.0 / toolchain go1.27.1")
    expect("go.work behind",
           fixture(**{"go.work": "go 1.26.0\n\ntoolchain go1.26.4\n\nuse (\n\t.\n)\n"}),
           1, "go.work: go 1.26.0")
    expect("a module with no toolchain line",
           fixture(**{"sub/go.mod": "module b\n\ngo 1.27.0\n"}),
           1, "sub/go.mod: needs `go 1.27.0` and `toolchain go1.27.1`")
    expect("a floating-minor Dockerfile",
           fixture(**{"Dockerfile": "FROM golang:1.26-bookworm AS builder\n"}),
           1, "golang:1.26-bookworm")
    expect("a Dockerfile one patch behind",
           fixture(**{"Dockerfile": "FROM golang:1.27.0-bookworm\n"}),
           1, "golang:1.27.0-bookworm")
    # UNMEASURED: nothing to compare, nothing scanned.
    expect("no setup-go step at all is UNMEASURED, not a pass",
           fixture(**{".github/workflows/ci.yml": "name: ci\n", ".github/workflows/other.yml": "name: x\n"}),
           2, "UNMEASURED: found 0 setup-go steps")
    expect("no Dockerfile that builds Go is UNMEASURED", fixture(**{"Dockerfile": "FROM debian:bookworm\n"}), 2, "UNMEASURED: no `FROM golang:`")
    expect("a bare `go 1.27` is UNMEASURED", fixture(**{"go.mod": "module a\n\ngo 1.27\n"}), 2, "UNMEASURED: go.mod has no `go X.Y.Z`")
    # A comment that mentions a version is not a version.
    expect("a comment about `go-version: stable` is not a use",
           fixture(**{".github/workflows/other.yml": "jobs:\n  a:\n    steps:\n      # the old `go-version: stable` floated\n      - uses: actions/setup-go@v7\n        with:\n          go-version-file: go.mod\n"}),
           0, "OK")
    if failures:
        print(f"SELF-TEST FAILED: {failures}")
        return 1
    print("self-test passed")
    return 0


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--self-test":
        sys.exit(self_test())
    root = sys.argv[1] if len(sys.argv) > 1 else os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    sys.exit(main(root))
