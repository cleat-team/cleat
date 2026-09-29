#!/usr/bin/env python3
"""Extract a structurally-bounded fragment from a source file, so a LOC
comparison can be role-symmetric (tenant code | host runner | tests)
without freezing a hand-picked line range in prose. cleat#2597.

CLAUDE.md's own rule is the reason this exists rather than a comment saying
"lines 157-192": a frozen range drifts the moment anyone edits the
surrounding function, and a stale range silently counts the wrong code
forever. Every extraction here is bounded by the SOURCE'S OWN STRUCTURE --
a brace, a function name, a const declaration's closing backtick -- so it
tracks edits to the block it names and breaks loudly (raises, non-zero
exit) if that structure ever goes missing, rather than silently returning
a wrong-but-plausible answer.

Modes:
  go-brace-block <file> <start-regex>
      From the first line matching start-regex (inclusive) through the
      line whose brace nesting returns to the depth just BEFORE that line
      (inclusive). Depth is tracked by counting '{' and '}' outside of
      string/rune literals and comments -- adequate for this repo's plain
      control-flow code, not a general Go tokenizer.

  go-func <file> <func-name>
      From "func <func-name>(" (inclusive) through the closing "}" at
      column 0 (inclusive).

  ts-const-template <file> <const-name>
      From "const <const-name> = \`" (inclusive) through the next line
      that is exactly a closing backtick, optionally followed by ';'
      (inclusive).

  go-struct-field <file> <field-name>
      The line whose trimmed text starts with "<field-name> " or
      "<field-name>\t" (a Go struct field declaration), plus any
      unbroken run of full-line "//" comments immediately above it.
      Exists because a brace-block extraction of a dispatch site does not
      see the struct fields that carry its input/output across a
      caller/response boundary -- coordinator's review of #2621 caught
      that hub.go's TenantStepName/TenantStepRan fields were real wedge
      wiring the go-brace-block extraction of the dispatch block alone
      did not count.

  go-line <file> <regex>
      The first line matching regex, plus any unbroken run of full-line
      "//" comments immediately above it. For a single reference site --
      e.g. a composite-literal field assignment -- that go-struct-field's
      declaration-only pattern does not match.

Prints the extracted text to stdout. Exits 2 (UNMEASURED) if the start
marker or the closing boundary is not found -- never prints a partial or
empty extraction as if it were the real thing.
"""
import re
import sys


def fail(msg):
    print(f"UNMEASURED: {msg}", file=sys.stderr)
    print("UNMEASURED: this is a failure of the extractor, not a finding about the file", file=sys.stderr)
    sys.exit(2)


def strip_go_line_for_braces(line):
    # Adequate for this repo's extraction targets: no braces appear inside
    # this file's string/rune literals in the blocks we extract. Strip
    # comments so a commented-out brace does not shift the count.
    out = []
    in_string = False
    in_backtick = False
    i = 0
    while i < len(line):
        c = line[i]
        if in_backtick:
            out.append(' ')
            if c == '`':
                in_backtick = False
            i += 1
            continue
        if in_string:
            out.append(' ')
            if c == '\\':
                i += 2
                continue
            if c == '"':
                in_string = False
            i += 1
            continue
        if c == '/' and i + 1 < len(line) and line[i + 1] == '/':
            break
        if c == '"':
            in_string = True
            out.append(' ')
            i += 1
            continue
        if c == '`':
            in_backtick = True
            out.append(' ')
            i += 1
            continue
        out.append(c)
        i += 1
    return ''.join(out)


def go_brace_block(path, start_regex):
    lines = open(path).read().split('\n')
    pat = re.compile(start_regex)
    start = None
    for i, l in enumerate(lines):
        if pat.search(l):
            start = i
            break
    if start is None:
        fail(f"no line in {path} matches {start_regex!r}")
    depth = 0
    end = None
    for i in range(start, len(lines)):
        stripped = strip_go_line_for_braces(lines[i])
        depth += stripped.count('{') - stripped.count('}')
        if i > start and depth <= 0:
            end = i
            break
    if end is None:
        fail(f"brace block starting at {path}:{start+1} never closes")
    return '\n'.join(lines[start:end + 1]) + '\n'


