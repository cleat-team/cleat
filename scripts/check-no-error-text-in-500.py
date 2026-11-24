#!/usr/bin/env python3
"""check-no-error-text-in-500.py -- cleat#3878.

A cmd/cleat-worker handler that answered a failed store call with
`s.writeError(w, 500, err.Error())` sent the database driver's text to the
client: hostnames, statement text and role names, none of which the client can
act on, with a 500 that says the request is not worth sending again. cleat#3878
swept 38 such sites onto `apiServer.writeStoreError` (store_errors.go), which
keeps a domain error's own status and message and answers anything else with a
fixed body, a request id and the full error in the log.

RULE. In a non-test .go file under cmd/cleat-worker/, a 500 answer
(`writeError(w, 500, ...)`, `writeError(w, http.StatusInternalServerError, ...)`,
`writeJSON(w, <either>, ...)`) must not carry an error's text. Its arguments,
with string-literal contents masked so a sentence ABOUT err.Error() is not read
as code, are flagged when they contain

  - a `.Error()` call on anything: `x.Error()` as the whole message,
    `"checking opt-in: " + err.Error()` after a sentence, or inside a map;
  - a formatter (`fmt.Sprintf`, `Errorf` ...) given an identifier that is an
    error by its name -- a whole word ending in err/Err (err, serr, vErr,
    schemaErr, aerr), which is how the `%v` form hides; or
  - a bare identifier assigned `x.Error()` earlier in the same function
    (`msg := err.Error()` ... `writeError(w, 500, msg)`), which no call-shape
    rule sees and which handleAdminOpError's default branch used (cleat#3883).

Any identifier is covered: the sweep found the same defect under `err`, `verr`,
`serr`, `vErr`, `pErr` and `defErr`, so a guard keyed on the name `err` missed
eleven of them. Use writeStoreError (or writeStoreErrorDuring, to keep a
sentence of your own in the body), or log the error and answer a sentence of
your own. A line, or the one above it, carrying `// error-text-ok: <reason>`
is allowed, for an error that is our own sentence written for the caller.

NOT COVERED, and said here so the green is not read as more than it is: a
status other than 500 (a 4xx carrying a domain error's text is the handler's
own prose; a 503 is not written this way); a 5xx written some other way
(http.Error); an error value reaching the message through a channel the guard
cannot follow (a struct field, a helper's return value, an alias assigned in
another function); the streaming endpoints' `error` events; and every package
outside cmd/cleat-worker. The identifier rule is by NAME: an error held in a
variable not named ...err/...Err and formatted with %v is not seen.

Exit codes: 0 clean, 1 a violation was found, 2 the check could not run.
"""
import re
import subprocess
import sys

def strip_comments(src):
    """Remove // and /* */ comments, preserving line counts and the
    contents of string/backtick literals (which may contain // or /* and
    must not be treated as comments -- and which may themselves quote
    "plugin.Rebind(" in prose, which must not be treated as a call)."""
    out = []
    i, n = 0, len(src)
    in_string = None
    while i < n:
        c = src[i]
        if in_string:
            out.append(c)
            if in_string == '"' and c == "\\" and i + 1 < n:
                out.append(src[i + 1])
                i += 2
                continue
            if c == in_string:
                in_string = None
            i += 1
            continue
        if c in ('"', "`"):
            in_string = c
            out.append(c)
            i += 1
            continue
        if c == "/" and i + 1 < n and src[i + 1] == "/":
            j = src.find("\n", i)
            if j == -1:
                i = n
            else:
                out.append("\n")
                i = j + 1
            continue
        if c == "/" and i + 1 < n and src[i + 1] == "*":
            j = src.find("*/", i + 2)
            if j == -1:
                i = n
            else:
                out.append("\n" * src[i:j + 2].count("\n"))
                i = j + 2
            continue
        out.append(c)
        i += 1
    return "".join(out)


STATUS = r"(?:500|http\.StatusInternalServerError)"
CALL = re.compile(r"\bwrite(?:Error|JSON)\(\s*w\s*,\s*" + STATUS + r"\s*,")
ERR_TEXT = re.compile(r"[A-Za-z_]\w*\.Error\(\)")
FORMATTER = re.compile(r"\bfmt\.(?:Sprintf|Sprint|Sprintln|Errorf)\(")
# An identifier that IS an error by its name: err, serr, vErr, schemaErr, aerr -- a whole word ending in
# err/Err. Not `errors` or `errNoTenant`: the pattern is about a value being formatted into the message.
ERR_IDENT = re.compile(r"\b(?:[a-z]\w*)?[eE]rr\b")
ALIAS = re.compile(r"\b([A-Za-z_]\w*)\s*:?=\s*[A-Za-z_]\w*\.Error\(\)")
FUNC = re.compile(r"^func\b", re.M)


