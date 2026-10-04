#!/usr/bin/env python3
"""Refuse docs/playbooks/README.md when it documents a signature or a build
claim the tree no longer backs.

cleat#2513: nothing re-checked the playbooks' factual claims against the tree,
and cleat#2511 is what that cost -- the "four hitch points" table's routes row
still said `RegisterRoutes(*http.ServeMux)` eleven days after cleat#2232
changed it to `plugin.Router`, because the four-playbook correction pass
(#2050-#2053) corrected each playbook's own body and never touched the shared
README they all point back to. A reader who trusts that table writes a
signature that will not compile.

TWO CLAIMS, BOTH MECHANICAL. #2511's own fix direction names them:

  1. THE FOUR HITCH POINTS TABLE documents a method call for each of cleat's
     four plugin extension points. Each one names a real Go interface
     (HasHostFunctions, HasRoutes, HasMiddleware, HasBackground in
     plugin/plugin.go and plugin/plugin_http.go) -- extract both sides and
     compare, rather than trusting either.

  2. THE BUILT? TABLE says which playbooks run end to end in CI, and names
     the example directory and CI job for each "yes". Both are checkable:
     the directory either exists and the job either has a `name:` line in
     ci.yml, or the claim is stale.

WHAT THIS DOES NOT CHECK: the "shipped"/"unbuilt" prose scattered through the
four playbook bodies themselves. That prose does not use one consistent
marker the way docs/full-stack-readiness.md's gap table does -- of the loose
"shipped (cleat#N)" phrasing in those four files, only two instances match a
tight, safely-parseable shape, and a looser pattern risks the same false
positive check-readiness-doc.py's self-test caught on its first run (an
incidental word read as a status). The two tables above are what a mechanical
check can do safely; the prose is not filed here as done, it is left to a
future issue rather than to this one closing.

UPDATE (cleat#2603, that future issue): the prose is now checked, but NOT by
loosening the pattern above -- which that note was right to refuse. The four
bodies' `## What you still have to build or buy` sections are a STRUCTURAL
surface (every playbook has one, and it is a list of live claims), so the check
is scoped to those sections and requires each `cleat#N` citation in them to
carry an explicit `**shipped**` / `**unbuilt**` / `**declined**` mark, verified
against the tracker. Everything the paragraph above worried about -- "was shipped
and unmentioned when this table was written", "**nothing shipped**", an order
"shipped and uncharged" -- lives OUTSIDE those sections and stays unguarded on
purpose. See check_body_claims().
"""

import json
import re
import subprocess
import sys
from pathlib import Path

README = "docs/playbooks/README.md"
CI_WORKFLOW = ".github/workflows/ci.yml"

# name -> (file that defines the interface, the interface's own name)
IFACES = {
    "RegisterHostFunctions": ("plugin/plugin.go", "HasHostFunctions"),
    "RegisterRoutes": ("plugin/plugin_http.go", "HasRoutes"),
    "Middleware": ("plugin/plugin_http.go", "HasMiddleware"),
    "Run": ("plugin/plugin.go", "HasBackground"),
}

CALL_RE = re.compile(r"`(\w+)\(([^()]*)\)\s*([^`]*)`")
IFACE_BLOCK_RE_TMPL = r"type\s+{iface}\s+interface\s*\{{(.*?)\n\}}"


def extract_section(text, start_marker, end_marker_re):
    i = text.find(start_marker)
    if i == -1:
        return None
    rest = text[i + len(start_marker):]
    m = re.search(end_marker_re, rest)
    return rest[: m.start()] if m else rest


def parse_go_params(params_str):
    """'scope FuncRegistry, next http.Handler' -> [(name, type), ...]."""
    if not params_str.strip():
        return []
    out = []
    for p in params_str.split(","):
        toks = p.strip().split()
        if len(toks) >= 2:
            out.append((toks[0], toks[-1]))
        else:
            out.append(("", toks[0] if toks else ""))
    return out


