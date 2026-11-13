"""Shared helpers for the Phase 1 verification: a stock cleat-worker against PostgreSQL that can be SIGKILLed and
restarted, plus the few API calls the scenarios need. Python 3.9 compatible (the macOS system python)."""
import json, os, re, signal, subprocess, sys, time, urllib.request, urllib.error
from pathlib import Path

HERE = Path(__file__).resolve().parent
W = HERE / ".work"
# Everything an instance of this harness shares with anything else on the machine is configurable, because two copies of it (or a
# reviewer re-deriving a checkpoint) must be able to run at the same time. Defaults are this prototype's original values.
#   CLEAT_PROTO_API_PORT        the worker's API port                      (18771)
#   CLEAT_PROTO_PG_CONTAINER    docker container with psql, for event_history reads   (cleat-ts-pg)
#   CLEAT_PROTO_PG_HOST/_PORT/_DB   the PostgreSQL instance and database   (127.0.0.1 / 54392 / cleat)
#   CLEAT_PROTO_OWNER_DSN / CLEAT_PROTO_APP_DSN   override the DSNs outright
#   CLEAT_PROTO_KEY_FILE        the default-tenant API key for that database   (.work/p1.key)
# To run beside someone else's run, give yours its OWN database as well as its own port: every worker polling a database claims every
# run in it, so a second port on a shared database still crosses the two runs. `provision-db.sh` creates one.
# The database must already have the cleat_app role and a default-tenant API key in CLEAT_PROTO_KEY_FILE (see PHASE0.md; provision-db.sh does both for a new database); a different
# database needs those created first.
_e = os.environ.get
PG_HOST, PG_PORT, PG_DB = _e("CLEAT_PROTO_PG_HOST", "127.0.0.1"), _e("CLEAT_PROTO_PG_PORT", "54392"), _e("CLEAT_PROTO_PG_DB", "cleat")
OWNER = _e("CLEAT_PROTO_OWNER_DSN") or f"postgres://postgres:postgres@{PG_HOST}:{PG_PORT}/{PG_DB}?sslmode=disable"
APP = _e("CLEAT_PROTO_APP_DSN") or f"postgres://cleat_app:cleat-ts-proto@{PG_HOST}:{PG_PORT}/{PG_DB}?sslmode=disable"
PORT = int(_e("CLEAT_PROTO_API_PORT", "18771"))
API = f"http://127.0.0.1:{PORT}"
PG = _e("CLEAT_PROTO_PG_CONTAINER", "cleat-ts-pg")
KEY_FILE = Path(_e("CLEAT_PROTO_KEY_FILE") or (W / "p1.key"))   # the default-tenant API key for THIS database


def psql(sql):
    p = subprocess.run(["docker", "exec", PG, "psql", "-U", "postgres", "-d", PG_DB, "-At", "-F", "\t", "-c", sql],
                       capture_output=True, text=True)
    if p.returncode != 0:
        raise SystemExit(f"psql failed: {p.stderr}")
    return p.stdout.rstrip("\n")


# Stopping stale workers, safely. A worker belongs to this harness only if (1) a pidfile in THIS checkout's .work/ names its pid and
# (2) that pid's command line contains THIS checkout's worker binary path. Never "whatever is on the port": a second copy of the
# harness, or a reviewer's run, can legitimately have its own worker on another port, or even on this one, and must not be killed.
# (A second worker sharing the database is still the failure this guards against: it claims runs whose log lines go to a file the
# first one's counts never read. Measured 2026-11-13: it turned a 10-of-10 into 5-of-10 and 0-of-10.)
import atexit
BIN = str(W / "cleat-worker")


def _cmd(pid):
    return subprocess.run(["ps", "-p", str(pid), "-o", "command="], capture_output=True, text=True).stdout.strip()


def _mine_alive():
    """Pids of live processes running THIS checkout's worker binary (path-scoped, not port-scoped)."""
    out = subprocess.run(["ps", "-axo", "pid,command"], capture_output=True, text=True).stdout.splitlines()
    return [int(l.split(None, 1)[0]) for l in out if BIN + " -db " in l]


def stop_stale():
    for pf in W.glob("worker-*.pid"):
        try:
            pid = int(pf.read_text().strip())
        except ValueError:
            pf.unlink(missing_ok=True); continue
        if BIN in _cmd(pid):
            try:
                os.killpg(os.getpgid(pid), signal.SIGKILL)
            except (ProcessLookupError, PermissionError):
                pass
        pf.unlink(missing_ok=True)
    for _ in range(20):
        if not _mine_alive():
            return
        time.sleep(0.25)
    raise SystemExit(f"UNMEASURED: a worker of this checkout is still alive ({_mine_alive()}); counts read from logs would be short")


