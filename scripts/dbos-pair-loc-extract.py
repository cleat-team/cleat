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


def main():
    if len(sys.argv) != 4:
        print(f"usage: {sys.argv[0]} go-brace-block|go-func|ts-const-template <file> <name-or-regex>", file=sys.stderr)
        sys.exit(2)
    mode, path, arg = sys.argv[1], sys.argv[2], sys.argv[3]
    try:
        if mode == 'go-brace-block':
            sys.stdout.write(go_brace_block(path, arg))
        elif mode == 'go-func':
            sys.stdout.write(go_func(path, arg))
        elif mode == 'ts-const-template':
            sys.stdout.write(ts_const_template(path, arg))
        else:
            print(f"unknown mode {mode!r}", file=sys.stderr)
            sys.exit(2)
    except FileNotFoundError:
        fail(f"{path} does not exist")


if __name__ == '__main__':
    main()