def extract_iface_method(src_text, method, iface, file_label):
    """Find `type <iface> interface { ... <method>(...) ... }` and return its
    (params, return) shape, independent of any implementation elsewhere in the
    file -- an implementation is `func (p *Plugin) Name(...)`, which this never
    matches, so a plugin's own method cannot be mistaken for the interface it
    satisfies."""
    m = re.search(IFACE_BLOCK_RE_TMPL.format(iface=re.escape(iface)), src_text, re.S)
    if not m:
        return None, f"no `type {iface} interface {{...}}` found in {file_label}"
    block = m.group(1)
    for raw in block.splitlines():
        line = raw.strip()
        if not line or line.startswith("//"):
            continue
        mm = re.match(r"^" + re.escape(method) + r"\((.*?)\)\s*(.*)$", line)
        if mm:
            params_str, ret = mm.groups()
            return (parse_go_params(params_str), ret.strip()), None
    return None, f"`type {iface} interface` in {file_label} has no `{method}(...)` member"


def last_segment(token):
    return token.split(".")[-1]


def doc_param_matches(doc_token, real_name, real_type):
    doc_token = doc_token.strip()
    if not doc_token:
        return False
    toks = doc_token.split()
    if len(toks) >= 2:
        return toks[0] == real_name and last_segment(toks[-1]) == last_segment(real_type)
    tok = toks[0]
    # Shorthand is accepted two ways: the documented token names the real
    # parameter (Run(ctx) for `ctx context.Context`) or names its type
    # (RegisterHostFunctions(FuncRegistry) for `scope FuncRegistry`).
    return tok == real_name or last_segment(tok) == last_segment(real_type)


def doc_ret_matches(doc_ret, real_ret):
    if not doc_ret.strip():
        return True  # omitting the return type is the table's own convention
    return last_segment(doc_ret.strip()) == last_segment(real_ret.strip())


def compare_signature(name, doc_params_str, doc_ret, real_params, real_ret):
    problems = []
    doc_params = [p.strip() for p in doc_params_str.split(",")] if doc_params_str.strip() else []
    if len(doc_params) != len(real_params):
        return [
            f"{name}: documented with {len(doc_params)} parameter(s) "
            f"({doc_params_str.strip() or '<none>'}), the real interface has "
            f"{len(real_params)} ({', '.join(f'{n} {t}' for n, t in real_params) or '<none>'})"
        ]
    for i, (dp, (rn, rt)) in enumerate(zip(doc_params, real_params)):
        if not doc_param_matches(dp, rn, rt):
            problems.append(
                f"{name}: parameter {i + 1} documented as `{dp}`, real signature has `{rn} {rt}`"
            )
    if not doc_ret_matches(doc_ret, real_ret):
        problems.append(f"{name}: documented return `{doc_ret}`, real return `{real_ret}`")
    return problems


def check_interfaces(readme_text, iface_sources):
    """iface_sources: {file_label: source_text}. Returns a list of complaints."""
    section = extract_section(readme_text, "## The four hitch points", r"\n## ")
    if section is None:
        return [f"{README}: could not find the '## The four hitch points' section"]

    doc_calls = {}
    for line in section.splitlines():
        if not line.lstrip().startswith("|"):
            continue
        for m in CALL_RE.finditer(line):
            name, params_str, ret = m.groups()
            if name in IFACES:
                doc_calls[name] = (params_str, ret)

    if not doc_calls:
        return [
            f"{README}: the four hitch points table has no recognizable "
            f"`Name(params) [ret]` interface calls -- either the table is gone "
            f"or the row format changed and CALL_RE no longer matches it"
        ]

    problems = []
    for name, (file_label, iface) in IFACES.items():
        if name not in doc_calls:
            problems.append(f"{name}: not documented in the four hitch points table")
            continue
        params_str, ret = doc_calls[name]
        src = iface_sources.get(file_label)
        if src is None:
            problems.append(f"{name}: {file_label} does not exist")
            continue
        real, err = extract_iface_method(src, name, iface, file_label)
        if err:
            problems.append(f"{name}: {err}")
            continue
        real_params, real_ret = real
        problems.extend(compare_signature(name, params_str, ret, real_params, real_ret))
    return problems


