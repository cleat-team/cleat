#!/usr/bin/env python3
"""Phase 1 decision (4): error and exception paths. Records how each failure ends; asserts only that none hangs."""
import json, os, re, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from phase1_lib import *

key = (W / "p1.key").read_text().strip()
log = W / "worker-p1-errors.log"; log.write_bytes(b"")
w = Worker(log); w.start()
POLICY = re.compile(r"fd_seek|wasi|WASI|policy|unsupported import|forbidden", re.I)
hung = []
unmeasured = []
for name in ("thrower", "rejecter", "late_thrower", "raw_throw", "oom"):
    mark = log.stat().st_size
    wid = start(key, name, {"__entry_point": name})
    t0 = time.time()
    try:
        r = wait(key, wid, terminal, 120, "terminal state")
    except SystemExit as e:
        r = get(key, wid); hung.append(name); print(f"## {name}: DID NOT TERMINATE in 120s: {r['status']}", flush=True)
    dt = time.time() - t0
    if "cannot determine entry point" in (r.get("error") or r.get("error_message") or ""):
        unmeasured.append(name)
    tail = log.read_bytes()[mark:].decode(errors="replace").splitlines()
    pol = [l[:200] for l in tail if POLICY.search(l)]
    print(f"## {name}: status={r['status']} after {dt:.1f}s  error_code={r.get('error_code')!r} reclaim_count={r.get('reclaim_count')} attempts={r.get('attempts', r.get('retry_count'))}")
    print("   error:", (r.get("error") or r.get("error_message") or "")[:420].replace("\n", " | "))
    print("   result:", (r.get("result") or "")[:120])
    print(f"   worker-log lines mentioning wasi/policy/fd_seek: {len(pol)}")
    for l in pol[:3]: print("     ", l)
    wl = [l[:260] for l in tail if '"level":"ERROR"' in l or '"level":"WARN"' in l]
    print(f"   worker WARN/ERROR lines for this run: {len(wl)}")
    for l in wl[:4]: print("     ", l)
w.kill9()
print("\nhung:", hung or "none")
print("UNMEASURED (failed before reaching the code under test):", unmeasured or "none")
sys.exit(2 if unmeasured else (1 if hung else 0))
