#!/usr/bin/env python3
"""Phase 5 / Q7 (plan section 2): "A thrown error names the .ts file and line". Three failures in one module (a throw in a helper, the same
throw after a resume, and a ZodError raised inside node_modules), run on a stock worker, built WITH source maps and WITHOUT (SOURCEMAP=0,
the known-negative). The expected line is found by searching workflows/srcmap/srcmap.ts for MARK: comments, not typed here.
Raw: evidence/phase5/srcmap.json. Exit 1 on a failed verdict, 2 if a run could not look."""
import json, os, re, subprocess, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from phase1_lib import *

SRC = "workflows/srcmap/srcmap.ts"
lines = (HERE / SRC).read_text().splitlines()
def mark(m):
    hit = [i + 1 for i, l in enumerate(lines) if f"MARK:{m}" in l]
    if len(hit) != 1: raise SystemExit(f"UNMEASURED: marker {m} found {len(hit)} times in {SRC}")
    return hit[0]
L = {m: mark(m) for m in ("inner-throw", "call-from-workflow", "call-after-resume", "zod-parse", "lib-throw")}
throw_col = lines[L["inner-throw"] - 1].index("throw") + 1
def build(out, **env):
    p = subprocess.run(["./build.sh", "srcmap", "srcmap_throw,srcmap_late,srcmap_zod,srcmap_lib"], cwd=HERE, env={**os.environ, "OUT_NAME": out, **env}, capture_output=True, text=True)
    if p.returncode: raise SystemExit(f"UNMEASURED: build {out} failed:\n{p.stdout[-300:]}{p.stderr[-300:]}")
    emb = re.search(r"source map embedded: ([\d,]+) bytes", p.stdout)
    return os.path.getsize(W / f"{out}.wasm"), (int(emb.group(1).replace(",", "")) if emb else 0)
sz_map, emb = build("srcmap"); sz_nomap, _ = build("srcmap_nomap", SOURCEMAP="0")
print(f"module with maps {sz_map:,} B (map payload {emb:,} B), without {sz_nomap:,} B, delta {sz_map - sz_nomap:,} B", flush=True)
# What the map costs, on bundles of different sizes (the same workflow built both ways; nothing else differs).
cost = {}
for wf, ent in (("crosslang", "place_order"), ("bench", "bench_steps"), ("npm_lodash", "npm_lodash"), ("npm_zod", "npm_zod")):
    sizes = {}
    for v, extra in (("map", {}), ("nomap", {"SOURCEMAP": "0"})):
        q = subprocess.run(["./build.sh", wf, ent], cwd=HERE, env={**os.environ, "OUT_NAME": f"{wf}_{v}_cost", **extra}, capture_output=True, text=True)
        if q.returncode: raise SystemExit(f"UNMEASURED: build {wf} {v} failed: {q.stderr[-200:]}")
        sizes[v] = os.path.getsize(W / f"{wf}_{v}_cost.wasm")
    cost[wf] = {**sizes, "delta": sizes["map"] - sizes["nomap"], "pct": round(100 * (sizes["map"] - sizes["nomap"]) / sizes["nomap"], 1)}
    print(f"  map cost {wf:11s} {sizes['nomap']:>9,d} -> {sizes['map']:>9,d} B  (+{cost[wf]['delta']:,d} B, +{cost[wf]['pct']}%)", flush=True)
for variant, wasm in (("map", "srcmap"), ("nomap", "srcmap_nomap")):
    for e in ("srcmap_throw", "srcmap_late", "srcmap_zod", "srcmap_lib"): print(" ", deploy(f"{variant}_{e}", W / f"{wasm}.wasm")[:100], flush=True)

