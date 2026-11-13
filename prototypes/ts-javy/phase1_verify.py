#!/usr/bin/env python3
"""Phase 1 / Q2: sleep, signal and child workflows suspend and resume across a worker restart, a `finally` does not run
on suspension, and Promise.race/all are deterministic. Raw output is the evidence (evidence/phase1/)."""
import json, sys, time
sys.path.insert(0, __import__("os").path.dirname(__import__("os").path.abspath(__file__)))
from phase1_lib import *

key = (W / "p1.key").read_text().strip()
log1 = W / "worker-p1-a.log"
log1.write_bytes(b"")
logs = [log1]
w = Worker(log1)
results = {}


def check(name, ok, detail=""):
    results[name] = ok
    print(f"  {'PASS' if ok else 'FAIL'}  {name}{('  ' + detail) if detail else ''}", flush=True)


def suspended(r):
    return r["status"] in ("ready", "waiting", "sleeping", "suspended") and r.get("assigned_to") in ("", None)


print("### deploy"); [print(" ", deploy(n, W / f"{n}.wasm")[:100]) for n in ("sleeper", "signaler", "parent", "racer", "finalizer", "journey")]
print(" ", deploy("finalizer_throw", W / "finalizer_throw.wasm")[:100])
w.start()

print("\n### S1 sleeper: SIGKILL the worker while the run is suspended on its sleep, restart, it completes")
wid = start(key, "sleeper", {"ms": 6000})
r = wait(key, wid, lambda r: r["status"] == "ready" and r.get("assigned_to") in ("", None), 20, "suspension")
ev1 = events(wid); print("  history at suspension:", ev1)
w.kill9(); print("  worker SIGKILLed"); time.sleep(1.5); w.start(); print("  worker restarted")
r = wait(key, wid, terminal, 60, "completion")
check("S1 sleeper completes after a restart", r["status"] == "done" and json.loads(r["result"]) == {"slept": 6000, "workflow": wid}, r["result"])
print(f"  info  reclaim_count={r['reclaim_count']} (not asserted: whether the kill landed during a claimed segment is timing, and either value completed correctly)")

print("\n### S2 signaler: SIGKILL while waiting for the signal, restart, deliver the signal")
wid = start(key, "signaler", {})
wait(key, wid, lambda r: r["status"] == "ready" and r.get("assigned_to") in ("", None) or r["status"] == "waiting", 20, "suspension")
time.sleep(1)
r = get(key, wid); print("  status at suspension:", r["status"]); print("  history:", events(wid))
w.kill9(); time.sleep(1.5); w.start()
code, resp = http("POST", f"/api/workflows/{wid}/signal", key, {"signal_name": "go", "payload": json.dumps({"n": 7})}); print("  signal ->", code, resp)
r = wait(key, wid, terminal, 60, "completion")
check("S2 signaler completes with the signal's payload", r["status"] == "done" and json.loads(r["result"]) == {"got": {"n": 7}, "timedOut": False}, r["result"])

print("\n### S3 parent -> child sleeper: SIGKILL while both are suspended, restart")
wid = start(key, "parent", {"ms": 6000})
wait(key, wid, lambda r: r["status"] == "ready" and r.get("assigned_to") in ("", None), 20, "parent suspension")
time.sleep(1.5)
print("  parent history:", events(wid)); kids = psql(f"SELECT id, status FROM workflow_leases WHERE id <> '{wid}' ORDER BY created_at DESC LIMIT 1"); print("  newest other run:", kids)
w.kill9(); time.sleep(1.5); w.start()
r = wait(key, wid, terminal, 90, "completion")
res = json.loads(r["result"]) if r["status"] == "done" else None
check("S3 parent completes with the child's result", r["status"] == "done" and res and res["child"]["slept"] == 6000, r["result"])

print("\n### S4 journey: sleep, signal, child in ONE run; kill -9 at each suspension; history rows never change underneath")
wid = start(key, "journey", {"sleepMs": 1500, "childMs": 15000})
snaps = []
def snap(label):
    r = get(key, wid)
    assert not terminal(r), f"run already {r['status']} at snapshot '{label}': a completed run's history is deleted, the snapshot would prove nothing"
    rows = history_rows(wid); snaps.append((label, rows)); print(f"  [{label}] status={r['status']} {len(rows.splitlines())} history rows: {[e[1] for e in events(wid)]}")
