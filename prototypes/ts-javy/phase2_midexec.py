#!/usr/bin/env python3
"""Phase 2 durability (coordinator, decision 3): kill -9 the worker in the MIDDLE of guest execution, not while the run is suspended.
Two shapes: the burn after a recorded side effect, and the burn before any host call (the kill leaves no history). Each is
compared with an uninterrupted run of the same input. Exit 0 only if every check passes."""
import json, os, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from phase1_lib import *

key = KEY_FILE.read_text().strip()
ITERS = 60_000_000   # was 120M: its baseline took 23.1 s at load ~32, against the worker's 30 s instance limit, and trapped (cleat-review, 2026-11-13)
log = W / "worker-p2-midexec.log"; log.write_bytes(b"")
w = Worker(log); w.start()
res = []
def verdict(name, ok, detail=""): res.append(ok); print(f"  {'PASS' if ok else 'FAIL'}  {name}{('  ' + detail) if detail else ''}", flush=True)
loglines = lambda wid, needle: [l for l in log.read_text(errors="replace").splitlines() if wid in l and needle in l]

print(f"iterations per run: {ITERS:,} (about 6 s of CPU unloaded, no host call inside the loop; the kill is sent 3 s in)\n")
for shape, spin_first in (("burn AFTER a recorded side effect", False), ("burn BEFORE any host call", True)):
    print(f"### {shape}")
    inp = {"iters": ITERS, "spinFirst": spin_first}
    t0 = time.time(); base = start(key, "busy", inp); rb = wait(key, base, terminal, 120, "baseline"); tb = time.time() - t0
    base_res = json.loads(rb["result"]); print(f"  baseline (uninterrupted): {tb:.1f}s  sum={base_res['sum']}")
    wid = start(key, "busy", inp)
    wait(key, wid, lambda r: r["status"] == "running", 30, "running"); time.sleep(3)
    r = get(key, wid)
    rows_at_kill = len(events(wid))
    print(f"  3 s in: status={r['status']!r}, persisted history rows={rows_at_kill}")
    verdict("the run was still executing when the kill was sent (not suspended, not finished)", r["status"] == "running" and not terminal(r), f"status={r['status']}")
    w.kill9(); t_kill = time.time(); time.sleep(1); w.start()
    r = wait(key, wid, terminal, 240, "completion after the kill"); t_done = time.time() - t_kill
    out = json.loads(r["result"]) if r["status"] == "done" else {}
    # The API reports `done` slightly before the worker's own "workflow completed" line is written (the first version of this check
    # counted at that instant and read 0 for a run that had in fact completed): wait for the line, bounded, then count strictly.
    for _ in range(100):
        if loglines(wid, "workflow completed"): break
        time.sleep(0.1)
    time.sleep(1)  # and give a (wrong) second completion the chance to appear before asserting there is exactly one
    claims, done = loglines(wid, "claimed workflow"), loglines(wid, "workflow completed")
    print(f"  after the kill: status={r['status']} in {t_done:.1f}s, reclaim_count={r['reclaim_count']}, claims in the worker logs={len(claims)}, completions={len(done)}")
    verdict("the run completed", r["status"] == "done")
    verdict("its computed sum equals the uninterrupted run's", out.get("sum") == base_res["sum"], f"{out.get('sum')} vs {base_res['sum']}")
    verdict("it was claimed again after the restart (>= 2 claims) and completed exactly once", len(claims) >= 2 and len(done) == 1, f"claims={len(claims)} completions={len(done)}")
    verdict("the recorded side-effect value is a valid Math.random result", isinstance(out.get("a"), float) and 0 <= out["a"] < 1, str(out.get("a")))
    print()
w.kill9()
print(f"{sum(res)} of {len(res)} checks passed")
sys.exit(0 if all(res) else 1)
