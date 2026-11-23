"""cleat#4383: the build REFUSES when the installed componentize-py is not the version this SDK was measured on, unless told not to.

componentize-py decides which standard-library modules a component contains (cleat#4266), and the README's compatibility tables and
``cleat vet``'s PY015 table were measured on ``PINNED_COMPONENTIZE_PY`` and no other version. cleat#4355 made a different version a warning,
because it is not known to be broken, only not measured. A module that is missing raises ModuleNotFoundError only when the workflow RUNS,
long after a warning has scrolled away, so the build now stops, says how to get the measured version, and has one explicit way to go on.

Two layers, because each can be wrong alone: the pure functions that decide and word it, and the script that has to actually stop before it
compiles. The script is driven with a componentize-py stand-in on PATH (as test_build_wasm_wazero_needs_decompose.py does), so no real
toolchain is needed and the version the stand-in reports is the thing under test. The stand-in leaves a marker file when it is asked to
COMPILE, so "refused before compiling anything" is a fact the test reads and not a conclusion from a missing output.
"""

from __future__ import annotations

import importlib.util
import os
import stat
import subprocess
import sys
from pathlib import Path

import pytest

SDK = Path(__file__).resolve().parent.parent
SCRIPT = SDK / "scripts" / "build_wasm.py"


def _load():
    spec = importlib.util.spec_from_file_location("build_wasm_under_test", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


build_wasm = _load()
PIN = build_wasm.PINNED_COMPONENTIZE_PY
FLAG = build_wasm.ALLOW_UNMEASURED_FLAG
ENV = build_wasm.ALLOW_UNMEASURED_ENV
REFUSED = build_wasm.EXIT_UNMEASURED_COMPONENTIZE_PY


# ---------------------------------------------------------------------------
# The decision and its wording
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "output",
    [f"componentize-py {PIN}\n", f"componentize-py {PIN}", f"  componentize-py {PIN} (stand-in)\n"],
)
def test_the_measured_version_is_not_a_problem(output: str) -> None:
    """NEGATIVE CONTROL."""
    assert build_wasm.componentize_py_version_problem(output) is None


@pytest.mark.parametrize(
    "got", ["0.25.0", "0.24.0", "0.26.0", "0.12.0", "0.0.0", f"{PIN}.dev1", f"{PIN}0", f"{PIN}rc1"]
)
def test_another_version_is_a_problem_naming_both(got: str) -> None:
    """KNOWN POSITIVE, including the neighbours a prefix match would wave through (``0.25.10``-style, a dev or rc of the pin)."""
    problem = build_wasm.componentize_py_version_problem(f"componentize-py {got}\n")
    assert problem is not None
    assert f"componentize-py {got} is installed" in problem
    assert f"measured with componentize-py {PIN}" in problem
    assert "ModuleNotFoundError" in problem, "it must say why it matters, not only that the numbers differ"


@pytest.mark.parametrize("output", ["", "\n", "some other tool 1.2.3", "componentize-py\n"])
def test_an_unreadable_version_is_a_problem_not_a_match(output: str) -> None:
    """The check could not look, so it must not say OK."""
    problem = build_wasm.componentize_py_version_problem(output)
    assert problem is not None
    assert "could not be read" in problem


def test_the_refusal_names_the_fix_and_the_way_past_it() -> None:
    message = build_wasm.componentize_py_refusal(build_wasm.componentize_py_version_problem("componentize-py 0.24.0\n"))
    assert message.startswith("Error: ")
    assert "Refusing to build" in message
    assert f"pip install componentize-py=={PIN}" in message
    assert f"cleat build --target python {FLAG}" in message, (
        "the way past it a person types is the cleat build command, and it must be written out and not only the flag's name, which the next line also carries"
    )
    assert f"{ENV}=1" in message, "a refusal with no stated way past it is a dead end"


@pytest.mark.parametrize(
    "flag, environ, want",
    [
        (False, {}, False),
        (True, {}, True),
        (False, {ENV: "1"}, True),
        (False, {ENV: "0"}, False),
        (False, {ENV: ""}, False),
        (False, {ENV: "false"}, False),
        (False, {ENV: "yes"}, False),
        (True, {ENV: "0"}, True),
    ],
)
def test_only_the_flag_or_the_value_1_asks_for_the_override(flag: bool, environ: dict, want: bool) -> None:
    """``0`` and ``false`` are how a person says no, so they must not read as yes."""
    assert build_wasm.unmeasured_componentize_py_allowed(flag, environ) is want


# ---------------------------------------------------------------------------
# The script stops before compiling, or goes on when told to
# ---------------------------------------------------------------------------

STAND_IN = """#!/bin/sh
# A componentize-py that reports a chosen version. It leaves {marker} when asked to COMPILE, and writes a file with the component header where -o says.
if [ "$1" = "--version" ]; then echo "componentize-py {version}"; exit 0; fi
touch "{marker}"
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; fi
  shift
done
printf '\\000asm\\015\\000\\001\\000stand-in' > "$out"
"""