wait(key, wid, lambda r: any(e[1] == "await_signals" for e in events(wid)) and r["status"] == "ready", 30, "the signal wait"); time.sleep(1)
snap("A: suspended at the signal wait")
w.kill9(); time.sleep(1.5); w.start(); time.sleep(2); snap("B: after SIGKILL #1, restarted")
http("POST", f"/api/workflows/{wid}/signal", key, {"signal_name": "go", "payload": json.dumps("hello")})
wait(key, wid, lambda r: any(e[1] == "await_child" for e in events(wid)) and r["status"] == "ready", 30, "the child wait"); time.sleep(1)
snap("C: suspended at the child wait")
w.kill9(); time.sleep(1.5); w.start(); time.sleep(2); snap("D: after SIGKILL #2, restarted, child still running")
r = wait(key, wid, terminal, 90, "completion")
res = json.loads(r["result"]) if r["status"] == "done" else None
check("S4 journey completes", r["status"] == "done" and res and res["signal"] == "hello" and res["child"]["slept"] == 15000, r["result"])
prefix_ok = True
for (la, ra), (lb, rb) in zip(snaps, snaps[1:]):
    a, b = ra.splitlines(), rb.splitlines()
    ok = b[:len(a)] == a
    prefix_ok &= ok
    print(f"  rows of [{la[:1]}] are an unchanged prefix of [{lb[:1]}]: {ok} ({len(a)} -> {len(b)} rows)")
check("S4 each earlier snapshot's history rows (all columns) are an unchanged prefix of every later one, across 2 SIGKILLs", prefix_ok and len(snaps) == 4)
short = [l for l in Path(log1).read_text(errors="replace").splitlines() if "replay history is shorter" in l and wid in l]
print(f"  'replay history is shorter' warnings for this run: {len(short)}")
for l in short: print("   ", l[:260])
(HERE / "evidence" / "phase1" / f"journey-history-{wid}.jsonl").write_text("\n".join(snaps[-1][1].splitlines()) + "\n")
print("  journey run id", wid)

print("\n### S5 finalizer: `finally` must not run at suspension, and must run once at completion")
wid = start(key, "finalizer", {})
wait(key, wid, lambda r: r["status"] == "ready" and r.get("assigned_to") in ("", None), 20, "suspension"); time.sleep(0.5)
n_susp = trace_count(logs, wid, "TRACE finalizer finally-ran")
check("S5 no `finally` ran during the suspended segment", n_susp == 0, f"trace lines so far: {n_susp}")
r = wait(key, wid, terminal, 30, "completion"); time.sleep(0.5)
n_done = trace_count(logs, wid, "TRACE finalizer finally-ran")
check("S5 the `finally` ran exactly once, in the segment that completed", r["status"] == "done" and n_done == 1, f"trace lines at completion: {n_done}")
print("  RED CONTROL: the same workflow built with suspension-by-THROW (the design the plan rejects)")
wid2 = start(key, "finalizer_throw", {})
wait(key, wid2, lambda r: terminal(r) or (r["status"] == "ready" and r.get("assigned_to") in ("", None)), 20, "first segment"); time.sleep(1)
n_throw = trace_count(logs, wid2, "TRACE finalizer finally-ran")
check("S5 control: with suspension-by-throw the `finally` DOES run during suspension", n_throw >= 1, f"trace lines in the first segment: {n_throw}")

print("\n### S6 racer: Promise.race / Promise.all over a suspended and a ready promise")
outs = []
for i in range(3):
    wid = start(key, "racer", {})
    r = wait(key, wid, terminal, 30, "completion"); outs.append((r["status"], r["result"]))
print("  results:", outs)
check("S6 the same result on three runs, each of which suspends twice", len(set(outs)) == 1 and outs[0][0] == "done", outs[0][1])

print("\n### summary"); bad = [k for k, v in results.items() if not v]
print(f"  {len(results) - len(bad)} of {len(results)} checks passed" + (f"; FAILED: {bad}" if bad else ""))
sys.exit(1 if bad else 0)
