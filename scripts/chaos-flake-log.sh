#!/usr/bin/env bash
# The flake log for the Chaos workflow (cleat#4169): every run of it, as a table re-derived from the API, so "no flaky failures for about three
# days" is a command anyone re-runs and not a sentence somebody wrote.
#
#   scripts/chaos-flake-log.sh [--since 2026-11-20T00:00:00Z] [--repo cleat-team/cleat] [--limit 200]
#
# One row per run: id, event, head sha, conclusion, wall seconds, and for a failed run the test cases the log names as failed.
# A FLAKE is a run that failed on a head whose rerun passed, or two runs of one head that disagree; the SAME HEAD column makes that visible.
# A run still in progress is listed as such and is not counted.
#
# Needs: gh (authenticated), python3.
set -euo pipefail

repo="cleat-team/cleat"; since=""; limit=200
while [ $# -gt 0 ]; do
  case "$1" in
    --since) since="$2"; shift 2 ;;
    --repo) repo="$2"; shift 2 ;;
    --limit) limit="$2"; shift 2 ;;
    *) echo "usage: $0 [--since ISO8601] [--repo owner/name] [--limit N]" >&2; exit 2 ;;
  esac
done

runs=$(gh run list --repo "$repo" --workflow "${CHAOS_WORKFLOW:-chaos.yml}" --limit "$limit" \
  --json databaseId,event,headSha,conclusion,status,createdAt,updatedAt,headBranch) || { echo "UNMEASURED: gh run list failed" >&2; exit 2; }

python3 - "$repo" "$since" <<PY
import json, os, subprocess, sys, datetime, collections
repo, since = sys.argv[1], sys.argv[2]
parser = os.path.join(os.path.dirname(os.path.abspath("$0")), "chaos-failed-cases.py")
runs = json.loads('''$runs''')
def ts(s): return datetime.datetime.fromisoformat(s.replace('Z', '+00:00'))
rows = []
for r in runs:
    if since and r["createdAt"] < since:
        continue
    secs = int((ts(r["updatedAt"]) - ts(r["createdAt"])).total_seconds())
    cases = ""
    if r["conclusion"] == "failure":
        p = subprocess.run(["gh", "run", "view", str(r["databaseId"]), "--repo", repo, "--log-failed"], capture_output=True, text=True)
        if p.returncode != 0:
            cases = "UNMEASURED (could not read the log)"
        else:
            q = subprocess.run([sys.executable, parser], input=p.stdout, capture_output=True, text=True)
            if q.returncode == 2:
                cases = "UNMEASURED (the log held no go test -json event: read it by hand)"
            else:
                names = [l for l in q.stdout.splitlines() if l]
                # A run that FAILED and yielded no failed case is not a clean run: a step other than go test failed, or the parse missed it.
                cases = "; ".join(names[:6]) + (" ..." if len(names) > 6 else "") if names else "UNMEASURED (the run failed but no failed case was found: read the log)"
    rows.append((r["createdAt"], r["databaseId"], r["event"], r["headSha"][:8], r["status"] if r["status"] != "completed" else r["conclusion"], secs, cases))
rows.sort()
print("| created | run | event | head | result | secs | failed cases |")
print("|---|---|---|---|---|---|---|")
for c in rows:
    print("| %s | %s | %s | %s | %s | %s | %s |" % c)
done = [r for r in rows if r[4] in ("success", "failure")]
byhead = collections.defaultdict(set)
for r in done:
    byhead[r[3]].add(r[4])
disagree = sorted(h for h, v in byhead.items() if len(v) > 1)
print()
print("completed runs: %d (success %d, failure %d); heads where runs DISAGREE (a flake): %s" % (
    len(done), sum(1 for r in done if r[4] == "success"), sum(1 for r in done if r[4] == "failure"), ", ".join(disagree) or "none"))
PY
