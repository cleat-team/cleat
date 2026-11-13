#!/usr/bin/env python3
"""Phase 5 / Q7: the developer's flow through `cleat build --target ts`, for a workflow that lives OUTSIDE the repository.
Build it with the CLI, deploy it, run it with valid and invalid input; the invalid run's error must name the user's own .ts file and
line. Refusals are exercised too: malformed --entry, a source the static check refuses (no artifact may be emitted), and a toolchain
that is not built (exit 2, UNMEASURED, never a pass). Raw: evidence/phase5/cli.txt (stdout). Exit 0 only if every verdict passes."""
import json, os, re, shutil, subprocess, sys, tempfile
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from phase1_lib import *

ROOT = HERE.parent.parent
env_nogo = {k: v for k, v in os.environ.items() if k not in ("GOWORK", "GOFLAGS", "GOPROXY")}
res = []
def verdict(name, ok, detail=""): res.append(ok); print(f"  {'PASS' if ok else 'FAIL'}  {name}{('  ' + detail) if detail else ''}", flush=True)

b = subprocess.run(["go", "build", "-o", str(W / "cleat"), "./cmd/cleat"], cwd=ROOT, env=env_nogo, capture_output=True, text=True)
if b.returncode: raise SystemExit(f"UNMEASURED: go build ./cmd/cleat failed: {b.stderr[-300:]}")
CLEAT = str(W / "cleat")
def cli(*args, cwd=None, env=None):
    p = subprocess.run([CLEAT, *args], capture_output=True, text=True, cwd=cwd, env={**os.environ, **(env or {})})
    return p.returncode, p.stdout + p.stderr

U = Path(tempfile.mkdtemp(prefix="cleat-userwf-")); (U / "src").mkdir(); OUT = U / "out"
SRC = U / "src" / "orders.ts"
SRC.write_text('''import { workflow } from "@cleat/ts";
import { z } from "zod";

const Order = z.object({ sku: z.string(), qty: z.number().int().positive() });

export const checkOrder = workflow<unknown, { ok: boolean; qty: number }>(async (_ctx, input) => {
  if (typeof input !== "object") throw new Error("order must be an object"); // MARK:own-throw
  const o = Order.parse(input);
  return { ok: true, qty: o.qty };
});
''')
own_line = next(i + 1 for i, l in enumerate(SRC.read_text().splitlines()) if "MARK:own-throw" in l)
try:
    print(f"user workflow: {SRC} (outside {ROOT})")
    rc, out = cli("build", "--target", "ts", "--entry", f"{SRC}:check_order", "-o", str(OUT), cwd=U)
    print(out.strip().splitlines()[-1] if out.strip() else "")
    verdict("cleat build --target ts builds a workflow outside the repository (exit 0, module written to -o)", rc == 0 and (OUT / "orders.wasm").exists(), f"rc={rc}")
    verdict("the module carries the javascript language label and the engine pin", b"cleat.metadata" in (OUT / "orders.wasm").read_bytes() and b"javascript" in (OUT / "orders.wasm").read_bytes() and b"cleat.js-engine" in (OUT / "orders.wasm").read_bytes())

    key = KEY_FILE.read_text().strip()
    print(" ", deploy("orders", OUT / "orders.wasm")[:100], flush=True)
    log = W / "worker-p5-cli.log"; log.write_bytes(b""); w = Worker(log); w.start()
    ok_run = wait(key, start(key, "orders", {"sku": "A-1", "qty": 3}), terminal, 60, "valid run")
    verdict("a valid order completes with the right result", ok_run["status"] == "done" and json.loads(ok_run["result"]) == {"ok": True, "qty": 3}, str(ok_run.get("result")))
    bad = wait(key, start(key, "orders", {"sku": "A-1", "qty": -2}), terminal, 60, "invalid run")
    verdict("an invalid order fails (zod refuses it)", bad["status"] == "failed", (bad.get("error") or "")[:80])
    err = bad.get("error") or ""
    m = re.search(r"at \S+ \((src/orders\.ts):(\d+):(\d+)\)", err)
    verdict("the error names the user's own file and a line in it, relative to the source root", bool(m), m.group(0) if m else err[-200:])
    own = wait(key, start(key, "orders", "not an object"), terminal, 60, "own throw")
    verdict(f"a throw in the user's code names src/orders.ts:{own_line}, as the whole path (nothing before it)", re.search(rf"\((src/orders\.ts):{own_line}:\d+\)", own.get("error") or "") is not None, (own.get("error") or "")[-160:].replace("\n", " | "))
    verdict("no bundle position (function.mjs) leaks into either error", "function.mjs" not in err and "function.mjs" not in (own.get("error") or ""))
    w.kill9()

    # ---- refusals ----
    rc, out = cli("build", "--target", "ts", "--entry", f"{SRC}:Check-Order", "-o", str(U / "o2"), cwd=U)
    verdict("a malformed entry name is refused (exit 1) before any build", rc == 1 and not (U / "o2").exists(), out.strip()[:90])
    bad_src = U / "src" / "timers.ts"
    bad_src.write_text('import { workflow } from "@cleat/ts";\nexport const t = workflow(async () => { setTimeout(() => {}, 1); });\n')
    rc, out = cli("build", "--target", "ts", "--entry", f"{bad_src}:t", "-o", str(U / "o3"), cwd=U)
    verdict("a source the static check refuses exits 1 and emits NO artifact", rc == 1 and not (U / "o3" / "timers.wasm").exists() and "TS001" in out, f"rc={rc}")
    empty = Path(tempfile.mkdtemp(prefix="cleat-notool-")); (empty / "build.sh").write_text("#!/bin/bash\nexit 0\n")
    rc, out = cli("build", "--target", "ts", "--entry", f"{SRC}:check_order", "-o", str(U / "o4"), cwd=U, env={"CLEAT_TS_DIR": str(empty)})
    verdict("an unbuilt toolchain is UNMEASURED (exit 2) and says what is missing", rc == 2 and "UNMEASURED" in out and "javy" in out and not (U / "o4").exists(), f"rc={rc}")
    shutil.rmtree(empty)
finally:
    shutil.rmtree(U, ignore_errors=True)
print(f"\n{sum(res)} of {len(res)} verdicts passed"); sys.exit(0 if all(res) else 1)