BUILT_LINK_RE = re.compile(r"\[`([^`]+)`\]\(([^)]+)\)")
BACKTICK_RE = re.compile(r"`([^`]+)`")


def check_built_table(readme_text, path_exists, ci_workflow_text):
    section = extract_section(readme_text, "| Playbook | Built? | Where |", r"\n\n")
    if section is None:
        return [f"{README}: could not find the 'Built?' table"]

    rows = [
        line for line in section.splitlines()
        if line.lstrip().startswith("|") and "---" not in line
    ]
    if not rows:
        return [f"{README}: the Built? table has no data rows"]

    problems = []
    for line in rows:
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if len(cells) != 3:
            continue
        label, built, where = cells
        m = re.search(r"\*\*(yes|no)\*\*", built, re.IGNORECASE)
        if not m:
            problems.append(f"Built? table row {label!r}: 'Built?' cell is not **yes** or **no**: {built!r}")
            continue
        if m.group(1).lower() != "yes":
            continue  # nothing is claimed for a "no" row

        link_m = BUILT_LINK_RE.search(where)
        if not link_m:
            problems.append(f"Built? table row {label!r}: marked **yes** but names no example directory")
            continue
        visible_path, href = link_m.groups()
        if not path_exists(href):
            problems.append(
                f"Built? table row {label!r}: marked **yes** and links to `{href}`, "
                f"which does not exist"
            )

        job_names = [t for t in BACKTICK_RE.findall(where) if t != visible_path]
        if not job_names:
            problems.append(
                f"Built? table row {label!r}: marked **yes** but names no CI job to "
                f"verify it against"
            )
            continue
        for job in job_names:
            if f"name: {job}" not in ci_workflow_text and f"name: {job} " not in ci_workflow_text:
                problems.append(
                    f"Built? table row {label!r}: names CI job `{job}`, which has no "
                    f"`name:` line in {CI_WORKFLOW}"
                )
    return problems


def real_path_exists(repo_root, href):
    # The link is authored relative to docs/playbooks/, where README.md lives.
    return (repo_root / "docs" / "playbooks" / href).resolve().exists()


# ---------------------------------------------------------------------------
# cleat#2603: the four bodies' OWN "shipped"/"unbuilt" prose.
#
# Everything above reads docs/playbooks/README.md. This reads the four bodies,
# and it is bounded deliberately -- this file's header already deferred it, in
# those words, to "a future issue rather than to this one closing". That issue
# is #2603.
#
# WHY A SECTION AND A MARKER, NOT THE WORD. "shipped" appears in these bodies as
# history ("was shipped and unmentioned when this table was written"), as
# load-bearing NEGATION ("**nothing shipped**"), and in an unrelated sense
# entirely (an order "shipped and uncharged"). A pattern keyed on the word flags
# all three, which is the false positive check-readiness-doc.py's own self-test
# caught on its first run with an incidental "Long done". So the surface is the
# one section that is STRUCTURAL -- every playbook has it and it is a list of
# live claims -- and within it a citation must carry the same `**shipped**` mark
# docs/full-stack-readiness.md uses.
BODY_SECTION = "## What you still have to build or buy"
BODIES = [
    "docs/playbooks/ai-agent-platform.md",
    "docs/playbooks/b2b-saas-control-plane.md",
    "docs/playbooks/integration-hub.md",
    "docs/playbooks/order-lifecycle.md",
]
CLEAT_ISSUE = re.compile(r"cleat#(\d+)")
BODY_SHIPPED = re.compile(r"\*\*\s*shipped\s*\*\*", re.IGNORECASE)
BODY_UNBUILT = re.compile(r"\*\*\s*(unbuilt|not shipped|still to build)\s*\*\*", re.IGNORECASE)
BODY_DECLINED = re.compile(r"\*\*\s*(declined|not planned|out of scope)\s*\*\*", re.IGNORECASE)

# What each mark ASSERTS, so the comparison is one line rather than a branch per
# pair of (mark, state). check-readiness-doc.py spells its three branches out
# because a row's absence of a mark is itself meaningful there; here the issue
# asks every citation to carry one, so the mark is always present and the
# question is only whether it is TRUE.
MARK_ASSERTS = {"shipped": "shipped", "unbuilt": "unbuilt", "declined": "declined"}


