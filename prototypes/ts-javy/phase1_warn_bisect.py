#!/usr/bin/env python3
"""Which journey variants make the engine log 'replay history is shorter than the instance's recorded event count'?
Variants differ only in how many SIGKILLs interrupt the run. Raw counts are printed, one line per variant."""
import sys, time
sys.path.insert(0, __import__("os").path.dirname(__import__("os").path.abspath(__file__)))
from phase1_lib import *
key = (W / "p1.key").read_text().strip()
log = W / "worker-p1-bisect.log"; log.write_bytes(b"")
w = Worker(log); w.start()

def run(label, kills):
    wid = start(key, "journey", {"sleepMs": 1500, "childMs": 4000})
    wait(key, wid, lambda r: any(e[1] == "await_signals" for e in events(wid)) and r["status"] == "ready", 30, "signal wait")
    if "A" in kills: time.sleep(1); w.kill9(); time.sleep(1.5); w.start()
    http("POST", f"/api/workflows/{wid}/signal", key, {"signal_name": "go", "payload": '"x"'})
    wait(key, wid, lambda r: any(e[1] == "await_child" for e in events(wid)) and r["status"] == "ready", 30, "child wait")
    if "B" in kills: time.sleep(1); w.kill9(); time.sleep(1.5); w.start()
    r = wait(key, wid, terminal, 60, "completion")
    n = sum(1 for l in log.read_text(errors="replace").splitlines() if "replay history is shorter" in l and wid in l)
    ec = psql(f"select event_count from workflow_instances where id='{wid}'")
    print(f"{label:34s} status={r['status']:5s} event_count={ec} warnings={n}", flush=True)

run("no kill", "")
run("kill at the signal wait only", "A")
run("kill at the child wait only", "B")
run("kill at both", "AB")
run("no kill (again)", "")