def mask_strings(src):
    """Blank the CONTENT of every string and raw-string literal (newlines kept, so line numbers hold), so
    that a sentence mentioning `err.Error()` or `%v` is never read as code."""
    out = []
    i, n = 0, len(src)
    while i < n:
        c = src[i]
        if c in ('"', "`"):
            q = c
            out.append(c)
            i += 1
            while i < n:
                d = src[i]
                if q == '"' and d == "\\" and i + 1 < n:
                    out.append("  ")
                    i += 2
                    continue
                if d == q:
                    break
                out.append("\n" if d == "\n" else "_")
                i += 1
            if i < n:
                out.append(q)
                i += 1
            continue
        if c == "'":
            # a rune literal: copy it whole so a quote inside it does not open a string
            j = src.find("'", i + 2 if src[i + 1:i + 2] == "\\" else i + 1)
            if j != -1 and j - i <= 6:
                out.append(src[i:j + 1])
                i = j + 1
                continue
        out.append(c)
        i += 1
    return "".join(out)


def call_args(masked, start):
    """The text between the parentheses of the call whose name begins at `start`, or None if unbalanced."""
    k = masked.index("(", start)
    depth = 0
    for m in range(k, len(masked)):
        ch = masked[m]
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
            if depth == 0:
                return masked[k + 1:m]
    return None


ALLOW = "error-text-ok:"


def violations_in(text):
    """1-indexed line numbers of every 500 answer whose message carries an error's text, less those whose
    line, or the line above it, carries `// error-text-ok: <reason>` (an error that is OUR OWN sentence
    written for the caller, not a store's; the reason is required, a bare marker does not count).

    A message carries an error's text when its arguments (string contents masked) contain
      - a `.Error()` call on anything, whole-message or concatenated;
      - a formatter (fmt.Sprintf and friends) given an identifier that is an error by its name; or
      - a bare identifier that was assigned `x.Error()` earlier in the same function (`msg := err.Error()`
        then `writeError(w, 500, msg)`), which no call-shape rule can see.
    """
    masked = mask_strings(strip_comments(text))
    raw = text.split("\n")
    funcs = [m.start() for m in FUNC.finditer(masked)] + [len(masked)]

    def aliases_before(pos):
        for a, b in zip(funcs, funcs[1:]):
            if a <= pos < b:
                return {m.group(1) for m in ALIAS.finditer(masked[a:pos])}
        return set()

    lines = []
    for m in CALL.finditer(masked):
        args = call_args(masked, m.start())
        if args is None:
            continue
        bad = bool(ERR_TEXT.search(args))
        if not bad and FORMATTER.search(args) and ERR_IDENT.search(args):
            bad = True
        if not bad:
            names = aliases_before(m.start())
            bad = any(re.search(r"\b" + re.escape(a) + r"\b", args) for a in names)
        if not bad:
            continue
        line = masked.count("\n", 0, m.start()) + 1
        nearby = raw[max(0, line - 2):line]
        if any(re.search(re.escape(ALLOW) + r"\s*\S", l) for l in nearby):
            continue
        lines.append(line)
    return sorted(set(lines))


def in_scope(path):
    return path.startswith("cmd/cleat-worker/") and path.endswith(".go") and not path.endswith("_test.go")


def flagged(path, text):
    """The decision main() makes for one file; the self-test drives this, so a
    bug in in_scope is exercised too."""
    if not in_scope(path):
        return []
    return violations_in(text)


