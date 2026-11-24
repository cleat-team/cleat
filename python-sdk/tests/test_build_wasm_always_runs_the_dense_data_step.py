"""Every ``build_wasm.py`` build runs the dense-data step, whatever the step decides (cleat#4382, cleat-review2's mutant P1).

The step rewrites the CPython module's data layout so wasmtime builds a shared memory image for it. Whether it has anything to do depends on componentize-py's
output: its layout sits within a point of wasmtime's 50% line and moves with the build environment (51.1% in one image, 49% in another). So a build that
never CALLS the step is indistinguishable, on a build whose layout happens to be dense already, from one that called it and found nothing to do: a real-build
test goes green either way, and only a toolchain that lands under 50% would notice. This pins the wiring itself, on components this test writes, so it does
not depend on the layout componentize-py produces: the script is driven with a componentize-py stand-in that emits a chosen component, and each outcome
(merged, not needed, not rewritten) must leave its one ``Dense data:`` line AND the file in the state that outcome implies.

Known positive: with the call to ``densify_output`` removed from the script, all three tests fail.
"""

from __future__ import annotations

import importlib.util
import os
import stat
import subprocess
import sys
from pathlib import Path

SDK = Path(__file__).resolve().parent.parent
SCRIPT = SDK / "scripts" / "build_wasm.py"
_spec = importlib.util.spec_from_file_location("build_wasm_under_test", SCRIPT)
bw = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(bw)

MIB = 1 << 20
MARK = "Dense data:"

# A componentize-py that copies the component named by $STANDIN_COMPONENT to where -o says.
STAND_IN = f"""#!/bin/sh
if [ "$1" = "--version" ]; then echo "componentize-py {bw.PINNED_COMPONENTIZE_PY}"; exit 0; fi
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; fi
  shift
done
cp "$STANDIN_COMPONENT" "$out"
"""


def _section(sid: int, payload: bytes) -> bytes:
    return bytes([sid]) + bw._uleb(len(payload)) + payload


def _segment(offset: int, data: bytes) -> bytes:
    return bw._uleb(0) + b"\x41" + bw._sleb(offset) + b"\x0b" + bw._uleb(len(data)) + data


def _module(segments: list[tuple[int, bytes]]) -> bytes:
    body = bw._uleb(len(segments)) + b"".join(_segment(o, d) for o, d in segments)
    return (
        b"\x00asm\x01\x00\x00\x00"
        + _section(1, b"\x01\x60\x00\x00")
        + _section(5, b"\x01\x00" + bw._uleb(400))
        + _section(12, bw._uleb(len(segments)))
        + _section(11, body)
    )


def _component(module: bytes) -> bytes:
    return b"\x00asm\x0d\x00\x01\x00" + _section(1, module)


SPARSE = _component(_module([(1024 + i * 40_000, bytes([i % 251 + 1]) * 12_000) for i in range(600)] + [(22 * MIB, b"\xaa" * 28_000)]))
DENSE = _component(_module([(0, b"\x07" * (4 * MIB))]))
# A passive data segment (flag 1) is a shape the rewrite does not model.
UNPARSEABLE = _component(
    b"\x00asm\x01\x00\x00\x00" + _section(11, bw._uleb(1) + bw._uleb(1) + bw._uleb(4) + b"abcd")
)


def _build(tmp_path: Path, component: bytes) -> tuple[subprocess.CompletedProcess[str], bytes]:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    stub = bin_dir / "componentize-py"
    stub.write_text(STAND_IN)
    stub.chmod(stub.stat().st_mode | stat.S_IXUSR)
    src = tmp_path / "standin.wasm"
    src.write_bytes(component)
    entry = tmp_path / "wf.py"
    entry.write_text(
        "from cleat_sdk import HostCalls, cleat_entry\n\n\n"
        '@cleat_entry("wf")\ndef wf(h: HostCalls, name: str) -> dict:\n    return {"ok": True}\n'
    )
    env = dict(os.environ, PATH=f"{bin_dir}:/usr/bin:/bin", PYTHONPATH=str(SDK), STANDIN_COMPONENT=str(src))
    out = tmp_path / "out.wasm"
    r = subprocess.run(
        [sys.executable, str(SCRIPT), "--entry", f"{entry}:wf", "--output", str(out), "--runtime", "wasmtime"],
        capture_output=True, text=True, timeout=120, env=env, cwd=str(SDK),
    )
    assert out.exists(), "the build left no output:\n" + r.stdout + r.stderr
    return r, out.read_bytes()


def _status_lines(r: subprocess.CompletedProcess[str]) -> list[str]:
    return [line.strip() for line in (r.stdout + r.stderr).splitlines() if line.strip().startswith(MARK)]


def _segment_count(component: bytes) -> int:
    for sid, a, b in bw._wasm_sections(component):
        if sid == 1:
            module = component[a:b]
            for s2, a2, b2 in bw._wasm_sections(module):
                if s2 == 11:
                    return len(bw._parse_data_segments(module[a2:b2]))
    raise AssertionError("no core module with a data section")


def test_a_sparse_component_is_rewritten_by_every_build(tmp_path: Path) -> None:
    r, built = _build(tmp_path, SPARSE)
    lines = _status_lines(r)
    assert len(lines) == 1 and "merged into one segment" in lines[0], r.stdout + r.stderr
    assert built != SPARSE and _segment_count(built) == 1, "the build reported a merge but the output was not rewritten"


def test_a_component_wasmtime_already_images_is_reported_not_needed_and_left_alone(tmp_path: Path) -> None:
    """The case P1 hides on a dense toolchain: nothing to do, and the script must still SAY it ran the step."""
    r, built = _build(tmp_path, DENSE)
    lines = _status_lines(r)
    assert len(lines) == 1 and "not needed" in lines[0], r.stdout + r.stderr
    # The build stamps a metadata section onto the end AFTER this step, so "left alone" is "still begins with the bytes componentize-py wrote".
    assert built.startswith(DENSE), "a component that needed no rewrite was modified"


def test_a_component_it_cannot_parse_is_kept_and_the_cost_is_stated(tmp_path: Path) -> None:
    r, built = _build(tmp_path, UNPARSEABLE)
    lines = _status_lines(r)
    assert len(lines) == 1 and "NOT rewritten" in lines[0], r.stdout + r.stderr
    assert built.startswith(UNPARSEABLE), "an unparseable component must be kept exactly as componentize-py wrote it"
