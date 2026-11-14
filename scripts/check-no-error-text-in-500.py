#!/usr/bin/env python3
"""check-no-error-text-in-500.py -- cleat#3878.

A cmd/cleat-worker handler that answered a failed store call with
`s.writeError(w, 500, err.Error())` sent the database driver's text to the
client: hostnames, statement text and role names, none of which the client can
act on, with a 500 that says the request is not worth sending again. cleat#3878
swept 38 such sites onto `apiServer.writeStoreError` (store_errors.go), which
keeps a domain error's own status and message and answers anything else with a
fixed body, a request id and the full error in the log.

RULE. In a non-test .go file under cmd/cleat-worker/, the whole message of a
500 answer must not be the text of an error value:

    writeError(w, 500, x.Error())
    writeError(w, http.StatusInternalServerError, x.Error())
    writeJSON(w, 500, map[string]string{"error": x.Error()})

for any identifier x (unless the line or the one above carries
`// error-text-ok: <reason>`, for an error that is our own sentence) -- the sweep found the same defect under `err`, `verr`,
`serr`, `vErr`, `pErr` and `defErr`, so a guard keyed on the name `err` would
have missed eleven of them. Use writeStoreError, or log the error and answer a
sentence of your own.

NOT COVERED, and said here so the green is not read as more than it is: a
message that CONCATENATES or formats an error into a longer string
("failed to deploy: " + err.Error()); a status other than 500; a 5xx written
some other way (http.Error); the streaming endpoints' `error` events; and
every package outside cmd/cleat-worker. A message built by hand from validation
text (a 4xx) is the handler's own prose and is not this defect.

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
ERR_TEXT = r"[A-Za-z_]\w*\.Error\(\)"
SHAPES = [
    re.compile(r"\bwriteError\(\s*w\s*,\s*" + STATUS + r"\s*,\s*" + ERR_TEXT + r"\s*,?\s*\)"),
    re.compile(
        r"\bwriteJSON\(\s*w\s*,\s*" + STATUS + r"\s*,\s*map\[string\](?:string|any)\{\s*\"error\"\s*:\s*"
        + ERR_TEXT + r"\s*,?\s*\}\s*,?\s*\)"
    ),
]


def in_scope(path):
    return path.startswith("cmd/cleat-worker/") and path.endswith(".go") and not path.endswith("_test.go")


ALLOW = "error-text-ok:"


def violations_in(text):
    """1-indexed line numbers where a forbidden shape starts, less those whose
    statement line, or the line above it, carries `// error-text-ok: <reason>`
    -- for an error that is OUR OWN sentence written for the caller (a
    configuration error naming the type that must be fixed), not a store's.
    The reason is required: a bare marker does not count."""
    stripped = strip_comments(text)
    raw = text.split("\n")
    lines = []
    for pat in SHAPES:
        for m in pat.finditer(stripped):
            line = stripped.count("\n", 0, m.start()) + 1
            nearby = raw[max(0, line - 2):line]
            if any(re.search(re.escape(ALLOW) + r"\s*\S", l) for l in nearby):
                continue
            lines.append(line)
    return sorted(lines)


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
