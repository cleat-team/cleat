#!/usr/bin/env python3
"""Phase 4, database-backed half (plan section 5): the size of the stored workflow row, and end-to-end throughput of a sleep-free
workflow with ONE worker against PostgreSQL, for the same benchmark workflow in TypeScript (static Javy), Go, Rust and Python.
Run with its own database, port and container (CLEAT_PROTO_*; see phase1_lib.py). Interleaves languages across rounds.
Raw output: evidence/phase4/db.json. Exit 2 if anything could not be measured, 1 if a run did not complete correctly."""
import concurrent.futures as cf, json, os, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from phase1_lib import *

LANGS = ["ts", "go", "rust", "python"]
ROUNDS = int(os.environ.get("P4_ROUNDS", "3"))
M = {"ts": 200, "go": 200, "rust": 200, "python": 60}      # workflows per round
N = 10                                                      # recorded host calls per workflow (sideEffect), no sleep
key = KEY_FILE.read_text().strip()
inp = lambda l: {"in": {"n": N, "sleep_ms": 0}} if l == "go" else {"n": N, "sleep_ms": 0}
out = {"workflows_per_round": M, "host_calls_per_workflow": N, "rounds": ROUNDS}

# ---- 1. the stored row ------------------------------------------------------------------------------------------------------
print("deploy:")
for l in LANGS:
    print(" ", deploy(f"bench_{l}", W / "bench" / f"{l}.wasm")[:120], flush=True)
rows = psql("SELECT DISTINCT ON (name) name, version, octet_length(wasm_bytes), pg_column_size(wasm_bytes) FROM workflow_defs WHERE name LIKE 'bench\\_%' ORDER BY name, version DESC")
out["row"] = {}
print("\nstored row (workflow_defs.wasm_bytes): octet_length = bytes as deployed; pg_column_size = bytes PostgreSQL stores (TOAST, compressed)")
for line in rows.splitlines():
    name, ver, raw, stored = line.split("\t")
    out["row"][name] = {"version": int(ver), "octet_length": int(raw), "pg_column_size": int(stored), "file_bytes": os.path.getsize(W / "bench" / (name[len("bench_"):] + ".wasm"))}
    print(f"  {name:13s} v{ver} octet_length={int(raw):>9,d}  pg_column_size={int(stored):>9,d}  ratio={int(stored)/int(raw):.2f}")

# ---- 2. throughput -----------------------------------------------------------------------------------------------------------
log = W / "worker-p4.log"; log.write_bytes(b"")
w = Worker(log); w.start()
res = {l: [] for l in LANGS}
bad = []
refused = []
def one(l):
    t0 = time.time()
    def try_start(_):
        code, r = http("POST", f"/api/workflows/bench_{l}/start", key, {"input": inp(l)})
        if code in (200, 201): return r["id"]
        refused.append((l, code, str(r)[:120]))      # NEVER retried: a refusal of a deployed definition is a finding, not noise
        return None
    with cf.ThreadPoolExecutor(8) as ex:
        ids = [i for i in ex.map(try_start, range(M[l])) if i]
    t_started = time.time() - t0
    if len(ids) != M[l]: print(f"  NOTE {l}: {M[l] - len(ids)} of {M[l]} start requests were refused; measuring the {len(ids)} that started", flush=True)
    arr = "ARRAY[" + ",".join(f"'{i}'" for i in ids) + "]"
    deadline = time.time() + 300
    while time.time() < deadline:
        st = dict((r.split("\t")[0], int(r.split("\t")[1])) for r in psql(f"SELECT status, count(*) FROM workflow_leases WHERE id = ANY({arr}) GROUP BY 1").splitlines() if r)
        if sum(st.values()) == len(ids) and set(st) <= {"done", "failed", "dead_lettered", "terminated", "cancelled"}:
            break
        time.sleep(0.05)
    else:
        raise SystemExit(f"UNMEASURED: {l}: {len(ids)} workflows did not all reach a terminal state in 300 s; last={st}")
    wall = time.time() - t0
    # correctness, not only speed: every run done, and its result is the right one (sum = N)
    sample = [json.loads(get(key, i).get("result") or "{}") for i in ids[:5] + ids[-5:]]
    if st != {"done": len(ids)} or any(s.get("sum") != N for s in sample):
        bad.append((l, st, sample[:2]))
    return {"workflows": len(ids), "refused_starts": M[l] - len(ids), "wall_s": round(wall, 3), "start_phase_s": round(t_started, 3), "per_s": round(len(ids) / wall, 2), "status": st, "load1": round(os.getloadavg()[0], 1)}
print("\nthroughput (1 worker, PostgreSQL, sleep-free, 10 recorded host calls per workflow; start requests sent by 8 threads):")
for r in range(ROUNDS):
    for l in LANGS:
        res[l].append(one(l)); x = res[l][-1]
        print(f"  round {r + 1} {l:7s} {x['workflows']:4d} workflows in {x['wall_s']:7.2f} s = {x['per_s']:7.2f}/s   status={x['status']} load1={x['load1']}", flush=True)
w.kill9()
out["throughput"] = res
out["refused_starts"] = refused
# What the worker itself said while this ran (transparency about a shared, loaded machine): WARN/ERROR lines by message.
import collections
wl = collections.Counter()
for line in log.read_text(errors="replace").splitlines():
    try: d = json.loads(line)
    except Exception: continue
    if d.get("level") in ("WARN", "ERROR"): wl[(d["level"], d.get("msg", "")[:90])] += 1
out["worker_warn_error_lines"] = {f"{k[0]} {k[1]}": v for k, v in wl.items()}
print("\nworker WARN/ERROR lines during the run:", dict(wl) or "none")
os.makedirs(HERE / "evidence" / "phase4", exist_ok=True)
json.dump(out, open(HERE / "evidence" / "phase4" / "db.json", "w"), indent=1, sort_keys=True)
med = lambda xs: sorted(xs)[len(xs) // 2]
print("\nmedian of rounds (best, worst):")
for l in LANGS:
    p = [x["per_s"] for x in res[l]]
    print(f"  {l:7s} {med(p):7.2f}/s  ({max(p):.2f}, {min(p):.2f})")
if refused:
    print(f"FAIL: {len(refused)} start request(s) were refused (not retried):", refused[:5]); sys.exit(1)
if bad:
    print("FAIL: runs that did not complete correctly:", bad); sys.exit(1)
