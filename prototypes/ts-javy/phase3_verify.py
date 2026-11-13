#!/usr/bin/env python3
"""Phase 3 / Q4: build the Go, Rust and TypeScript place_order modules, then run the cross-language harness (xlang/) and check not
only that it passed but that it measured what it claims: every count below is a size the test design fixes in advance, so a
harness that quietly ran fewer cases cannot pass."""
import os, re, subprocess, sys
from pathlib import Path
HERE = Path(__file__).resolve().parent
EV = HERE / "evidence" / "phase3"; EV.mkdir(parents=True, exist_ok=True)
clean = {k: v for k, v in os.environ.items() if k not in ("GOWORK", "GOFLAGS", "GOPROXY")}   # the build script sets its own, per step

b = subprocess.run([str(HERE / "phase3_build.sh")], cwd=HERE, env=clean, capture_output=True, text=True)
(EV / "build.txt").write_text(b.stdout + b.stderr)
if b.returncode:
    print("UNMEASURED: phase3_build.sh failed\n" + (b.stdout + b.stderr)[-600:]); sys.exit(2)

env = {**os.environ, "GOWORK": "off", "GOFLAGS": "-mod=mod", "GOPROXY": "off", "XLANG_DIR": str(HERE / ".work" / "xlang")}
g = subprocess.run(["go", "test", "-count=1", "-timeout", "300s", "-v", "./xlang"], cwd=HERE, env=env, capture_output=True, text=True)
out = g.stdout + g.stderr
(EV / "xlang-go-test.txt").write_text(out)

want = [
    ("eight tests passed", len(re.findall(r"^--- PASS: Test", out, re.M)), 8),
    ("no test failed", len(re.findall(r"^--- FAIL", out, re.M)), 0),
    ("no test skipped (a skip prints ok and measures nothing)", len(re.findall(r"^--- SKIP|UNMEASURED", out, re.M)), 0),
    ("nine place_order exec->replay pairs bit-identical (3 languages x 3, incl. the same-language controls)", out.count("bit-identical=true"), 9),
    ("none of them differed", out.count("bit-identical=false"), 0),
    ("six tampered place_order histories refused (2 histories x 3 targets)", len(re.findall(r"payments.Charge -> payments.Refund, replayed on \S+\s*: refused=true", out)), 6),
    ("none accepted", len(re.findall(r"payments.Charge -> payments.Refund, replayed on \S+\s*: refused=false", out)), 0),
    ("fifteen canonical-JSON cases agree with Go's encoding/json", out.count("hostMatchesGo=true"), 15),
    ("none disagreed", out.count("hostMatchesGo=false"), 0),
    ("suspension events: both complete histories cross (go->ts, ts->go) to the same bytes", len(re.findall(r"history -> \S+\s+replay: err=<nil> suspended=\"\" result=\{\"signal\":\"hello\",\"slept\":1500\}", out)), 2),
    ("suspension events: both languages' runs suspended at the signal wait and RESUMED in the other", len(re.findall(r"resumed in \S+\s*: err=<nil> suspended=\"\" result=\{\"signal\":\"hello\",\"slept\":1500\}", out)), 2),
    ("suspension events: four tampered child_workflow histories refused as divergences", len(re.findall(r"child_workflow -> signal_received, replayed on \S+\s*: refused=true", out)), 4),
    ("sleep records no event in either language", len(re.findall(r"with a 300 ms sleep: suspended=\"cleat_sleep\(300ms\)\" events=0", out)), 2),
    ("the Go NaN workflow failed and the TS one returned null", int("json: unsupported value: NaN" in out and 'result="{\\"v\\":null}"' in out), 1),
]
bad = 0
for name, got, exp in want:
    ok = got == exp; bad += not ok
    print(f"  {'PASS' if ok else 'FAIL'}  {name}: {got}" + ("" if ok else f" (expected {exp})"))
m = re.search(r"cases where raw JavaScript JSON.stringify differs from the canonical form: (\[.*\])", out)
print("  INFO  raw JS JSON.stringify differs from the canonical form in:", m.group(1) if m else "UNKNOWN")
print(f"  INFO  go test rc={g.returncode}")
sys.exit(0 if (not bad and g.returncode == 0) else 1)
