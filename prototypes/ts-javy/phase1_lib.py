"""Shared helpers for the Phase 1 verification: a stock cleat-worker against PostgreSQL that can be SIGKILLed and
restarted, plus the few API calls the scenarios need. Python 3.9 compatible (the macOS system python)."""
import json, os, re, signal, subprocess, sys, time, urllib.request, urllib.error
from pathlib import Path

HERE = Path(__file__).resolve().parent
W = HERE / ".work"
OWNER = "postgres://postgres:postgres@127.0.0.1:54392/cleat?sslmode=disable"
APP = "postgres://cleat_app:cleat-ts-proto@127.0.0.1:54392/cleat?sslmode=disable"
PORT = 18771
API = f"http://127.0.0.1:{PORT}"
PG = "cleat-ts-pg"


def psql(sql):
    p = subprocess.run(["docker", "exec", PG, "psql", "-U", "postgres", "-d", "cleat", "-At", "-F", "\t", "-c", sql],
                       capture_output=True, text=True)
    if p.returncode != 0:
        raise SystemExit(f"psql failed: {p.stderr}")
    return p.stdout.rstrip("\n")


# Matches only workers started by THIS harness (its binary name and its API port in one command line). A second worker on the same
# database is the failure this guards against: it claims runs the first one's log never shows, so every count read from a log is
# silently short. Measured 2026-11-13: two survivors of Phase 1 scripts turned a 10-of-10 into 5-of-10 and 0-of-10.
import atexit
STALE = f"cleat-worker -db .*-api-addr :{PORT}"


def stop_stale():
    subprocess.run(["pkill", "-f", STALE], capture_output=True)
    for _ in range(20):
        if not subprocess.run(["pgrep", "-f", STALE], capture_output=True, text=True).stdout.split():
            return
        time.sleep(0.25)
    raise SystemExit("UNMEASURED: a harness worker is still alive after pkill; counts read from logs would be short")


class Worker:
    def __init__(self, log):
        self.log = Path(log)
        self.proc = None
        stop_stale()
        atexit.register(self.stop)

    def start(self):
        try:
            urllib.request.urlopen(f"{API}/healthz", timeout=1).read()
            raise SystemExit(f"UNMEASURED: something is already serving {API}; starting a worker now would share its database")
        except SystemExit:
            raise
        except Exception:
            pass
        f = open(self.log, "ab")
        f.write(f"\n=== worker start {time.strftime('%H:%M:%S')} ===\n".encode())
        f.flush()
        self.proc = subprocess.Popen(
            [str(W / "cleat-worker"), "-db", APP, "-migrate-db", OWNER, "-api-addr", f":{PORT}", "-concurrency", "4",
             "-poll", "200ms"], stdout=f, stderr=subprocess.STDOUT, start_new_session=True)
        for _ in range(60):
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