key = KEY_FILE.read_text().strip()
log = W / "worker-p5-srcmap.log"; log.write_bytes(b""); w = Worker(log); w.start()
err = {}
for variant in ("map", "nomap"):
    for e in ("srcmap_throw", "srcmap_late", "srcmap_zod", "srcmap_lib"):
        wid = start(key, f"{variant}_{e}", {"__entry_point": e})
        r = wait(key, wid, terminal, 60, f"{variant}/{e}")
        err[(variant, e)] = (r.get("error") or "")
        print(f"## {variant:5s} {e:13s} status={r['status']}\n   {err[(variant, e)][:620]!r}", flush=True)
w.kill9()

res = []
def verdict(name, ok, detail=""): res.append(ok); print(f"  {'PASS' if ok else 'FAIL'}  {name}{('  ' + detail) if detail else ''}")
print("\nverdicts")
def frame(t, line): return re.search(rf"{re.escape(SRC)}:{line}:(\d+)", t)
for e, call in (("srcmap_throw", "call-from-workflow"), ("srcmap_late", "call-after-resume")):
    t = err[("map", e)]; f1, f2 = frame(t, L["inner-throw"]), frame(t, L[call])
    verdict(f"{e}: names {SRC} at the throw line {L['inner-throw']}", bool(f1), f"col={f1.group(1) if f1 else None} (the `throw` keyword is at col {throw_col})")
    verdict(f"{e}: and at the call site, line {L[call]}", bool(f2))
    verdict(f"{e}: no bundle position is left in the message", "function.mjs" not in t)
    verdict(f"{e}: known-negative, the no-map build names only the bundle", "function.mjs:" in err[("nomap", e)] and ".ts:" not in err[("nomap", e)], err[("nomap", e)].split("\n")[1][:70] if "\n" in err[("nomap", e)] else "")
t = err[("map", "srcmap_zod")]
verdict(f"srcmap_zod: names {SRC} at the user's .parse line {L['zod-parse']}", bool(frame(t, L["zod-parse"])))
# zod's own stack holds only the caller's frames (observed), so a library frame is tested with a plain Error thrown inside uuid.
t = err[("map", "srcmap_lib")]
verdict(f"srcmap_lib: names {SRC} at the call line {L['lib-throw']}", bool(frame(t, L["lib-throw"])))
verdict("srcmap_lib: names a node_modules/uuid frame by path, mapped from the bundle (a library frame is mapped too)", bool(re.search(r"node_modules/uuid/[^\s:]+\.js:\d+:\d+", t)), re.search(r"at \S+ \((node_modules/uuid/[^)]*)\)", t).group(1) if re.search(r"at \S+ \((node_modules/uuid/[^)]*)\)", t) else "")
verdict("srcmap_lib: no bundle position is left in the message", "function.mjs" not in t)
verdict("srcmap_lib: known-negative, the no-map build names only the bundle", "function.mjs:" in err[("nomap", "srcmap_lib")] and "node_modules" not in err[("nomap", "srcmap_lib")])
verdict("srcmap_zod: known-negative, the no-map build names only the bundle", "function.mjs:" in err[("nomap", "srcmap_zod")] and "node_modules" not in err[("nomap", "srcmap_zod")])
firstline = lambda t: (re.search(r'"error":"(.*?)\\n', t) or re.search(r'"error":"(.*)', t)).group(1)
verdict("the message itself is unchanged by maps (the text before the first stack frame is identical, mapped vs unmapped)", all(firstline(err[("map", e)]) == firstline(err[("nomap", e)]) for e in ("srcmap_throw", "srcmap_late", "srcmap_zod", "srcmap_lib")))
os.makedirs(HERE / "evidence" / "phase5", exist_ok=True)
json.dump({"expected_lines": L, "throw_col": throw_col, "module_bytes": {"map": sz_map, "nomap": sz_nomap}, "map_payload_bytes": emb, "map_cost_by_workflow": cost, "errors": {f"{k[0]}/{k[1]}": v for k, v in err.items()}}, open(HERE / "evidence" / "phase5" / "srcmap.json", "w"), indent=1)
print(f"\n{sum(res)} of {len(res)} verdicts passed"); sys.exit(0 if all(res) else 1)
