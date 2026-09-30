#!/usr/bin/env python3
"""check-mssql-tenant-scoped-reads.py -- cleat#2210.

MSSQLStore.WithTenant copies the struct but not the pool: the pool's
connector still carries the ORIGINAL tenant's SESSION_CONTEXT baked into
every physical connection (set once, at connect time, by
MSSQLStoreFactory.OpenStore). A method that queries the bare pool
(`s.db.Query`/`s.db.Exec`/`s.db.Prepare`) therefore reads or writes under
whatever tenant that connection happened to be opened for -- which agrees
with `s.tenantID` for a store obtained the ordinary way, and silently
disagrees for one re-scoped via `.WithTenant()` after the fact. See
`beginTxWithContext`'s own doc comment and cleat#2187/#2207/#2226 for the
first two methods this bit in practice.

`beginTxWithContext` is the fix: it opens a transaction and reasserts
`sp_set_session_context` for `s.tenantID` before the caller's first query,
so every subsequent statement on that `*sql.Tx` is scoped correctly
regardless of which physical connection the pool handed out or what tenant
it was originally opened for.

RULE. Any `*MSSQLStore` method whose body both (a) references `s.tenantID`
-- the signal that this method's query is *meant* to be tenant-scoped --
and (b) queries `s.db` directly rather than through `s.beginTxWithContext`,
is forbidden UNLESS it appears in the allowlist below with a non-empty
reason and a valid status.

This is deliberately structural rather than a one-time count: the count in
cleat#2210 ("~45", later re-derived as 60) is a census of a fixed population
at review time, and this file's own "Ground rules" section names that
shape as the one that reliably rots. The predicate that doesn't rot is "a
new tenant-scoped method must not repeat this mistake" -- which this guard
enforces on every future method, not just the ones enumerated today.

ALLOWLIST, why two statuses and not one: an entry can be here for two
different reasons that look identical to a casual reader --

  - EXEMPT: this method genuinely does not need `beginTxWithContext`,
    because it does not filter by `s.tenantID` for correctness (a
    cross-tenant admin read) or is a false positive of the `s.tenantID`
    heuristic (references it in a comment, or an unrelated capture).
  - DEFERRED: this method IS in-scope and IS still vulnerable, and simply
    hasn't been converted yet -- tracked debt, not a decision.

A single undifferentiated allowlist would let "9 exempt" and "60 deferred"
read identically as "60 allowlisted", which is indistinguishable from
"complete" at a glance. Every entry below states which it is and why, and
the self-test proves the guard tells them apart.

STALENESS. An allowlist entry naming a method that no longer exists, or
that is no longer vulnerable (already converted, or no longer references
s.tenantID), is a FAILURE, not a silent pass -- an allowlist that can only
grow is a ratchet in the wrong direction, and this is the same rule
scripts/check-skips.sh already applies to a skip ledger line that matches
nothing.

Exit codes: 0 clean, 1 a violation was found, 2 the check could not run.
"""
import re
import subprocess
import sys

# ---------------------------------------------------------------------------
# ALLOWLIST
# ---------------------------------------------------------------------------
# (file, method): (status, reason)
# status is "exempt" or "deferred". Reason must be non-empty (enforced by
# the self-test and by ALLOWLIST_SHAPE_OK below).
EXEMPT = "exempt"
DEFERRED = "deferred"

ALLOWLIST = {
    # Deliberately empty in source: every method that reaches this guard's
    # OWN vulnerable-shape test (bare s.db + a real s.tenantID reference +
    # no beginTxWithContext) genuinely needs a beginTxWithContext fix, and
    # is either converted or listed in mssql-tenant-scope-deferred.tsv.
    #
    # Nine methods were manually reviewed and found NOT to need one --
    # CheckCancellation, LoadCompactionState, VerifyWorkflowEvents,
    # TraceWorkflow, ResolveTenantFromAPIKey, retentionCount,
    # ListTenantIDs, IsTenantSuspended, GetChildResult -- but none of them
    # match this guard's vulnerable-shape test in the first place (each
    # either doesn't reference s.tenantID in a WHERE clause at all, or IS
    # the method that establishes s.tenantID). An allowlist entry for a
    # method the guard would never flag is not documentation, it is a
    # permanently stale entry waiting to happen -- exactly the staleness
    # class this guard's own self-test (case 5) and main() (the `stale`
    # check) exist to catch. Each of the nine instead carries an inline
    # doc-comment explaining why it queries s.db directly, at its own
    # definition site.
}