def section_text(text, heading):
    """(body, first_line) for the section at `heading`, up to the next `## `.

    `first_line` is the 1-based FILE line number of the body's first line. The
    caller needs it because `claim_blocks` counts from the section's own start: a
    bare offset reported as `path:N` looks perfectly specific and points a reader
    at the wrong line, which is worse than saying nothing.
    """
    lines = text.split("\n")
    start = None
    for i, line in enumerate(lines):
        if line.strip() == heading:
            start = i + 1
            break
    if start is None:
        return None, None
    out = []
    for line in lines[start:]:
        if line.startswith("## "):
            break
        out.append(line)
    return "\n".join(out), start + 1


def claim_blocks(section):
    """Yield (line_number, block_text) for each list item or paragraph.

    A claim is an ITEM or a PARAGRAPH, not a line. The sentences wrap, and one
    item routinely cites two issues -- ai-agent-platform's token-streaming entry
    names cleat#1572 and cleat#1639 on two different lines of the same item -- so
    a per-line rule would demand a marker on each line of one claim.
    """
    blocks, cur, cur_start = [], [], None
    for i, line in enumerate(section.split("\n"), start=1):
        if not line.strip():
            if cur:
                blocks.append((cur_start, "\n".join(cur)))
                cur, cur_start = [], None
            continue
        if re.match(r"^\s*(\d+\.|[-*])\s", line):
            if cur:
                blocks.append((cur_start, "\n".join(cur)))
            cur, cur_start = [line], i
        else:
            if cur_start is None:
                cur_start = i
            cur.append(line)
    if cur:
        blocks.append((cur_start, "\n".join(cur)))
    return blocks


def check_body_claims(path, text, state_of):
    """(complaints, citations_seen) for one playbook's live claims."""
    problems = []
    section, base = section_text(text, BODY_SECTION)
    if section is None:
        problems.append(
            f"{path}: no '{BODY_SECTION}' section. Either the playbook was restructured "
            f"or BODY_SECTION no longer names it."
        )
        return problems, 0

    seen = 0
    for block_line, block in claim_blocks(section):
        lineno = base + block_line - 1  # report the FILE line, not a section offset
        issues = CLEAT_ISSUE.findall(block)
        if not issues:
            continue
        seen += len(issues)
        cited = ", ".join("cleat#" + i for i in issues)
        marks = [
            m for m, pat in (("shipped", BODY_SHIPPED), ("unbuilt", BODY_UNBUILT),
                             ("declined", BODY_DECLINED))
            if pat.search(block)
        ]
        if not marks:
            problems.append(
                f"{path}:{lineno}: cites {cited} but carries no explicit marker.\n"
                f"    This section lists live claims, so a citation here asserts a state. "
                f"Mark it **shipped**, **unbuilt** or **declined** so the assertion is "
                f"checkable rather than inferred from the sentence around it.\n"
                f"    {block.splitlines()[0].strip()}"
            )
            continue
        if len(marks) > 1:
            problems.append(
                f"{path}:{lineno}: cites {cited} and marks it {' and '.join(marks)} -- two "
                f"claims about one state."
            )
            continue
        mark = marks[0]
        for issue in issues:
            state, reason = state_of(issue)
            if state != "CLOSED":
                actual = "unbuilt"
            elif reason == "NOT_PLANNED":
                actual = "declined"
            else:
                actual = "shipped"
            if MARK_ASSERTS[mark] != actual:
                problems.append(
                    f"{path}:{lineno}: cleat#{issue} is {state}"
                    f"{'/' + reason if reason else ''} -- {actual} -- but this claim marks "
                    f"it **{mark}**.\n"
                    f"    A reader deciding what to build is being told the opposite of what "
                    f"the tracker says.\n"
                    f"    {block.splitlines()[0].strip()}"
                )
    return problems, seen


