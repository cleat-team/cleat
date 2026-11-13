#!/usr/bin/env python3
"""Coordinator's control for PHASE1 observation 1: does a GO guest log `replay history is shorter than the instance's recorded
event count` for the same sleep -> signal -> child sequence? Same five kill patterns as phase1_warn_bisect.py, run TWICE per
guest (the JS guest was bisected twice: 5 of 5 then 3 of 5), JS and Go interleaved in one session so load and timing match.
Counts only; the rows are raw output. Exit 0 always unless a run could not be completed (then 2, UNMEASURED)."""
import json, os, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from phase1_lib import *

key = (W / "p1.key").read_text().strip()
log = W / "worker-p2-gocontrol.log"; log.write_bytes(b"")
print(" ", deploy("go_journey", W / "go_journey.wasm")[:110]); print(" ", deploy("go_sleeper", W / "go_journey.wasm")[:110])
w = Worker(log); w.start()
GUESTS = {
    "js": ("journey", {"sleepMs": 1500, "childMs": 4000}),
    "go": ("go_journey", {"in": {"sleepMs": 1500, "childMs": 4000}, "__entry_point": "go_journey"}),
}
PATTERNS = [("no kill", ""), ("kill at the signal wait only", "A"), ("kill at the child wait only", "B"), ("kill at both", "AB"), ("no kill (again)", "")]

def run(guest, label, kills):
    name, inp = GUESTS[guest]
    wid = start(key, name, inp)
    wait(key, wid, lambda r: any(e[1] == "await_signals" for e in events(wid)) and r["status"] == "ready", 40, "signal wait")
    if "A" in kills: time.sleep(1); w.kill9(); time.sleep(1.5); w.start()
    http("POST", f"/api/workflows/{wid}/signal", key, {"signal_name": "go", "payload": '"x"'})
    wait(key, wid, lambda r: any(e[1] == "await_child" for e in events(wid)) and r["status"] == "ready", 40, "child wait")
    if "B" in kills: time.sleep(1); w.kill9(); time.sleep(1.5); w.start()
    r = wait(key, wid, terminal, 90, "completion")
    n = sum(1 for l in log.read_text(errors="replace").splitlines() if "replay history is shorter" in l and wid in l)
    ec = psql(f"select event_count from workflow_instances where id='{wid}'")
    kinds = [e[1] for e in events(wid)]
    print(f"{guest} {label:30s} status={r['status']:5s} event_count={ec} warnings={n}", flush=True)
    return r["status"], n

tot = {"js": [0, 0, 0], "go": [0, 0, 0]}   # runs, runs-with-warning, runs-not-done
for rnd in (1, 2):
    print(f"\n-- pass {rnd}")
    for label, kills in PATTERNS:
        for guest in ("js", "go"):
            st, n = run(guest, label, kills)
            t = tot[guest]; t[0] += 1; t[1] += 1 if n else 0; t[2] += 0 if st == "done" else 1
w.kill9()
print("\nsummary (runs, runs that logged the warning, runs that did not end done):")
for g, t in tot.items(): print(f"  {g}: {t[0]} runs, {t[1]} with the warning, {t[2]} not done")
sys.exit(0)
