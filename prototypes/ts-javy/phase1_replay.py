#!/usr/bin/env python3
"""Q2 replay half: hold a real journey run in flight at its child wait, then replay its persisted history 3x with the
Go harness (replay/), with a tampered-history red control. Raw `go test -v` output goes to evidence/phase1/."""
import os, subprocess, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from phase1_lib import *

key = (W / "p1.key").read_text().strip()
log = W / "worker-p1-replay.log"; log.write_bytes(b"")
w = Worker(log); w.start()
inp = {"sleepMs": 1500, "childMs": 600000}
wid = start(key, "journey", inp)
wait(key, wid, lambda r: any(e[1] == "await_signals" for e in events(wid)) and r["status"] == "ready", 30, "signal wait")
http("POST", f"/api/workflows/{wid}/signal", key, {"signal_name": "go", "payload": '"hello"'})
wait(key, wid, lambda r: any(e[1] == "await_child" for e in events(wid)) and r["status"] == "ready", 30, "child wait")
time.sleep(1)
print("run", wid, "in flight, history:", [e[1] for e in events(wid)], flush=True)
env = {**os.environ, "GOWORK": "off", "GOFLAGS": "-mod=mod", "GOPROXY": "off",
       "REPLAY_DSN": OWNER, "REPLAY_WORKFLOW_ID": wid, "REPLAY_WASM": str(W / "journey.wasm"),
       "REPLAY_ENTRY": "journey", "REPLAY_INPUT": json.dumps(inp)}
p = subprocess.run(["go", "test", "-count=1", "-v", "./..."], cwd=HERE / "replay", env=env, capture_output=True, text=True, timeout=600)
out = p.stdout + p.stderr
print(out)
(HERE / "evidence" / "phase1" / "q2-replay-3x.txt").write_text(out)
w.kill9()
ran = "--- PASS: TestReplayOfAnInFlightJSRunIsIdenticalThreeTimes" in out
skipped = "SKIP" in out or "UNMEASURED" in out
print(f"rc={p.returncode} ran-and-passed={ran} skipped={skipped}")
sys.exit(0 if (p.returncode == 0 and ran and not skipped) else 1)
