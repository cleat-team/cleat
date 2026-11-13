#!/usr/bin/env python3
"""Plan section 7: one smoke test on MySQL and one on SQL Server, enough to show nothing in the TypeScript guest depends on the dialect.

The `journey` workflow (a sleep, a signal wait and a child workflow that itself sleeps) runs on a stock cleat-worker against each
database, with the worker killed (SIGKILL) while the run is suspended at the signal wait and restarted before the signal is sent. That is
the shape PHASE1.md's Q2 used on PostgreSQL, so a pass here says the same suspend/replay/resume path works on the other storage.

It is a SMOKE test: one run per dialect, not a measurement. Everything else in this prototype is PostgreSQL.

  usage: phase4_dialects.py mysql|mssql      (starts and removes its OWN container; refuses to touch any other)
  env:   CLEAT_PROTO_BIN=<dir holding cleat-worker and cleatctl, built from the committed head>  CLEAT_PROTO_MODULES=<dir holding journey.wasm, sleeper.wasm>
  exit:  0 pass; 1 a check failed; 2 UNMEASURED (the setup could not run, which is not a finding about the prototype)

A pass means: the run completes, with the signal payload we sent, the child's result, and a worker killed in between. A control makes the
check able to fail: the run is also checked while NO signal has been sent, and must still be suspended (a harness that reported 'done'
there would be reporting a run that never waited).
"""
import json, os, shutil, signal, subprocess, sys, time, urllib.request
from pathlib import Path

BIN = Path(os.environ.get("CLEAT_PROTO_BIN", "/private/tmp/claude-501/dialect"))
MODS = Path(os.environ.get("CLEAT_PROTO_MODULES", Path(__file__).parent / ".work"))
PORT = int(os.environ.get("CLEAT_PROTO_API_PORT", "18795"))
API = f"http://127.0.0.1:{PORT}"
OUT = Path(__file__).parent / "evidence" / "phase4"
LOG = OUT / "dialect-worker"


def unmeasured(msg):
    print(f"UNMEASURED: {msg}\n  (this is a failure of the setup, not a finding about the prototype)", file=sys.stderr)
    sys.exit(2)


def sh(*a, **k):
    return subprocess.run(a, capture_output=True, text=True, **k)


def free_gib():
    s = os.statvfs("/System/Volumes/Data")
    return s.f_bavail * s.f_frsize / 2**30


def http(method, path, body=None):
    req = urllib.request.Request(API + path, method=method, data=None if body is None else json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:300]


def worker_pids():
    out = sh("ps", "-axo", "pid=,command=").stdout.splitlines()
    return [int(l.split(None, 1)[0]) for l in out if str(BIN / "cleat-worker") + " -driver " in l]


def stop_worker():
    for p in worker_pids():
        os.kill(p, signal.SIGKILL)
    time.sleep(0.5)


def start_worker(dsn, n):
    LOG.mkdir(parents=True, exist_ok=True)
    try:
        urllib.request.urlopen(API + "/healthz", timeout=1)
        unmeasured(f"something already serves {API}; set CLEAT_PROTO_API_PORT")
    except SystemExit:
        raise
    except Exception:
        pass
    f = open(LOG / f"{DIALECT}-{n}.log", "wb")
    subprocess.Popen([str(BIN / "cleat-worker"), "-driver", DIALECT, "-db", dsn, "-api-addr", f":{PORT}", "-require-auth=false", "-poll", "200ms",
                      "-concurrency", "4"], stdout=f, stderr=f)
    for _ in range(100):
        try:
            urllib.request.urlopen(API + "/healthz", timeout=1)
            return
        except Exception:
            time.sleep(0.3)
    unmeasured(f"the worker did not come up; see {LOG}/{DIALECT}-{n}.log")


def wait(pred, secs, what):
    end = time.time() + secs
    while time.time() < end:
        v = pred()
        if v:
            return v
        time.sleep(0.3)
    raise SystemExit(f"FAIL: timed out waiting for {what}")