class Worker:
    def __init__(self, log):
        self.log = Path(log)
        self.proc = None
        stop_stale()
        atexit.register(self.stop)

    def start(self):
        try:
            urllib.request.urlopen(f"{API}/healthz", timeout=1).read()
            raise SystemExit(f"UNMEASURED: something is already serving {API}, and it is not a worker of this checkout; set CLEAT_PROTO_API_PORT to another port instead of starting a second worker on one database")
        except SystemExit:
            raise
        except Exception:
            pass
        f = open(self.log, "ab")
        f.write(f"\n=== worker start {time.strftime('%H:%M:%S')} ===\n".encode())
        f.flush()
        self.proc = subprocess.Popen(
            [BIN, "-db", APP, "-migrate-db", OWNER, "-api-addr", f":{PORT}", "-concurrency", "4",
             "-poll", "200ms"], stdout=f, stderr=subprocess.STDOUT, start_new_session=True)
        for _ in range(60):
            (W / f"worker-{PORT}.pid").write_text(str(self.proc.pid))
            if self.proc.poll() is not None:
                raise SystemExit(f"UNMEASURED: the worker exited at start (rc={self.proc.returncode}); see {self.log}")
            try:
                urllib.request.urlopen(f"{API}/healthz", timeout=2).read()
                return
            except Exception:
                time.sleep(1)
        raise SystemExit("worker never became healthy")

    def kill9(self):
        try:
            os.killpg(self.proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        self.proc.wait(timeout=20)

    def stop(self):
        """Idempotent cleanup, registered with atexit so a driver that fails still leaves no worker behind."""
        if self.proc is not None and self.proc.poll() is None:
            self.kill9()
        (W / f"worker-{PORT}.pid").unlink(missing_ok=True)


def http(method, path, key, body=None, timeout=15):
    req = urllib.request.Request(API + path, method=method, data=None if body is None else json.dumps(body).encode(),
                                 headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:300]


def start(key, name, inp):
    code, r = http("POST", f"/api/workflows/{name}/start", key, {"input": inp})
    assert code in (200, 201), (code, r)
    return r["id"]


def get(key, wid):
    code, r = http("GET", f"/api/workflows/{wid}", key)
    assert code == 200, (code, r)
    return r


def wait(key, wid, pred, timeout=60, what=""):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        last = get(key, wid)
        if pred(last):
            return last
        time.sleep(0.25)
    raise SystemExit(f"timed out waiting for {what or 'condition'} on {wid}; last={json.dumps({k: last.get(k) for k in ('status','result','error')})}")


def terminal(r):
    return r["status"] in ("done", "failed", "dead_lettered", "terminated", "cancelled")


def history_rows(wid):
    """Every column of every event_history row for the run, in step order, as one comparable text block."""
    return psql(f"SELECT row_to_json(e)::text FROM (SELECT * FROM event_history WHERE workflow_id = '{wid}' ORDER BY step) e")


def events(wid):
    out = psql(f"SELECT step, event_type, coalesce(service,''), coalesce(operation,''), coalesce(signal_name,''), coalesce(child_name,'') "
               f"FROM event_history WHERE workflow_id = '{wid}' ORDER BY step")
    return [tuple((l.split("\t") + [""] * 6)[:6]) for l in out.splitlines() if l]


def deploy(name, wasm):
    p = subprocess.run([str(W / "cleatctl"), "deploy", "workflow", name, str(wasm)], capture_output=True, text=True,
                       env={**os.environ, "CLEAT_DB_URL": OWNER})
    return (p.stdout + p.stderr).strip().splitlines()[-1]


def trace_count(logs, wid, needle):
    n = 0
    for lg in logs:
        for line in Path(lg).read_text(errors="replace").splitlines():
            if needle in line and wid in line:
                n += 1
    return n


if __name__ == "__main__":   # shell hooks: lib.py stop | leak | owner-dsn | settings
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    if cmd == "stop":
        stop_stale()
    elif cmd == "leak":
        left = _mine_alive(); print(" ".join(map(str, left))); sys.exit(1 if left else 0)
    elif cmd == "owner-dsn":
        print(OWNER)
    elif cmd == "settings":
        print(f"api port {PORT}; postgres {PG_HOST}:{PG_PORT}/{PG_DB}; container {PG}; worker binary {BIN}")
    else:
        sys.exit("usage: phase1_lib.py stop|leak|owner-dsn|settings")