def run_self_test():
    f = "cmd/cleat-worker/server.go"
    positives = {
        "the literal shape": "func h() {\n\ts.writeError(w, 500, err.Error())\n}\n",
        "http.StatusInternalServerError": "func h() {\n\ts.writeError(w, http.StatusInternalServerError, err.Error())\n}\n",
        "an identifier that is not `err`": "func h() {\n\ts.writeError(w, 500, vErr.Error())\n}\n",
        "a marker with no reason": "func h() {\n\t// error-text-ok:\n\ts.writeError(w, 500, err.Error())\n}\n",
        "a marker two lines above": "func h() {\n\t// error-text-ok: stale\n\tx := 1\n\ts.writeError(w, 500, err.Error())\n}\n",
        "split across lines": "func h() {\n\ts.writeError(w,\n\t\thttp.StatusInternalServerError,\n\t\terr.Error(),\n\t)\n}\n",
        "writeJSON with an error map": "func h() {\n\ts.writeJSON(w, http.StatusInternalServerError, map[string]string{\"error\": err.Error()})\n}\n",
        "writeJSON with map[string]any": "func h() {\n\ts.writeJSON(w, 500, map[string]any{\"error\": serr.Error()})\n}\n",
        "an error concatenated after a sentence": "func h() {\n\ts.writeError(w, http.StatusInternalServerError, \"checking opt-in: \"+err.Error())\n}\n",
        "the same in a writeJSON map": "func h() {\n\ts.writeJSON(w, http.StatusInternalServerError, map[string]string{\"error\": \"failed to deploy: \" + aerr.Error()})\n}\n",
        "an error formatted with Sprintf, statement split across lines": "func h() {\n\ts.writeError(w, 500, fmt.Sprintf(\n\t\t\"entry point %q: %v\",\n\t\tname, schemaErr))\n}\n",
        "an error formatted with %w through Errorf": "func h() {\n\ts.writeError(w, 500, fmt.Errorf(\"x: %w\", err).Error())\n}\n",
        "an alias of err.Error() used as the message": "func h() {\n\tmsg := err.Error()\n\tswitch {\n\tdefault:\n\t\ts.writeError(w, 500, msg)\n\t}\n}\n",
        "an alias in a map": "func h() {\n\tmsg := e.Error()\n\ts.writeJSON(w, 500, map[string]string{\"error\": msg})\n}\n",
    }
    for name, src in positives.items():
        if not flagged(f, src):
            print(f"SELF-TEST FAILED: the guard missed a known-positive: {name}")
            return False

    negatives = {
        "the helper": "func h() {\n\ts.writeStoreError(w, r, err)\n}\n",
        "a 4xx carrying a domain error's text": "func h() {\n\ts.writeError(w, 400, err.Error())\n}\n",
        "a 409": "func h() {\n\ts.writeError(w, http.StatusConflict, err.Error())\n}\n",
        "a sentence of the handler's own": "func h() {\n\ts.writeError(w, 500, \"schedule \"+op+\" failed\")\n}\n",
        "a line comment quoting the old shape": "// it was `s.writeError(w, 500, err.Error())`, which sent the driver's text\nfunc h() {}\n",
        "a block comment quoting it": "/* s.writeError(w, 500, err.Error()) */\nfunc h() {}\n",
        "a Sprintf of values that are not errors": "func h() {\n\ts.writeError(w, 500, fmt.Sprintf(\"entry point %q of %s v%d\", ep, name, v))\n}\n",
        "a sentence that MENTIONS err.Error() and %v": "func h() {\n\ts.writeError(w, 500, \"never send err.Error() or %v err\")\n}\n",
        "errors.New is not an error value in the message": "func h() {\n\ts.writeError(w, 500, fmt.Sprintf(\"%v\", errors.ErrUnsupported.Error()[:0]+name))\n}\n".replace('errors.ErrUnsupported.Error()[:0]+',''),
        "an alias assigned in ANOTHER function": "func a() {\n\tmsg := err.Error()\n\t_ = msg\n}\nfunc h() {\n\ts.writeError(w, 500, msg)\n}\n",
        "a 4xx that concatenates an error": "func h() {\n\ts.writeError(w, 400, \"bad input: \"+err.Error())\n}\n",
        "an allowed line, marker above, with a reason": "func h() {\n\t// error-text-ok: our own configuration error\n\ts.writeError(w, 500, serr.Error())\n}\n",
        "an allowed line, marker beside, with a reason": "func h() {\n\ts.writeError(w, 500, serr.Error()) // error-text-ok: our own sentence\n}\n",
    }
    for name, src in negatives.items():
        if flagged(f, src):
            print(f"SELF-TEST FAILED: the guard flagged something it must not: {name}")
            return False

    bad = positives["the literal shape"]
    for path in ("cmd/cleat-worker/server_test.go", "engine/db.go", "cmd/cleatctl/x.go"):
        if flagged(path, bad):
            print(f"SELF-TEST FAILED: {path} is out of scope and was flagged")
            return False

    print(f"self-test: OK ({len(positives)} positives, {len(negatives)} negatives, 3 out-of-scope paths)")
    return True


def main():
    if "--self-test" in sys.argv:
        sys.exit(0 if run_self_test() else 1)
    try:
        out = subprocess.run(["git", "ls-files", "cmd/cleat-worker/*.go"], capture_output=True, text=True, check=True)
    except Exception as e:
        print(f"UNMEASURED: could not list tracked files: {e}")
        print("This is a failure of the check, not a finding about the tree.")
        sys.exit(2)
    files = [p for p in out.stdout.splitlines() if in_scope(p)]
    if not files:
        print("UNMEASURED: git ls-files returned no cmd/cleat-worker/*.go files -- not run from inside the repository?")
        print("This is a failure of the check, not a finding about the tree.")
        sys.exit(2)
    problems = []
    for path in files:
        try:
            text = open(path, encoding="utf-8").read()
        except Exception as e:
            print(f"UNMEASURED: could not read {path}: {e}")
            sys.exit(2)
        for line in violations_in(text):
            problems.append((path, line))
    if problems:
        print("A 500 answer carries the text of an error value. A driver's message can name hosts, statements and "
              "roles, and a client cannot act on it. Use s.writeStoreError(w, r, err), or log the error and write "
              "a sentence of your own (cleat#3878):\n")
        for path, line in problems:
            print(f"  {path}:{line}")
        sys.exit(1)
    print(f"OK: no 500 answer in {len(files)} cmd/cleat-worker files is the text of an error value.")
    sys.exit(0)


if __name__ == "__main__":
    main()