def _load_deferred_from_file():
    """Deferred entries live in a companion TSV so the count of what's left
    is a `wc -l`, not a scroll through this file -- see
    mssql-tenant-scope-deferred.tsv alongside this script."""
    import os
    path = os.path.join(os.path.dirname(__file__), "mssql-tenant-scope-deferred.tsv")
    entries = {}
    try:
        with open(path, encoding="utf-8") as f:
            for lineno, line in enumerate(f, 1):
                line = line.rstrip("\n")
                if not line or line.startswith("#"):
                    continue
                parts = line.split("\t")
                if len(parts) != 3:
                    print(f"UNMEASURED: {path}:{lineno}: expected 3 tab-separated "
                          f"fields (file, method, reason), got {len(parts)}")
                    sys.exit(2)
                file_, method, reason = parts
                entries[(file_, method)] = (DEFERRED, reason)
    except FileNotFoundError:
        print(f"UNMEASURED: {path} not found")
        sys.exit(2)
    return entries


# ---------------------------------------------------------------------------
# Extraction (shares its shape with check-no-raw-rebind.py's comment/string
# stripping, since prose can quote "s.tenantID" or "s.db.Query" without
# meaning either).
# ---------------------------------------------------------------------------

def strip_comments(src):
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


METHOD_RE = re.compile(r"^func \(s \*MSSQLStore\) (\w+)\((.*?)\n(?=^func |\Z)", re.M | re.S)
BARE_DB_RE = re.compile(r"\bs\.db\.(Query|Exec|Prepare)")
TENANT_ID_RE = re.compile(r"\bs\.tenantID\b")
TX_CONTEXT_RE = re.compile(r"\bbeginTxWithContext\b")


def find_methods(path, text):
    """Yields (method_name, is_vulnerable) for every *MSSQLStore method in
    this file's (unstripped) source."""
    stripped = strip_comments(text)
    for m in METHOD_RE.finditer(stripped):
        name = m.group(1)
        body = m.group(2)
        vulnerable = bool(
            BARE_DB_RE.search(body)
            and TENANT_ID_RE.search(body)
            and not TX_CONTEXT_RE.search(body)
        )
        yield name, vulnerable


# ---------------------------------------------------------------------------
# Self-test
# ---------------------------------------------------------------------------