def go_func(path, func_name):
    lines = open(path).read().split('\n')
    pat = re.compile(r'^func\s+' + re.escape(func_name) + r'\s*\(')
    start = None
    for i, l in enumerate(lines):
        if pat.match(l):
            start = i
            break
    if start is None:
        fail(f"no function {func_name!r} found in {path}")
    end = None
    for i in range(start + 1, len(lines)):
        if lines[i] == '}':
            end = i
            break
    if end is None:
        fail(f"function {func_name!r} in {path} never reaches a column-0 closing brace")
    return '\n'.join(lines[start:end + 1]) + '\n'


def ts_const_template(path, const_name):
    lines = open(path).read().split('\n')
    pat = re.compile(r'^const\s+' + re.escape(const_name) + r'\s*=\s*`\s*$')
    start = None
    for i, l in enumerate(lines):
        if pat.match(l):
            start = i
            break
    if start is None:
        fail(f"no template-literal const {const_name!r} found in {path}")
    end_pat = re.compile(r'^`;?\s*$')
    end = None
    for i in range(start + 1, len(lines)):
        if end_pat.match(lines[i]):
            end = i
            break
    if end is None:
        fail(f"template literal {const_name!r} in {path} never closes")
    return '\n'.join(lines[start:end + 1]) + '\n'


def go_struct_field(path, field_name):
    lines = open(path).read().split('\n')
    pat = re.compile(r'^\s*' + re.escape(field_name) + r'[ \t]')
    field_line = None
    for i, l in enumerate(lines):
        if pat.match(l):
            field_line = i
            break
    if field_line is None:
        fail(f"no struct field {field_name!r} found in {path}")
    start = field_line
    while start > 0 and lines[start - 1].lstrip().startswith('//'):
        start -= 1
    return '\n'.join(lines[start:field_line + 1]) + '\n'


def go_line(path, regex):
    lines = open(path).read().split('\n')
    pat = re.compile(regex)
    match_line = None
    for i, l in enumerate(lines):
        if pat.search(l):
            match_line = i
            break
    if match_line is None:
        fail(f"no line in {path} matches {regex!r}")
    start = match_line
    while start > 0 and lines[start - 1].lstrip().startswith('//'):
        start -= 1
    return '\n'.join(lines[start:match_line + 1]) + '\n'


def _write(path, text):
    with open(path, 'w') as fh:
        fh.write(text)