def _build(tmp_path: Path, version: str, *extra_args: str, env_extra: dict | None = None) -> tuple[subprocess.CompletedProcess[str], Path]:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    marker = tmp_path / "compile-was-asked-for"
    stub = bin_dir / "componentize-py"
    stub.write_text(STAND_IN.format(version=version, marker=marker))
    stub.chmod(stub.stat().st_mode | stat.S_IXUSR)
    entry = tmp_path / "wf.py"
    entry.write_text(
        "from cleat_sdk import HostCalls, cleat_entry\n\n\n"
        '@cleat_entry("wf")\ndef wf(h: HostCalls, name: str) -> dict:\n    return {"ok": True}\n'
    )
    env = {k: v for k, v in os.environ.items() if k != ENV}
    env["PATH"] = f"{bin_dir}:/usr/bin:/bin"
    env["PYTHONPATH"] = str(SDK)
    env.update(env_extra or {})
    r = subprocess.run(
        [
            sys.executable,
            str(SCRIPT),
            "--entry",
            f"{entry}:wf",
            "--output",
            str(tmp_path / "out.wasm"),
            "--runtime",
            "wasmtime",
            *extra_args,
        ],
        capture_output=True,
        text=True,
        timeout=120,
        env=env,
        cwd=str(SDK),
    )
    return r, marker


def test_a_build_with_another_version_is_refused_before_it_compiles(tmp_path: Path) -> None:
    r, marker = _build(tmp_path, "0.24.0")
    out = r.stdout + r.stderr
    assert r.returncode == REFUSED, f"want exit {REFUSED}, got {r.returncode}: {out}"
    assert "componentize-py 0.24.0 is installed" in r.stderr, out
    assert f"measured with componentize-py {PIN}" in r.stderr
    assert f"pip install componentize-py=={PIN}" in r.stderr
    assert f"cleat build --target python {FLAG}" in r.stderr, "the command a person types must reach them through the script, not only the unit"
    assert not marker.exists(), "componentize-py was asked to compile: the refusal came too late to be one. " + out
    assert not (tmp_path / "out.wasm").exists(), out
    assert "Build SUCCESS" not in out
    assert "Build FAILED" not in out, "a refusal is not a build that ran and failed, and the troubleshooting list for one would mislead: " + out


def test_a_build_whose_version_cannot_be_read_is_refused_too(tmp_path: Path) -> None:
    r, marker = _build(tmp_path, "")
    assert r.returncode == REFUSED, r.stdout + r.stderr
    assert "could not be read" in r.stderr, r.stdout + r.stderr
    assert not marker.exists()


def test_the_flag_builds_anyway_and_says_it_is_doing_so(tmp_path: Path) -> None:
    r, marker = _build(tmp_path, "0.24.0", FLAG)
    out = r.stdout + r.stderr
    assert r.returncode == 0, out
    assert "componentize-py 0.24.0 is installed" in r.stderr, out
    assert "Building anyway" in r.stderr and FLAG in r.stderr, out
    assert marker.exists() and (tmp_path / "out.wasm").exists(), "the override did not build: " + out
    assert "Build SUCCESS" in r.stdout, out


def test_the_environment_variable_builds_anyway_and_zero_does_not(tmp_path: Path) -> None:
    on = tmp_path / "on"
    on.mkdir()
    r, _ = _build(on, "0.24.0", env_extra={ENV: "1"})
    assert r.returncode == 0, r.stdout + r.stderr
    assert "Building anyway" in r.stderr
    off = tmp_path / "off"
    off.mkdir()
    r, marker = _build(off, "0.24.0", env_extra={ENV: "0"})
    assert r.returncode == REFUSED, "=0 must not count as yes: " + r.stdout + r.stderr
    assert not marker.exists()


def test_a_build_with_the_measured_version_is_neither_refused_nor_warned(tmp_path: Path) -> None:
    """NEGATIVE CONTROL for the script, so the tests above cannot pass by the refusal always firing."""
    r, marker = _build(tmp_path, PIN)
    out = r.stdout + r.stderr
    assert r.returncode == 0, out
    assert "is installed, but this SDK was tested" not in r.stderr, r.stderr
    assert "Building anyway" not in r.stderr
    assert marker.exists() and (tmp_path / "out.wasm").exists(), out


def test_the_flag_with_the_measured_version_is_silent(tmp_path: Path) -> None:
    """Asking for the override when nothing needs overriding must not print a warning that is not true."""
    r, _ = _build(tmp_path, PIN, FLAG)
    assert r.returncode == 0, r.stdout + r.stderr
    assert "Building anyway" not in r.stderr, r.stderr
