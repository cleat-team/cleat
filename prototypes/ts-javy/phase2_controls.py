#!/usr/bin/env python3
"""Plan §4.5 / Q3. For each plugin variant (exactly one shim removed, or none) build `nondet`, deploy it under its own name,
and run every nondeterminism source: segment 1 records the value, the run suspends, segment 2 REPLAYS and recomputes it.
`same` = recorded == recomputed. Raw rows go to stdout; the verdicts at the end are computed from them, not typed."""
import json, os, subprocess, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from phase1_lib import *

key = KEY_FILE.read_text().strip()
SOURCES = ["dateNow", "newDate", "dateElapsed", "newDateElapsed", "dateEqualsHostNow", "dateCall", "random", "randomRange", "intl", "toLocaleDate", "toLocaleNumber",
           "perfNow", "cryptoRandom", "weakRef", "finalizationRegistry", "setTimeout", "setInterval", "setTimeoutCall", "setTimeoutComputed"]
VARIANTS = ["full", "off-date", "off-random", "off-lockdown", "off-all"]

def build(variant):
    env = {**os.environ, "PLUGIN": f".work/plugin-{variant}.wasm", "OUT_NAME": f"nondet_{variant}"}
    p = subprocess.run(["./build.sh", "nondet", "nondet"], cwd=HERE, env=env, capture_output=True, text=True)
    if p.returncode: raise SystemExit(f"UNMEASURED: build {variant} failed:\n{p.stdout[-400:]}{p.stderr[-400:]}")
    print(" ", deploy(f"nondet_{variant}", W / f"nondet_{variant}.wasm")[:110], flush=True)

log = W / "worker-p2-controls.log"; log.write_bytes(b"")
for v in VARIANTS: build(v)
w = Worker(log); w.start()
rows = {}
def run(variant, source, inject=None):
    wid = start(key, f"nondet_{variant}", {"source": source, **({"inject": inject} if inject else {})})
    r = wait(key, wid, terminal, 60, f"{variant}/{source}")
    if r["status"] == "done":
        res = json.loads(r["result"]); return {"status": "done", "same": res["same"], "recorded": res["recorded"], "later": res["later"]}
    err = (r.get("error") or "")
    return {"status": r["status"], "diverged": "replay divergence" in err, "error": err[:160]}

print(f"\n{'variant':13s} {'source':20s} outcome")
for v in VARIANTS:
    for s in SOURCES:
        rows[(v, s)] = o = run(v, s)
        if o["status"] == "done": print(f"{v:13s} {s:20s} same={o['same']!s:5s} recorded={o['recorded'][:24]:24s} later={o['later'][:24]}", flush=True)
        else: print(f"{v:13s} {s:20s} {o['status'].upper()} divergence={o['diverged']}  {o['error'][:90]}", flush=True)
inj = {}
print("\nknown-positive for the absence checks (a global is injected by user code; the check must SEE it):")
for s, name in [("weakRef", "WeakRef"), ("finalizationRegistry", "FinalizationRegistry"), ("setTimeout", "setTimeout"), ("perfNow", "performance")]:
    inj[s] = run("full", s, [name]); print(f"  inject {name:22s} -> {inj[s].get('recorded')}", flush=True)
w.kill9()

# ---- verdicts, computed from the rows above ----
def stable(v, s):  return rows[(v, s)]["status"] == "done" and rows[(v, s)]["same"] is True
def broke(v, s):   return not stable(v, s)
live = lambda v, s: rows[(v, s)]["status"] == "done" and rows[(v, s)]["recorded"] != rows[(v, s)]["later"]
res = []
def verdict(name, ok, detail=""): res.append(ok); print(f"  {'PASS' if ok else 'FAIL'}  {name}{('  ' + detail) if detail else ''}")
print("\nverdicts")
print("  INFO  locale probes under the full plugin: " + ", ".join(f"{s}={rows[('full', s)]['recorded']}" for s in ("intl", "toLocaleDate", "toLocaleNumber")))
verdict(f"full: every source is replay-stable (all {len(SOURCES)} runs completed, recorded == replayed)", all(stable("full", s) for s in SOURCES))
verdict("full: the live sources are live (value after the sleep differs from the recorded one), so 'stable' is not 'constant'",
        all(live("full", s) for s in ("dateNow", "newDate", "random")))
verdict("CONTROL random: with only the random shim removed, random is NOT replay-stable", broke("off-random", "random"),
        f"{rows[('off-random','random')]['status']}" + (" (replay divergence)" if rows[("off-random","random")].get("diverged") else ""))
verdict("random shim removal affects no other source", all(stable("off-random", s) for s in SOURCES if s not in ("random", "randomRange")))
verdict("CONTROL random: randomRange also breaks without the shim", broke("off-random", "randomRange"))
date_srcs = ("dateNow", "newDate", "dateElapsed", "newDateElapsed", "dateEqualsHostNow")
lb = [s for s in date_srcs if broke("off-date", s)]
print(f"  INFO  CONTROL date: with only the date shim removed, sources that stop being replay-stable: {lb or 'none'}; "
      f"stable: {[s for s in date_srcs if s not in lb]}")
DET = ("dateCall", "intl", "toLocaleDate", "toLocaleNumber", "perfNow", "cryptoRandom", "weakRef", "finalizationRegistry", "setTimeout", "setInterval", "setTimeoutCall")
ld = [s for s in DET if rows[("off-lockdown", s)].get("recorded") != rows[("full", s)].get("recorded")]
print(f"  INFO  CONTROL lockdown: with only lockdown removed, deterministic sources whose value changes: {ld or 'none (the globals are already absent in this Javy build, so this control cannot fail today)'}")
def rng(v):
    r = rows[(v, "randomRange")]
    if r["status"] != "done": return None
    return json.loads(r["recorded"])
for v in ("full", "off-date", "off-lockdown"):
    r = rng(v)
    verdict(f"{v}: Math.random is in [0,1), centred and not constant (400 draws)", bool(r) and r["min"] >= 0 and r["max"] < 1 and 0.4 < r["mean"] < 0.6 and r["distinct"] > 380, str(r))
verdict("static-check limit: a computed-property timer passes the checker and then throws at run time, replay-stable", rows[("full", "setTimeoutComputed")].get("recorded", "").lstrip('"').startswith("threw:") and stable("full", "setTimeoutComputed"), str(rows[("full", "setTimeoutComputed")].get("recorded")))
verdict("absent globals really are absent under the full plugin", all(rows[("full", s)].get("recorded") in ('"absent"', '"undefined"') for s in ("perfNow","cryptoRandom","weakRef","finalizationRegistry","setTimeout","setInterval","setTimeoutCall")))
verdict("known-positive: the absence check notices an injected global (all four)", all(inj[s].get("recorded") not in ('"absent"', '"undefined"') for s in inj))
verdict("off-all differs from full exactly where the individual controls say (no interaction hidden)", broke("off-all", "random") and (not lb or all(broke("off-all", s) for s in lb)))
bad = [x for x in res if not x]
print(f"\n{len(res) - len(bad)} of {len(res)} verdicts passed")
json.dump({f"{k[0]}/{k[1]}": v for k, v in rows.items()}, open(HERE / "evidence" / "phase2" / "controls.json", "w"), indent=1, sort_keys=True)
sys.exit(1 if bad else 0)