def self_test():
    """Verify every extraction mode against a KNOWN-POSITIVE (the marker is
    there, and the right text comes back) and a KNOWN-NEGATIVE (the marker
    is gone, and extraction fails loud rather than returning nothing or the
    wrong span) -- the same discipline check-dbos-pair-loc.py's own
    --self-test uses, applied here because coordinator's review of #2621
    asked directly whether this extractor's correctness was checked anywhere
    other than by hand: four markers were broken and confirmed to fail
    loud during that PR, but a hand-run falsification decays the moment
    nobody remembers to re-run it. This makes it part of the script instead.

    Only checks THIS FILE's own extraction logic -- not dbos-pair-loc.sh's
    use of it, and not whether the pair's README agrees with either (that
    is check-dbos-pair-loc.py's job, tracked for this pair as cleat#2632).
    """
    import tempfile

    failures = []
    tmpdir = tempfile.mkdtemp(prefix='dbos-pair-loc-extract-selftest-')

    def check(label, fn, expect_substring):
        try:
            result = fn()
        except SystemExit as e:
            failures.append(f"{label}: known-positive case exited ({e.code}) instead of returning text")
            return
        if expect_substring not in result:
            failures.append(f"{label}: known-positive case did not contain {expect_substring!r}: got {result!r}")

    def check_fails(label, fn):
        try:
            fn()
        except SystemExit as e:
            if e.code != 2:
                failures.append(f"{label}: known-negative case exited {e.code}, want 2")
            return
        failures.append(f"{label}: known-negative case returned normally instead of exiting 2")

    # go-brace-block
    p = f"{tmpdir}/brace.go"
    _write(p, "func f() {\n\tif x.Y {\n\t\tdoThing()\n\t}\n}\n")
    check("go-brace-block", lambda: go_brace_block(p, r'if x\.Y'), "doThing()")
    check_fails("go-brace-block (marker missing)", lambda: go_brace_block(p, r'if x\.NOPE'))
    unclosed = f"{tmpdir}/brace_unclosed.go"
    _write(unclosed, "func f() {\n\tif x.Y {\n\t\tdoThing()\n")
    check_fails("go-brace-block (never closes)", lambda: go_brace_block(unclosed, r'if x\.Y'))

    # go-func
    p = f"{tmpdir}/func.go"
    _write(p, "func TestThing(t *testing.T) {\n\tassertSomething()\n}\n")
    check("go-func", lambda: go_func(p, "TestThing"), "assertSomething()")
    check_fails("go-func (marker missing)", lambda: go_func(p, "TestNope"))

    # ts-const-template
    p = f"{tmpdir}/tmpl.ts"
    _write(p, "const SOURCE = `\n  the payload\n`;\n")
    check("ts-const-template", lambda: ts_const_template(p, "SOURCE"), "the payload")
    check_fails("ts-const-template (marker missing)", lambda: ts_const_template(p, "NOPE"))

    # go-struct-field
    p = f"{tmpdir}/field.go"
    _write(p, "type S struct {\n\t// a comment\n\tFieldName string `json:\"field_name\"`\n}\n")
    check("go-struct-field", lambda: go_struct_field(p, "FieldName"), "a comment")
    check_fails("go-struct-field (marker missing)", lambda: go_struct_field(p, "NopeField"))

    # go-line
    p = f"{tmpdir}/line.go"
    _write(p, "func f() {\n\t// why this line exists\n\tResultField: someVar,\n}\n")
    check("go-line", lambda: go_line(p, r'^\s*ResultField:'), "why this line exists")
    check_fails("go-line (marker missing)", lambda: go_line(p, r'^\s*NopeField:'))

    # FileNotFoundError -> UNMEASURED (2), for every mode, not a traceback
    for mode, arg in [
        ('go-brace-block', 'x'), ('go-func', 'x'),
        ('ts-const-template', 'x'), ('go-struct-field', 'x'), ('go-line', 'x'),
    ]:
        try:
            run_mode(mode, f"{tmpdir}/does-not-exist.go", arg)
            failures.append(f"{mode} (missing file): returned normally instead of exiting 2")
        except SystemExit as e:
            if e.code != 2:
                failures.append(f"{mode} (missing file): exited {e.code}, want 2")

    if failures:
        print("self-test FAILED:\n" + "\n".join(f"  {f}" for f in failures), file=sys.stderr)
        return 1
    print("self-test passed")
    return 0


def run_mode(mode, path, arg):
    """The one dispatch table both main() and self_test() use -- a second,
    independently-maintained copy of this if/elif chain is exactly the kind
    of drift this file's own extraction logic exists to avoid in the README
    tables it feeds."""
    try:
        if mode == 'go-brace-block':
            return go_brace_block(path, arg)
        elif mode == 'go-func':
            return go_func(path, arg)
        elif mode == 'ts-const-template':
            return ts_const_template(path, arg)
        elif mode == 'go-struct-field':
            return go_struct_field(path, arg)
        elif mode == 'go-line':
            return go_line(path, arg)
        print(f"unknown mode {mode!r}", file=sys.stderr)
        sys.exit(2)
    except FileNotFoundError:
        fail(f"{path} does not exist")


def main():
    if len(sys.argv) == 2 and sys.argv[1] == '--self-test':
        sys.exit(self_test())
    if len(sys.argv) != 4:
        print(
            f"usage: {sys.argv[0]} go-brace-block|go-func|ts-const-template|go-struct-field|go-line <file> <name-or-regex>",
            file=sys.stderr,
        )
        sys.exit(2)
    mode, path, arg = sys.argv[1], sys.argv[2], sys.argv[3]
    sys.stdout.write(run_mode(mode, path, arg))


if __name__ == '__main__':
    main()