def check_all_bodies(paths_texts, state_of):
    """Complaints across every playbook body.

    The "matched nothing" guard is CROSS-FILE on purpose: two of the four
    sections carry no cleat# citations at all, so a per-file emptiness check
    would fire on correct files -- see check-readiness-doc.py's own note that a
    scan which measures nothing reads identically to success, applied one level
    up rather than one level down.
    """
    problems, total = [], 0
    for path, text in paths_texts:
        found, seen = check_body_claims(path, text, state_of)
        problems += found
        total += seen
    if total == 0:
        problems.append(
            f"no cleat#N citations were found in any playbook's '{BODY_SECTION}'. "
            f"That section is a list of live claims; matching none of them means this "
            f"check measured nothing, not that the playbooks are clean."
        )
    return problems


# --------------------------------------------------------------------------


SELF_TEST_README = """
## The four hitch points

| Extension point | Interface | What it gets you |
|---|---|---|
| **Host functions** | `RegisterHostFunctions(FuncRegistry)` | Callable from a workflow. |
| **HTTP routes** | `RegisterRoutes(plugin.Router)` | A control surface. |
| **Edge middleware** | `Middleware(http.Handler) http.Handler` | Wraps every request. |
| **Background loop** | `Run(ctx) error` | Sweeps, reconciliation, polling. |

## Next section

nothing here

| Playbook | Built? | Where |
|---|---|---|
| 1 -- AI agent platform | **yes** | [`examples/ai-agent-platform/`](../../examples/ai-agent-platform/), with the `AI agent platform scenario` CI job |
| 2 -- B2B SaaS control plane | **no** | a design grounded in code that exists |

"""

SELF_TEST_PLUGIN_GO = """
package plugin

type HasHostFunctions interface {
\tPlugin
\tRegisterHostFunctions(scope FuncRegistry) error
}

type HasBackground interface {
\tPlugin
\tRun(ctx context.Context) error
}
"""

SELF_TEST_PLUGIN_HTTP_GO = """
package plugin

type HasRoutes interface {
\tPlugin
\tRegisterRoutes(mux Router) error
}

type HasMiddleware interface {
\tPlugin
\tMiddleware(next http.Handler) http.Handler
}
"""

SELF_TEST_SOURCES = {
    "plugin/plugin.go": SELF_TEST_PLUGIN_GO,
    "plugin/plugin_http.go": SELF_TEST_PLUGIN_HTTP_GO,
}


