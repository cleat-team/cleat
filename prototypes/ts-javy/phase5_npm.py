#!/usr/bin/env python3
"""Phase 5 / Q6 (plan section 2): bundle 4 common npm packages (zod, date-fns, lodash-es, uuid) into workflows and run them on a stock worker.
Every case is run twice: under plain Node (TZ=UTC) for the expected value, and in the guest, which also records it through a side effect,
suspends on a sleep and computes it again, so a case that is not replay-stable shows as recorded != replayed.
Verdict per case: EQUAL (guest answer == Node answer), DIFFERENT, GUEST_THREW (Node did not), BOTH_THREW. Known-positive for the comparator:
each package has a `control_differs` case whose answers differ by construction; the comparator must say DIFFERENT for all four.
Raw: evidence/phase5/npm.json. Exit 0 = ran to the end and the comparator's known-positive held; the package verdicts are reported, not asserted."""
import json, os, subprocess, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from phase1_lib import *

PKGS = {"zod": "npm_zod", "datefns": "npm_datefns", "lodash": "npm_lodash", "uuid": "npm_uuid"}
VERSIONS = json.load(open(HERE / "package.json"))["dependencies"]
def sh(*a, **kw): 
    p = subprocess.run(a, capture_output=True, text=True, cwd=HERE, **kw)
    if p.returncode: raise SystemExit(f"UNMEASURED: {' '.join(a)[:120]}: {p.stderr[-400:]}")
    return p.stdout

# ---- expected, from Node ----
X = W / "npm_expected"; X.mkdir(exist_ok=True)
for n, mod in [("common", "workflows/npm_common.ts")] + [(p, f"workflows/{m}/{m}.ts") for p, m in PKGS.items()]:
    sh("npx", "esbuild", mod, "--bundle", "--platform=node", "--format=esm", "--target=es2020", f"--outfile={X}/{n}.mjs", "--log-level=warning")
expected = json.loads(sh("node", "phase5_npm_expected.mjs", str(X), env={**os.environ, "TZ": "UTC"}))

# ---- guest ----
key = KEY_FILE.read_text().strip()
sizes = {}
sizes_nomap = {}
for p, m in PKGS.items():
    sh("./build.sh", m, m)                                   # the default build: source maps embedded
    sizes[p] = os.path.getsize(W / f"{m}.wasm"); print(" ", deploy(m, W / f"{m}.wasm")[:110], flush=True)
    sh("./build.sh", m, m, env={**os.environ, "SOURCEMAP": "0", "OUT_NAME": f"{m}_nomap"})   # size only: what the maps cost this bundle
    sizes_nomap[p] = os.path.getsize(W / f"{m}_nomap.wasm")
log = W / "worker-p5-npm.log"; log.write_bytes(b""); w = Worker(log); w.start()
rows = []
for p, m in PKGS.items():
    for case, exp in expected[p].items():
        wid = start(key, m, {"case": case})
        r = wait(key, wid, terminal, 60, f"{p}/{case}")
        if r["status"] != "done":
            rows.append({"pkg": p, "case": case, "verdict": "RUN_FAILED", "status": r["status"], "error": (r.get("error") or "")[:300], "node": exp}); continue
        g = json.loads(r["result"])
        got = g["recorded"]
        gt, nt = got.startswith("threw:"), exp.startswith("threw:")
        v = "EQUAL" if got == exp else ("BOTH_THREW" if gt and nt else "GUEST_THREW" if gt else "NODE_THREW" if nt else "DIFFERENT")
        rows.append({"pkg": p, "case": case, "verdict": v, "replay_same": g["replaySame"], "later_same": g["laterSame"], "guest": got[:300], "node": exp[:300]})
w.kill9()

print(f"\n{'package':8s} {'case':22s} {'verdict':12s} replay-stable later-same   (guest answer when not EQUAL)")
for r in rows:
    extra = "" if r["verdict"] == "EQUAL" else f"  guest={r.get('guest', r.get('error'))[:110]!r}  node={r['node'][:60]!r}"
    print(f"{r['pkg']:8s} {r['case']:22s} {r['verdict']:12s} {str(r.get('replay_same')):13s}{str(r.get('later_same')):10s}{extra}")
ok = True
ctl = [r for r in rows if r["case"] == "control_differs"]
print(f"\nknown-positive: control_differs reported DIFFERENT for {sum(r['verdict'] == 'DIFFERENT' for r in ctl)} of {len(ctl)} packages (want all 4)")
ok &= len(ctl) == 4 and all(r["verdict"] == "DIFFERENT" for r in ctl)
print("\nper package (control_differs excluded):")
summary = {}
for p in PKGS:
    pr = [r for r in rows if r["pkg"] == p and r["case"] != "control_differs"]
    eq = [r["case"] for r in pr if r["verdict"] == "EQUAL"]; bad = [(r["case"], r["verdict"]) for r in pr if r["verdict"] != "EQUAL"]
    summary[p] = {"version": VERSIONS.get({"datefns": "date-fns", "lodash": "lodash-es"}.get(p, p)), "module_bytes": sizes[p], "module_bytes_without_source_map": sizes_nomap[p], "equal": eq, "not_equal": bad,
                  "replay_unstable": [r["case"] for r in pr if r.get("replay_same") is False]}
    print(f"  {p:8s} {summary[p]['version']:8s} module {sizes[p]:>9,d} B ({sizes_nomap[p]:,d} without source map)  EQUAL {len(eq)} of {len(pr)}  not equal: {bad or 'none'}  replay-unstable: {summary[p]['replay_unstable'] or 'none'}")
os.makedirs(HERE / "evidence" / "phase5", exist_ok=True)
json.dump({"rows": rows, "summary": summary}, open(HERE / "evidence" / "phase5" / "npm.json", "w"), indent=1, sort_keys=True)
sys.exit(0 if ok else 1)