def main():
    global DIALECT
    if len(sys.argv) != 2 or sys.argv[1] not in ("mysql", "mssql"):
        sys.exit(__doc__)
    DIALECT = sys.argv[1]
    for b in ("cleat-worker", "cleatctl"):
        if not (BIN / b).exists():
            unmeasured(f"{BIN / b} is missing")
    for m in ("journey.wasm", "sleeper.wasm"):
        if not (MODS / m).exists():
            unmeasured(f"{MODS / m} is missing")
    if free_gib() < 2.5:
        unmeasured(f"only {free_gib():.1f} GiB free on the host disk; a database container would fill it and take other sessions' containers with it")
    name = "cleat-ts-my" if DIALECT == "mysql" else "cleat-ts-ms"
    if sh("docker", "ps", "-aq", "-f", f"name=^{name}$").stdout.strip():
        unmeasured(f"a container named {name} already exists; it is not created by this run, so it is left alone")
    if DIALECT == "mysql":
        img = ["mirror.gcr.io/library/mysql:8.4.11", "--innodb-buffer-pool-size=128M", "--performance-schema=OFF"]
        env, port = ["-e", "MYSQL_ROOT_PASSWORD=cleat", "-e", "MYSQL_DATABASE=cleat"], "3306"
    else:
        img = ["mcr.microsoft.com/mssql/server:2022-latest"]
        env, port = ["-e", "ACCEPT_EULA=Y", "-e", "MSSQL_SA_PASSWORD=CleatTest123!"], "1433"
    r = sh("docker", "run", "-d", "--name", name, "-p", f"127.0.0.1:0:{port}", *env, *img)
    if r.returncode:
        unmeasured(f"docker run failed: {r.stderr.strip()[-200:]}")
    try:
        hostport = sh("docker", "port", name, port).stdout.strip().splitlines()[0].rsplit(":", 1)[1]
        if DIALECT == "mysql":
            # The image starts a TEMPORARY server to run its init, stops it, and starts the real one; a ping in between succeeds and the
            # next connection is reset. Wait for the init process to say it is done, then for the real server.
            wait(lambda: "MySQL init process done" in sh("docker", "logs", name).stderr + sh("docker", "logs", name).stdout, 240, "mysql init")
            wait(lambda: sh("docker", "exec", name, "mysqladmin", "-uroot", "-pcleat", "ping").stdout.find("alive") >= 0 and
                 sh("docker", "exec", name, "mysql", "-uroot", "-pcleat", "-N", "-e", "select 1", "cleat").stdout.strip() == "1", 240, "mysql")
            dsn = f"root:cleat@tcp(127.0.0.1:{hostport})/cleat?parseTime=true&multiStatements=true&tls=false"
        else:
            sq = ["docker", "exec", name, "/opt/mssql-tools18/bin/sqlcmd", "-C", "-S", "localhost", "-U", "sa", "-P", "CleatTest123!"]
            wait(lambda: subprocess.run(sq + ["-Q", "select 1"], capture_output=True).returncode == 0, 300, "sql server")
            subprocess.run(sq + ["-Q", "IF DB_ID('cleat') IS NULL CREATE DATABASE cleat"], check=True, capture_output=True)
            dsn = f"sqlserver://sa:CleatTest123!@127.0.0.1:{hostport}?database=cleat"
        print(f"## {DIALECT}: database up on 127.0.0.1:{hostport}; free disk {free_gib():.1f} GiB")

        stop_worker()
        m = subprocess.run([str(BIN / "cleat-worker"), "-driver", DIALECT, "-db", dsn, "-migrate-only"], capture_output=True, text=True)
        print(f"## migrate-only rc={m.returncode} {(m.stdout + m.stderr).strip().splitlines()[-1][:140] if (m.stdout + m.stderr).strip() else ''}")
        if m.returncode:
            unmeasured(f"migration failed on {DIALECT}: {(m.stdout + m.stderr).strip()[-300:]}")
        start_worker(dsn, 1)
        # `cleatctl deploy workflow` is refused on MySQL and SQL Server (not ported, cleat#1316), so the definitions go in through the
        # worker's own API, which is the same store code on every dialect.
        import base64
        for n, f in (("sleeper", "sleeper.wasm"), ("journey", "journey.wasm")):
            code, r = http("POST", "/api/definitions", {"name": n, "wasm_bytes_base64": base64.b64encode((MODS / f).read_bytes()).decode()})
            print(f"## deploy {n} -> {code} {json.dumps(r)[:140]}")
            if code not in (200, 201):
                unmeasured(f"deploy of {n} answered {code} on {DIALECT}")
        code, r = http("POST", "/api/workflows/journey/start", {"input": {"sleepMs": 1500, "childMs": 1500}})
        if code not in (200, 201):
            raise SystemExit(f"FAIL: start answered {code} {r}")
        wid = r["id"]

        def waiting():
            c, g = http("GET", f"/api/workflows/{wid}")
            return g if c == 200 and g.get("status") != "done" else None
        # past the 1.5 s sleep, now suspended at the signal wait: the run is NOT done, and has recorded events
        time.sleep(4)
        c, g = http("GET", f"/api/workflows/{wid}")
        checks = []

        def check(name, ok, detail=""):
            checks.append(ok)
            print(f"  {'PASS' if ok else 'FAIL'}  {name}  {detail}")
        check("control: with no signal sent, the run is still suspended (not done, not failed)", c == 200 and g.get("status") not in ("done", "failed"), f"status={g.get('status')}")

        print("## kill -9 the worker while the run is suspended at the signal wait, then restart it")
        stop_worker()
        check("the worker is gone", not worker_pids())
        start_worker(dsn, 2)
        c, g = http("GET", f"/api/workflows/{wid}")
        check("after the restart the run is still suspended", c == 200 and g.get("status") not in ("done", "failed"), f"status={g.get('status')}")

        payload = f"hello-from-{DIALECT}"
        c, r = http("POST", f"/api/workflows/{wid}/signal", {"signal_name": "go", "payload": payload})
        check("the signal is accepted", c == 200, f"{c} {r}")
        done = wait(lambda: (lambda x: x[1] if x[0] == 200 and x[1].get("status") in ("done", "failed") else None)(http("GET", f"/api/workflows/{wid}")), 90, "completion")
        res = json.loads(done["result"]) if done.get("status") == "done" and done.get("result") else None
        check("the run completes", done.get("status") == "done", f"status={done.get('status')} error={done.get('error', '')[:120]}")
        check("it received the signal payload we sent", bool(res) and res.get("signal") == payload, f"signal={res and res.get('signal')!r}")
        check("the child workflow ran and returned", bool(res) and res.get("child", {}).get("slept") == 1500, f"child={res and res.get('child')}")
        check("uuid, random and now were produced", bool(res) and bool(res.get("uuid")) and (isinstance(res.get("random"), (int, float)) or str(res.get("random", "")).lstrip("-").isdigit()) and res.get("now") is True)
        print(f"## result {json.dumps(res)[:300]}")
        ok = all(checks)
        print(f"\n{sum(checks)} of {len(checks)} checks passed on {DIALECT}")
        return 0 if ok else 1
    finally:
        stop_worker()
        sh("docker", "rm", "-f", name)
        print(f"## removed container {name}; free disk {free_gib():.1f} GiB")


if __name__ == "__main__":
    sys.exit(main())