def self_test():
    failures = []

    # Known positive: matched fixtures agree on all four, and the Built table
    # is internally consistent.
    problems = check_interfaces(SELF_TEST_README, SELF_TEST_SOURCES)
    if problems:
        failures.append(f"  FALSE POSITIVE on a matched fixture: {problems}")

    built_problems = check_built_table(
        SELF_TEST_README,
        path_exists=lambda href: href == "../../examples/ai-agent-platform/",
        ci_workflow_text="  name: AI agent platform scenario\n",
    )
    if built_problems:
        failures.append(f"  FALSE POSITIVE on a matched Built? fixture: {built_problems}")

    # Known negative #1 -- the actual cleat#2511 bug: the routes row reverted
    # to the pre-#2232 signature.
    stale_routes = SELF_TEST_README.replace(
        "`RegisterRoutes(plugin.Router)`", "`RegisterRoutes(*http.ServeMux)`"
    )
    problems = check_interfaces(stale_routes, SELF_TEST_SOURCES)
    joined = "\n".join(problems)
    if "RegisterRoutes" not in joined:
        failures.append("  MISSED: cleat#2511's own bug (stale RegisterRoutes row) was not reported")

    # And the other three rows must stay silent -- a guard that reports every
    # row once one is wrong is as useless as one that reports none.
    for quiet in ("RegisterHostFunctions", "Middleware", "Run"):
        if quiet in joined:
            failures.append(f"  FALSE POSITIVE: {quiet} was correct but reported anyway")

    # Known negative #2 -- source-side drift: the interface's own parameter is
    # renamed, so `Run(ctx)`'s name-shorthand match stops working. This is the
    # direction #2511 did not test: the doc staying still while the code moves.
    renamed_param_go = SELF_TEST_PLUGIN_GO.replace(
        "Run(ctx context.Context) error", "Run(c context.Context) error"
    )
    problems = check_interfaces(
        SELF_TEST_README, {**SELF_TEST_SOURCES, "plugin/plugin.go": renamed_param_go}
    )
    joined = "\n".join(problems)
    if "Run:" not in joined:
        failures.append("  MISSED: a renamed interface parameter (ctx -> c) was not reported")

    # Known negative #3 -- a missing interface block entirely (the file lost
    # the type, or the check is pointed at the wrong name).
    problems = check_interfaces(SELF_TEST_README, {**SELF_TEST_SOURCES, "plugin/plugin.go": "package plugin\n"})
    joined = "\n".join(problems)
    if "HasHostFunctions" not in joined and "HasBackground" not in joined:
        failures.append("  MISSED: a source file missing the interface block entirely was not reported")

    # Known negative #4 -- the measures-nothing case: a README with no
    # recognizable table at all must not read as a clean pass.
    problems = check_interfaces("# nothing here\n", SELF_TEST_SOURCES)
    if not problems:
        failures.append("  MISSED: a document with no four-hitch-points table read as clean")

    problems = check_built_table("# nothing here\n", lambda href: True, "")
    if not problems:
        failures.append("  MISSED: a document with no Built? table read as clean")

    # Known negative #5 -- Built? table claims a directory that is not there.
    problems = check_built_table(
        SELF_TEST_README,
        path_exists=lambda href: False,
        ci_workflow_text="  name: AI agent platform scenario\n",
    )
    joined = "\n".join(problems)
    if "does not exist" not in joined:
        failures.append("  MISSED: a Built? row linking a missing example directory was not reported")

    # Known negative #6 -- Built? table names a CI job with no name: line.
    problems = check_built_table(
        SELF_TEST_README,
        path_exists=lambda href: True,
        ci_workflow_text="  name: some other job\n",
    )
    joined = "\n".join(problems)
    if "AI agent platform scenario" not in joined:
        failures.append("  MISSED: a Built? row naming a nonexistent CI job was not reported")

    # And the "no" row must never be checked -- it makes no claim.
    problems = check_built_table(
        SELF_TEST_README, path_exists=lambda href: False, ci_workflow_text=""
    )
    if any("B2B" in p for p in problems):
        failures.append("  FALSE POSITIVE: a 'no' row was checked as though it claimed a directory/job")

    # cleat#2603: the four bodies' own live claims. state_of is INJECTED, so the
    # self-test needs no network -- the same shape check-readiness-doc.py uses,
    # and the reason its own self-test can assert on text and status together.
    def fake_state(issue):
        return {
            "900": ("CLOSED", "COMPLETED"),
            "901": ("OPEN", ""),
            "902": ("CLOSED", "NOT_PLANNED"),
        }.get(issue, ("OPEN", ""))

    matched_bodies = [("docs/playbooks/x.md", f"""
{BODY_SECTION}

1. **Streaming -- it shipped.** cleat#900, **shipped**.
2. **An eval harness.** cleat#901, **unbuilt**.
3. **SCIM.** cleat#902, **declined**.
""")]
    problems = check_all_bodies(matched_bodies, fake_state)
    if problems:
        failures.append(f"  FALSE POSITIVE on matched body claims: {problems}")

    # Known positive, and it is the drift cleat#2603 names: a claim that was true
    # when written and was never revisited after the issue shipped.
    drifted = [("docs/playbooks/x.md", f"{BODY_SECTION}\n\n1. **Streaming.** cleat#900, **unbuilt**.\n")]
    problems = check_all_bodies(drifted, fake_state)
    if not problems or "cleat#900" not in problems[0]:
        failures.append(f"  MISSED the shipped/unbuilt drift: {problems}")

    # The word is not the mark. "shipped" in prose earns nothing; the marker does.
    unmarked = [("docs/playbooks/x.md", f"{BODY_SECTION}\n\n1. **Streaming.** cleat#900 shipped.\n")]
    problems = check_all_bodies(unmarked, fake_state)
    if not problems or "no explicit marker" not in problems[0]:
        failures.append(f"  MISSED the unmarked citation: {problems}")

    # A marker asserting the opposite of the tracker, in the other direction.
    overclaimed = [("docs/playbooks/x.md", f"{BODY_SECTION}\n\n1. **SCIM.** cleat#902, **shipped**.\n")]
    problems = check_all_bodies(overclaimed, fake_state)
    if not problems or "cleat#902" not in problems[0]:
        failures.append(f"  MISSED a declined issue marked shipped: {problems}")

    # The cross-file emptiness guard: two of the four real sections cite nothing,
    # so this is deliberately NOT a per-file check.
    nothing = [("docs/playbooks/x.md", f"{BODY_SECTION}\n\n1. Nothing cited here.\n")]
    problems = check_all_bodies(nothing, fake_state)
    if not any("measured nothing" in p for p in problems):
        failures.append(f"  MISSED the measured-nothing case: {problems}")

    if failures:
        print("self-test FAILED:\n" + "\n".join(failures), file=sys.stderr)
        return 1
    print("self-test passed")
    return 0


