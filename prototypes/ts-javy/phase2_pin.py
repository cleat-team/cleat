#!/usr/bin/env python3
"""Plan §4.5 "Engine version": the pin is in the deployed bytes and checkable; a bump does not move an in-flight run; replay under a
bumped engine is identical or refused, never silently different. Also `python3 phase2_pin.py check <module.wasm>`: the refusal tool."""
import hashlib, json, os, re, subprocess, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from phase1_lib import *

SECTION = b"cleat.js-engine"

def uleb(b, i):
    n = s = 0
    while True:
        c = b[i]; i += 1; n |= (c & 0x7f) << s; s += 7
        if not c & 0x80: return n, i

def read_pin(wasm: bytes):
    """The cleat.js-engine custom section as a dict, or None if the module has no such section."""
    i = 8
    while i < len(wasm):
        sid = wasm[i]; size, j = uleb(wasm, i + 1); end = j + size
        if sid == 0:
            nl, k = uleb(wasm, j)
            if wasm[k:k + nl] == SECTION: return json.loads(wasm[k + nl:end])
        i = end
    return None

def strip_pin(wasm: bytes) -> bytes:
    """The same module without the cleat.js-engine custom section: what a module built before the pin existed looks like."""
    out, i = bytearray(wasm[:8]), 8
    while i < len(wasm):
        sid = wasm[i]; size, j = uleb(wasm, i + 1); end = j + size
        if sid == 0:
            nl, k = uleb(wasm, j)
            if wasm[k:k + nl] == SECTION: i = end; continue
        out += wasm[i:end]; i = end
    return bytes(out)

def pinned_plugin_sha():
    m = re.search(r"^cleat-plugin\s+sha256 ([0-9a-f]{64})", (HERE / "toolchain.lock").read_text(), re.M)
    if not m: raise SystemExit("UNMEASURED: toolchain.lock has no pinned plugin hash")
    return m.group(1)

def engine_pin_problems(wasm: bytes):
    """[] if the module carries exactly the pinned engine; else every reason it does not. This is the refusal."""
    p = read_pin(wasm)
    if p is None: return ["no cleat.js-engine section: the module does not say which engine it carries"]
    why = []
    javy = subprocess.check_output([str(HERE / ".tools/javy"), "--version"], text=True).strip()
    if p.get("javy") != javy: why.append(f"javy {p.get('javy')!r} != pinned {javy!r}")
    if p.get("plugin_sha256") != pinned_plugin_sha(): why.append("plugin_sha256 is not the pinned plugin")
    if p.get("deterministic_build") is not True: why.append("not a deterministic build")
    return why

if len(sys.argv) == 3 and sys.argv[1] == "check":
    why = engine_pin_problems(open(sys.argv[2], "rb").read())
    print("PIN OK" if not why else "REFUSED: " + "; ".join(why)); sys.exit(1 if why else 0)

key = (W / "p1.key").read_text().strip()
res = []
def verdict(name, ok, detail=""): res.append(ok); print(f"  {'PASS' if ok else 'FAIL'}  {name}{('  ' + detail) if detail else ''}", flush=True)

def build(variant, out):
    env = {**os.environ, "PLUGIN": f".work/plugin-{variant}.wasm", "OUT_NAME": "pinprobe"}
    p = subprocess.run(["./build.sh", "nondet", "nondet"], cwd=HERE, env=env, capture_output=True, text=True)
    if p.returncode: raise SystemExit(f"UNMEASURED: build {variant}: {p.stderr[-300:]}")
    (W / out).write_bytes((W / "pinprobe.wasm").read_bytes())

print("### build three engines of the same workflow (a: shipped, b: benign bump, c: random shim removed)")
build("full", "pinprobe_a.wasm"); build("off-date", "pinprobe_b.wasm"); build("off-random", "pinprobe_c.wasm")
A, B, C = (( W / f"pinprobe_{x}.wasm").read_bytes() for x in "abc")

print("\n### T1 the pin is in the module and the check can refuse")
pa = read_pin(A); print("  pin of a:", json.dumps(pa, sort_keys=True)[:230])
verdict("a carries exactly the pinned engine", engine_pin_problems(A) == [], str(engine_pin_problems(A)))
verdict("CONTROL b (a different plugin build) is refused", engine_pin_problems(B) != [], "; ".join(engine_pin_problems(B))[:90])
verdict("CONTROL c (random shim removed) is refused", engine_pin_problems(C) != [])
tam = A.replace(pa["plugin_sha256"].encode(), ("0" * 64).encode(), 1)
verdict("CONTROL a module whose pin was edited is refused", tam != A and engine_pin_problems(tam) != [], "; ".join(engine_pin_problems(tam))[:90])
old = strip_pin(A)
verdict("CONTROL a module with no pin section (as built before the pin existed) is refused", len(old) < len(A) and read_pin(old) is None and engine_pin_problems(old) != [], "; ".join(engine_pin_problems(old))[:90])
verdict("placeholder", True)  # replaced below once deployed

