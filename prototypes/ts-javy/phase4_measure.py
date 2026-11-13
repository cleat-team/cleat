#!/usr/bin/env python3
"""Phase 4, in-process half (plan section 5): cold start, replay of a 1,000-step history, peak linear memory, for the same
benchmark workflow in TypeScript (static Javy), Go, Rust and Python, on the wasmtime backend the worker uses.
The machine is shared and not quiet, so languages are INTERLEAVED across rounds (drift hits all alike) and the load average is
recorded with every sample. Raw output: evidence/phase4/inproc.json, and a table on stdout. Exit 2 if any run could not look."""
import json, os, platform, re, subprocess, sys, time
HERE = os.path.dirname(os.path.abspath(__file__)); os.chdir(HERE)
LANGS = ["ts", "go", "rust", "python"]
ROUNDS = int(os.environ.get("P4_ROUNDS", "5"))
H = ".work/bench-harness"
def sh(*a): return subprocess.run(a, capture_output=True, text=True)
def run(lang, mode, **kw):
    args = [H, "-wasm", f".work/bench/{lang}.wasm", "-lang", lang, "-mode", mode] + sum([[f"-{k}", str(v)] for k, v in kw.items()], [])
    l0 = os.getloadavg()[0]
    p = sh(*args)
    if p.returncode: print(f"UNMEASURED: {lang} {mode}: {p.stderr.strip()[:300]}"); sys.exit(2)
    d = json.loads(p.stdout); d["load1_before"] = l0; d["load1_after"] = os.getloadavg()[0]; return d

meta = {
  "date_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
  "hardware": sh("sysctl", "-n", "machdep.cpu.brand_string").stdout.strip() + f", {os.cpu_count()} cores, " + platform.platform(),
  "commit": sh("git", "rev-parse", "HEAD").stdout.strip(), "tree_dirty_outside_evidence": bool(sh("git", "status", "--porcelain", "--", ".", ":!evidence").stdout.strip()),
  "javy": sh(".tools/javy", "--version").stdout.strip(), "plugin_sha256": sh("shasum", "-a", "256", ".work/cleat-plugin.wasm").stdout.split()[0],
  "wasmtime_go": [l.split()[1] for l in open("go.mod") if "wasmtime-go" in l][0],
  "rustc": sh("rustc", "--version").stdout.strip(), "go": sh("go", "version").stdout.strip(),
  "module_bytes": {l: os.path.getsize(f".work/bench/{l}.wasm") for l in LANGS},
  "initial_memory_pages_64KiB": {l: int(re.search(r"\(memory \(;0;\) (\d+)", sh("wasm-tools", "print", f".work/bench/{l}.wasm").stdout).group(1)) for l in LANGS},
  "dynamic_mode_size_only": {"module_bytes": os.path.getsize(".work/bench/ts_dynamic_not_runnable.wasm"), "plugin_bytes": os.path.getsize(".work/cleat-plugin.wasm"), "runnable_on_stock_engine": False},
  "rounds": ROUNDS, "load1_at_start": os.getloadavg()[0],
}
res = {"meta": meta, "cold": {l: [] for l in LANGS}, "replay": {l: [] for l in LANGS}, "mem": {}}
for r in range(ROUNDS):
    for l in LANGS:
        res["cold"][l].append(run(l, "cold", samples=40 if l != "python" else 20))
        res["replay"][l].append(run(l, "replay", steps=1000, samples=20 if l != "python" else 10))
    print(f"round {r + 1}/{ROUNDS} done (load1 {os.getloadavg()[0]:.1f})", flush=True)
for l in LANGS:
    res["mem"][l] = run(l, "mem", steps=1000)
    print(f"mem {l}: <= {res['mem'][l]['peak_linear_memory_mib_upper_bound']} MiB", flush=True)
meta["load1_at_end"] = os.getloadavg()[0]
os.makedirs("evidence/phase4", exist_ok=True)
json.dump(res, open("evidence/phase4/inproc.json", "w"), indent=1, sort_keys=True)

med = lambda xs: sorted(xs)[len(xs) // 2]
print("\n(all times ms; medians of the per-round medians, and the best/worst round)")
print(f"{'lang':7s} {'bytes':>9s} | {'cold med':>9s} {'best':>7s} {'worst':>7s} {'1st+compile':>12s} | {'replay1000 med':>14s} {'best':>8s} {'worst':>8s} {'exec1000 med':>12s} | {'mem MiB<=':>9s}")
for l in LANGS:
    c = [x["cached"]["median_ms"] for x in res["cold"][l]]; rp = [x["replay"]["median_ms"] for x in res["replay"][l]]; ex = [x["execute"]["median_ms"] for x in res["replay"][l]]
    fe = [x["first_ever_ms_includes_compile"] for x in res["cold"][l]]
    print(f"{l:7s} {meta['module_bytes'][l]:9d} | {med(c):9.3f} {min(c):7.3f} {max(c):7.3f} {med(fe):12.1f} | {med(rp):14.3f} {min(rp):8.3f} {max(rp):8.3f} {med(ex):12.3f} | {res['mem'][l]['peak_linear_memory_mib_upper_bound']:9d}")