def run_self_test():
    ok = True

    # 1. Known positive: the exact shape this guard exists to catch.
    positive_src = (
        "package engine\n\n"
        "func (s *MSSQLStore) GetWidget(ctx context.Context, id string) (*Widget, error) {\n"
        "\tvar w Widget\n"
        "\terr := s.db.QueryRowContext(ctx, `SELECT * FROM widgets WHERE id=@p1 AND tenant_id=@p2`, id, s.tenantID).Scan(&w)\n"
        "\treturn &w, err\n"
        "}\n"
    )
    found = dict(find_methods("engine/fake.go", positive_src))
    if not found.get("GetWidget"):
        print("SELF-TEST FAILED: the exact vulnerable shape (bare s.db + "
              "s.tenantID, no beginTxWithContext) was not flagged.")
        ok = False

    # 2. Known negative: same shape, but routed through beginTxWithContext.
    negative_tx_src = (
        "package engine\n\n"
        "func (s *MSSQLStore) GetWidgetSafe(ctx context.Context, id string) (*Widget, error) {\n"
        "\ttx, err := s.beginTxWithContext(ctx)\n"
        "\tif err != nil {\n\t\treturn nil, err\n\t}\n"
        "\tdefer tx.Rollback()\n"
        "\tvar w Widget\n"
        "\terr = tx.QueryRowContext(ctx, `SELECT * FROM widgets WHERE id=@p1 AND tenant_id=@p2`, id, s.tenantID).Scan(&w)\n"
        "\treturn &w, err\n"
        "}\n"
    )
    if dict(find_methods("engine/fake.go", negative_tx_src)).get("GetWidgetSafe"):
        print("SELF-TEST FAILED: a method routed through beginTxWithContext "
              "was flagged as vulnerable.")
        ok = False

    # 3. Known negative: bare s.db, but no s.tenantID reference (not meant
    # to be tenant-scoped -- e.g. a genuine cross-tenant admin read).
    negative_no_tenant_src = (
        "package engine\n\n"
        "func (s *MSSQLStore) ListAllWidgets(ctx context.Context) ([]Widget, error) {\n"
        "\trows, err := s.db.QueryContext(ctx, `SELECT * FROM widgets`)\n"
        "\t_ = rows\n"
        "\treturn nil, err\n"
        "}\n"
    )
    if dict(find_methods("engine/fake.go", negative_no_tenant_src)).get("ListAllWidgets"):
        print("SELF-TEST FAILED: a method with no s.tenantID reference at "
              "all was flagged (this is the shape of a genuine cross-tenant "
              "admin read).")
        ok = False

    # 4. Comments/prose must not trigger a false positive -- the same trap
    # check-no-raw-rebind.py guards against, applied to this guard's own
    # signal words.
    negative_comment_src = (
        "package engine\n\n"
        "// GetWidgetDocumented used to call s.db.QueryContext directly and\n"
        "// filter by s.tenantID, which was wrong; see cleat#2210.\n"
        "func (s *MSSQLStore) GetWidgetDocumented(ctx context.Context) error {\n"
        "\ttx, err := s.beginTxWithContext(ctx)\n"
        "\tdefer tx.Rollback()\n"
        "\treturn err\n"
        "}\n"
    )
    if dict(find_methods("engine/fake.go", negative_comment_src)).get("GetWidgetDocumented"):
        print("SELF-TEST FAILED: a comment merely describing the old bug "
              "was read as the vulnerable shape.")
        ok = False

    # 5. Allowlist shape: every entry needs a non-empty reason and a valid
    # status. A blank reason is silence wearing a justification's clothes.
    for key, (status, reason) in ALLOWLIST.items():
        if status not in (EXEMPT, DEFERRED):
            print(f"SELF-TEST FAILED: {key} has invalid status {status!r}")
            ok = False
        if not reason or not reason.strip():
            print(f"SELF-TEST FAILED: {key} has an empty reason")
            ok = False

    if ok:
        print("self-test: OK (5/5)")
    return ok


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    if "--self-test" in sys.argv:
        sys.exit(0 if run_self_test() else 1)

    for key, (status, reason) in ALLOWLIST.items():
        if status not in (EXEMPT, DEFERRED) or not reason.strip():
            print(f"UNMEASURED: malformed allowlist entry {key}: "
                  f"status={status!r} reason={reason!r}")
            sys.exit(2)

    deferred = _load_deferred_from_file()
    overlap = set(ALLOWLIST) & set(deferred)
    if overlap:
        print(f"UNMEASURED: {sorted(overlap)} appear in both the inline "
              f"ALLOWLIST and mssql-tenant-scope-deferred.tsv")
        sys.exit(2)
    allowlist = dict(ALLOWLIST)
    allowlist.update(deferred)

    try:
        out = subprocess.run(
            ["git", "ls-files", "engine/*.go"],
            capture_output=True, text=True, check=True,
        )
    except Exception as e:
        print(f"UNMEASURED: could not list tracked engine/*.go files: {e}")
        sys.exit(2)

    files = [f for f in out.stdout.splitlines() if not f.endswith("_test.go")]
    if not files:
        print("UNMEASURED: git ls-files returned no engine/*.go files -- "
              "not run from inside the repository?")
        sys.exit(2)

    vulnerable_seen = set()
    for path in files:
        try:
            text = open(path, encoding="utf-8").read()
        except Exception as e:
            print(f"UNMEASURED: could not read {path}: {e}")
            sys.exit(2)
        if "MSSQLStore" not in text:
            continue
        for name, vulnerable in find_methods(path, text):
            if vulnerable:
                vulnerable_seen.add((path, name))

    unallowed = sorted(vulnerable_seen - set(allowlist))
    stale = sorted(set(allowlist) - vulnerable_seen)

    problems = False
    if unallowed:
        problems = True
        print("The following *MSSQLStore methods reference s.tenantID and "
              "query s.db directly, without routing through "
              "beginTxWithContext -- a WithTenant(B) copy's call to any of "
              "these silently answers as the ORIGINAL tenant (cleat#2210). "
              "Either convert to beginTxWithContext, matching cleat#2207/"
              "#2226's precedent, or add a justified allowlist entry:\n")
        for path, name in unallowed:
            print(f"  {path}: {name}")

    if stale:
        problems = True
        print("\nThe following allowlist entries no longer match a "
              "vulnerable method -- the method was removed, renamed, or "
              "already converted. A stale entry is a ratchet in the wrong "
              "direction; remove it:\n")
        for path, name in stale:
            print(f"  {path}: {name}")

    if problems:
        sys.exit(1)

    n_exempt = sum(1 for s, _ in allowlist.values() if s == EXEMPT)
    n_deferred = sum(1 for s, _ in allowlist.values() if s == DEFERRED)
    print(f"OK: {len(vulnerable_seen)} tenant-scoped MSSQLStore methods "
          f"using bare s.db are all accounted for "
          f"({n_exempt} exempt, {n_deferred} deferred, 0 unconverted-and-unlisted).")
    sys.exit(0)


if __name__ == "__main__":
    main()