log = W / "worker-p2-pin.log"; log.write_bytes(b"")
print("\n### T2 a bump does not move an in-flight run")
da = deploy("pinprobe", W / "pinprobe_a.wasm"); print(" ", da[:100])
va = int(re.search(r"pinprobe v(\d+)", da).group(1))
dep = lambda v: bytes(psql(f"select encode(wasm_bytes,'hex') from workflow_defs where name='pinprobe' and version={v}") and bytes.fromhex(psql(f"select encode(wasm_bytes,'hex') from workflow_defs where name='pinprobe' and version={v}")))
stored = dep(va)
res[-1] = read_pin(stored) == read_pin(A) and engine_pin_problems(stored) == []
print(f"  {'PASS' if res[-1] else 'FAIL'}  the pin survives deployment: the pin read back from workflow_defs (v{va}) equals the file's and passes the check (deploy re-stamps the version in cleat.metadata, so the bytes are not compared whole)")
w = Worker(log); w.start()
IN = lambda ms: {"source": "random", "sleepMs": ms}
r1 = start(key, "pinprobe", IN(25000)); r3 = start(key, "pinprobe", IN(900000))
for r in (r1, r3): wait(key, r, lambda x: x["status"] == "ready" and any(e[1] == "side_effect" for e in events(r)), 40, "suspension after the recorded side effect")
print(f"  R1 {r1[:8]} (25 s sleep) and R3 {r3[:8]} (15 min sleep) are suspended on v1; history of R3: {[e[1] for e in events(r3)]}")

print("\n### T3 replay of R3's persisted history under three engines (Go harness, wasmtime)")
env = {**os.environ, "GOWORK": "off", "GOFLAGS": "-mod=mod", "GOPROXY": "off", "REPLAY_DSN": OWNER, "REPLAY_WORKFLOW_ID": r3,
       "REPLAY_WASM_A": str(W / "pinprobe_a.wasm"), "REPLAY_WASM_B": str(W / "pinprobe_b.wasm"), "REPLAY_WASM_C": str(W / "pinprobe_c.wasm"),
       "REPLAY_ENTRY": "nondet", "REPLAY_DEF_NAME": "pinprobe", "REPLAY_DEF_VERSION": str(va), "REPLAY_INPUT": json.dumps(IN(900000))}
g = subprocess.run(["go", "test", "-count=1", "-v", "-run", "TestReplayUnderABumpedEngine", "./replay"], cwd=HERE, env=env, capture_output=True, text=True, timeout=300)
out = g.stdout + g.stderr; (HERE / "evidence" / "phase2" / "pin-replay-go-test.txt").write_text(out)
for l in out.splitlines():
    if re.search(r"replay_test.go:\d+: (loaded|a |b |c )|^--- |^ok|^FAIL", l): print("   ", l.strip()[:230])
verdict("replay under the recorded engine, a benign bump, and a behaviour-changing bump: identical, identical, REFUSED (go test passed, test ran)",
        g.returncode == 0 and "--- PASS: TestReplayUnderABumpedEngine" in out and "SKIP" not in out)

print("\n### T2 (continued) deploy the bumped engine as v2 while R1 and R3 are suspended on v1")
db_ = deploy("pinprobe", W / "pinprobe_b.wasm"); print(" ", db_[:100])
vb = int(re.search(r"pinprobe v(\d+)", db_).group(1))
r2 = start(key, "pinprobe", IN(1200))
a1 = wait(key, r1, terminal, 90, "R1"); a2 = wait(key, r2, terminal, 60, "R2")
ver = lambda wid: sorted({int(m) for l in log.read_text(errors="replace").splitlines() if wid in l for m in re.findall(r'"def_version":(\d+)', l)})
res1, res2 = json.loads(a1["result"]), json.loads(a2["result"])
verdict("R1 (started before the bump) completed", a1["status"] == "done" and res1["same"] is True, f"same={res1.get('same')}")
verdict(f"R1 ran only on def_version {va} (the engine it recorded on), across the deploy", va != vb and ver(r1) == [va], f"versions seen in the worker log: {ver(r1)}")
verdict(f"R2 (started after the bump) ran on def_version {vb}", a2["status"] == "done" and ver(r2) == [vb], f"versions: {ver(r2)}")
verdict("the two engines are different bytes (so the test could have disagreed)", A != B and read_pin(A) != read_pin(B))
w.kill9()

print("\n### T4 deploy identity: the same source, rebuilt, must not look like a new version")
db2 = None
build("off-date", "pinprobe_b2.wasm")
again = deploy("pinprobe", W / "pinprobe_b2.wasm"); print("  rebuilt b, deployed again:", again[:110])
verdict("a rebuild of unchanged source deploys as 'WASM unchanged' (no new version)", "unchanged" in again and (W / "pinprobe_b2.wasm").read_bytes() == B, again[:70])
def nd_build(out):
    env = {**os.environ, "PLUGIN": ".work/plugin-off-date.wasm", "OUT_NAME": "pinprobe", "JAVY_DETERMINISTIC": "n"}
    p = subprocess.run(["./build.sh", "nondet", "nondet"], cwd=HERE, env=env, capture_output=True, text=True)
    if p.returncode: raise SystemExit(f"UNMEASURED: build n: {p.stderr[-300:]}")
    (W / out).write_bytes((W / "pinprobe.wasm").read_bytes())
nd_build("pinprobe_n1.wasm"); n1 = deploy("pinprobe", W / "pinprobe_n1.wasm"); nd_build("pinprobe_n2.wasm"); n2 = deploy("pinprobe", W / "pinprobe_n2.wasm")
print("  CONTROL, javy build WITHOUT -C deterministic, same source, two builds, two deploys:\n   ", n1[:100], "\n   ", n2[:100])
verdict("CONTROL: without the deterministic flag the same source deploys as a NEW version each time", n1.startswith("Deployed") and n2.startswith("Deployed") and (W / "pinprobe_n1.wasm").read_bytes() != (W / "pinprobe_n2.wasm").read_bytes())
subprocess.run(["./build.sh", "nondet", "nondet"], cwd=HERE, env={**os.environ, "PLUGIN": ".work/plugin-full.wasm", "OUT_NAME": "pinprobe"}, capture_output=True)
print(f"\n{sum(res)} of {len(res)} verdicts passed")
sys.exit(0 if all(res) else 1)
