"""cleat#3870: ``--runtime wazero`` must fail, not succeed with the wrong artifact.

wazero cannot load a WASM Component Model binary, so ``--runtime wazero`` is only satisfiable by decomposing the
component into core WASM with ``wasm-tools component decompose``. With no ``wasm-tools`` at all the build already
exited 1. With a ``wasm-tools`` that has no ``decompose`` subcommand (every release checked on 2026-11-14, 1.200.0
to 1.261.0, is one) it exited 0, printed a warning that told the reader to install an older wasm-tools, and
wrote the component: a "Build SUCCESS" whose output the requested runtime cannot load.

Neither componentize-py nor wasm-tools is needed to test this. Both are stand-ins on PATH: a componentize-py
that writes a file with the component header where it is asked to, and a wasm-tools that answers like the
real one does for a subcommand it does not have. What is under test is the decision the script makes after the
compile, which does not read the file.
"""

from __future__ import annotations

import os
import stat
import subprocess
import sys
from pathlib import Path

SDK = Path(__file__).resolve().parent.parent
SCRIPT = SDK / "scripts" / "build_wasm.py"

COMPONENTIZE_PY = """#!/bin/sh
# A componentize-py that writes a file with the component header where -o says.
if [ "$1" = "--version" ]; then echo "componentize-py 0.0.0 (stand-in)"; exit 0; fi
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; fi
  shift
done
printf '\\000asm\\015\\000\\001\\000stand-in' > "$out"
"""

# What wasm-tools 1.261.0 prints for `component decompose`, measured 2026-11-14.
WASM_TOOLS_WITHOUT_DECOMPOSE = """#!/bin/sh
echo "error: unrecognized subcommand 'decompose'" >&2
echo >&2
echo "Usage: wasm-tools component <COMMAND>" >&2
exit 2
"""


def _stub(directory: Path, name: str, body: str) -> None:
    path = directory / name
    path.write_text(body)
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


def _build(tmp_path: Path, runtime: str, wasm_tools: str | None) -> subprocess.CompletedProcess[str]:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    _stub(bin_dir, "componentize-py", COMPONENTIZE_PY)
    if wasm_tools is not None:
        _stub(bin_dir, "wasm-tools", wasm_tools)
    entry = tmp_path / "wf.py"
    entry.write_text(
        "from cleat_sdk import HostCalls, cleat_entry\n\n\n"
        '@cleat_entry("wf")\ndef wf(h: HostCalls, name: str) -> dict:\n    return {"ok": True}\n'
    )
    env = dict(os.environ)
    # The stand-ins first, and nothing after them that could supply a real wasm-tools.
    env["PATH"] = f"{bin_dir}:/usr/bin:/bin"
    env["PYTHONPATH"] = str(SDK)
    return subprocess.run(
        [sys.executable, str(SCRIPT), "--entry", f"{entry}:wf", "--output", str(tmp_path / "out.wasm"),
         "--runtime", runtime],
        capture_output=True, text=True, timeout=120, env=env, cwd=str(SDK),
    )


def test_wazero_fails_when_wasm_tools_cannot_decompose(tmp_path: Path) -> None:
    r = _build(tmp_path, "wazero", WASM_TOOLS_WITHOUT_DECOMPOSE)
    assert r.returncode != 0, f"exited 0 with the wrong artifact:\n{r.stdout}\n{r.stderr}"
    assert "--runtime wazero needs core WASM" in r.stderr, r.stderr
    assert "--runtime wasmtime" in r.stderr, "the message must name the way out that works"
    assert "older wasm-tools" not in r.stderr, "advice that was not found to work must not come back"


def test_wazero_still_fails_when_wasm_tools_is_absent(tmp_path: Path) -> None:
    """The other row of the same table, which already failed: the two now answer alike."""
    r = _build(tmp_path, "wazero", None)
    assert r.returncode != 0, r.stdout + r.stderr
    assert "wasm-tools not found" in r.stderr, r.stderr


def test_wasmtime_is_not_asked_to_decompose(tmp_path: Path) -> None:
    """The control: the same stand-in wasm-tools is not an error for a runtime that does not need core WASM."""
    r = _build(tmp_path, "wasmtime", WASM_TOOLS_WITHOUT_DECOMPOSE)
    assert "needs core WASM" not in r.stderr, r.stderr
    assert "Decompose skipped" in r.stdout, r.stdout