def github_state(issue):
    """(STATE, STATEREASON) for a cited issue, in the shape check_all_bodies wants.

    The same call check-readiness-doc.py makes. Its CI step carries GH_TOKEN and
    so must this one's now -- a `gh` that cannot reach the API raises, and the
    caller below turns that into UNMEASURED rather than into a pass.
    """
    out = subprocess.run(
        ["gh", "issue", "view", str(issue), "--json", "state,stateReason"],
        capture_output=True, text=True, check=True,
    )
    d = json.loads(out.stdout)
    return d["state"].upper(), (d.get("stateReason") or "").upper()


def main():
    if "--self-test" in sys.argv:
        return self_test()

    repo_root = Path(".")
    readme_path = repo_root / README
    if not readme_path.exists():
        print(f"UNMEASURED: {README} does not exist", file=sys.stderr)
        return 2
    readme_text = readme_path.read_text(encoding="utf-8")

    iface_sources = {}
    for file_label, _ in IFACES.values():
        p = repo_root / file_label
        if p.exists():
            iface_sources[file_label] = p.read_text(encoding="utf-8")

    ci_path = repo_root / CI_WORKFLOW
    if not ci_path.exists():
        print(f"UNMEASURED: {CI_WORKFLOW} does not exist", file=sys.stderr)
        return 2
    ci_text = ci_path.read_text(encoding="utf-8")

    problems = check_interfaces(readme_text, iface_sources)
    problems += check_built_table(
        readme_text,
        path_exists=lambda href: real_path_exists(repo_root, href),
        ci_workflow_text=ci_text,
    )

    body_texts = []
    for path in BODIES:
        p = repo_root / path
        if not p.exists():
            print(f"UNMEASURED: {path} does not exist", file=sys.stderr)
            return 2
        body_texts.append((path, p.read_text(encoding="utf-8")))
    try:
        problems += check_all_bodies(body_texts, github_state)
    except subprocess.CalledProcessError as exc:
        # stderr carries the discriminator -- a transport failure and a citation to
        # an issue that does not exist both exit 1, and CalledProcessError does not
        # keep the message on its own.
        detail = (exc.stderr or "").strip() or str(exc)
        print(
            f"UNMEASURED: `gh` could not be asked for an issue's state ({detail}), so the "
            f"playbook bodies' claims were NOT checked. This is a failure of the check, "
            f"not a finding about the documents.",
            file=sys.stderr,
        )
        return 2

    if problems:
        print("\n".join(problems), file=sys.stderr)
        print(
            f"\n{README} is the entry point every playbook points back to. A stale "
            f"signature here is one a reader copies into code that will not compile "
            f"(cleat#2511); a stale Built? row is one a reader trusts as CI-verified "
            f"when it is not. A playbook body's own 'still to build' list is where a "
            f"reader decides what work is left, so a citation there that disagrees with "
            f"the tracker -- or that carries no checkable mark at all -- costs them work "
            f"they did not have to do, or hides work they do (cleat#2603).",
            file=sys.stderr,
        )
        return 1

    print(
        f"OK: {README} agrees with the interfaces and CI it cites, and the four playbook "
        f"bodies' live 'still to build' claims agree with the tracker"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
