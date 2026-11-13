#!/usr/bin/env python3
"""The stale-worker kill must touch only workers THIS checkout started. Uses inert stand-ins (a `sleep` whose command line is faked),
never a real worker, so it is safe to run while someone else's run uses the same port and database. Each case states what the OLD
pattern kill (`pkill -f 'cleat-worker -db .*-api-addr :PORT'`) would have done, so the test can be seen to disagree with it."""
import os, re, subprocess, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import phase1_lib as L

res = []
def verdict(name, ok, detail=""): res.append(ok); print(f"  {'PASS' if ok else 'FAIL'}  {name}{('  ' + detail) if detail else ''}", flush=True)
alive = lambda pid: subprocess.run(["ps", "-p", str(pid)], capture_output=True).returncode == 0
OLD = f"cleat-worker -db .*-api-addr :{L.PORT}"
def standin(argv0_cmd):
    """An inert process (sleep) whose argv looks like `argv0_cmd`."""
    p = subprocess.Popen(["bash", "-c", f'exec -a "{argv0_cmd}" sleep 600'], start_new_session=True)
    time.sleep(0.4)
    return p
def old_pattern_matches(pid):
    return subprocess.run(["pgrep", "-f", OLD], capture_output=True, text=True).stdout.split().count(str(pid)) == 1

L.W.mkdir(exist_ok=True)
mine = foreign = unrecorded = None
try:
    print(f"settings: {L.PORT=} {L.BIN=}")
    # 1. FOREIGN: another checkout's worker, same port, a different binary path. Never touched.
    foreign = standin(f"/tmp/some-other-checkout/.work/cleat-worker -db x -api-addr :{L.PORT}")
    verdict("the old pattern WOULD have matched the foreign worker (so this test can disagree with it)", old_pattern_matches(foreign.pid))
    # 2. MINE: this checkout's binary, recorded in this checkout's pidfile. Stopped.
    mine = standin(f"{L.BIN} -db x -api-addr :{L.PORT}")
    (L.W / f"worker-{L.PORT}.pid").write_text(str(mine.pid))
    L.stop_stale()
    mine.poll(); time.sleep(0.3)
    verdict("a worker recorded in this checkout's pidfile, running this checkout's binary, IS stopped", not alive(mine.pid))
    verdict("the foreign worker on the same port was NOT touched", alive(foreign.pid), f"pid {foreign.pid}")
    verdict("the pidfile was removed", not (L.W / f"worker-{L.PORT}.pid").exists())
    # 3. UNRECORDED: this checkout's binary but no pidfile entry (we did not start it): refuse, do not kill.
    unrecorded = standin(f"{L.BIN} -db x -api-addr :{L.PORT}")
    try:
        L.stop_stale(); refused = False
    except SystemExit as e:
        refused = "UNMEASURED" in str(e)
    verdict("a process running this checkout's binary with NO pidfile entry makes it refuse as UNMEASURED", refused)
    verdict("and it was not killed", alive(unrecorded.pid))
    # 4. A pidfile that names a pid reused by something else (a foreign command line): the pid is NOT killed.
    unrecorded.kill(); unrecorded.wait()
    (L.W / f"worker-{L.PORT}.pid").write_text(str(foreign.pid))
    L.stop_stale()
    verdict("a pidfile naming a pid whose command line is NOT this checkout's binary kills nothing", alive(foreign.pid))
    # 5. Something that is not a worker of this checkout already answers /healthz on the port: starting a worker would put two on one
    #    database. start() must refuse instead of starting one. An inert stand-in serves the file `healthz`; no real worker is started.
    import socket, tempfile
    d = tempfile.mkdtemp(); open(os.path.join(d, "healthz"), "w").write("ok")
    sock = socket.socket(); sock.bind(("127.0.0.1", 0)); spare = sock.getsockname()[1]; sock.close()
    srv = subprocess.Popen([sys.executable, "-m", "http.server", str(spare), "--bind", "127.0.0.1", "--directory", d],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    try:
        for _ in range(40):
            try:
                L.urllib.request.urlopen(f"http://127.0.0.1:{spare}/healthz", timeout=1).read(); break
            except Exception:
                time.sleep(0.25)
        saved = L.API; L.API = f"http://127.0.0.1:{spare}"
        try:
            L.Worker(L.W / "worker-selftest.log").start(); refused2 = False
        except SystemExit as e:
            refused2 = "not a worker of this checkout" in str(e) and "CLEAT_PROTO_API_PORT" in str(e)
        finally:
            L.API = saved
        verdict("start() refuses when something else already answers /healthz, and says how to pick another port", refused2)
        verdict("and it started no worker of this checkout", L._mine_alive() == [])
    finally:
        srv.kill()
finally:
    for p in (mine, foreign, unrecorded):
        if p is not None and p.poll() is None:
            p.kill()
    (L.W / f"worker-{L.PORT}.pid").unlink(missing_ok=True)
bad = [r for r in res if not r]
print(f"{len(res) - len(bad)} of {len(res)} checks passed")
sys.exit(1 if bad else 0)
