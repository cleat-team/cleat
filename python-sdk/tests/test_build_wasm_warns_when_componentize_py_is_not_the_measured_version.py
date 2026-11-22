"""cleat#4355: the build warns, and does not refuse, when the installed componentize-py is not the version this SDK was measured on.

componentize-py decides which standard-library modules a component contains (cleat#4266), and the README's compatibility tables and
``cleat vet``'s PY015 table were measured on ``PINNED_COMPONENTIZE_PY`` and no other version. A different version is not known to be
broken, only not measured, so the coordinator's default is a warning that names both versions and says why it matters; the owner can
tighten it to a refusal later.

Two layers, because each can be wrong alone: the pure function that decides, and the script that has to actually print it before it
compiles. The script is driven with a componentize-py stand-in on PATH (as test_build_wasm_wazero_needs_decompose.py does), so no real
toolchain is needed and the version the stand-in reports is the thing under test.
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


# ---------------------------------------------------------------------------
# The decision
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "output",
    [f"componentize-py {PIN}\n", f"componentize-py {PIN}", f"  componentize-py {PIN} (stand-in)\n"],
)
def test_the_measured_version_is_not_warned_about(output: str) -> None:
    """NEGATIVE CONTROL."""
    assert build_wasm.componentize_py_version_warning(output) is None


@pytest.mark.parametrize(
    "got", ["0.25.0", "0.24.0", "0.26.0", "0.12.0", "0.0.0", f"{PIN}.dev1", f"{PIN}0", f"{PIN}rc1"]
)
def test_another_version_is_warned_about_naming_both(got: str) -> None:
    """KNOWN POSITIVE, including the neighbours a prefix match would wave through (``0.25.10``-style, a dev or rc of the pin)."""
    warning = build_wasm.componentize_py_version_warning(f"componentize-py {got}\n")
    assert warning is not None
    assert f"componentize-py {got} is installed" in warning
    assert f"measured with componentize-py {PIN}" in warning
    assert f"pip install componentize-py=={PIN}" in warning
    assert "ModuleNotFoundError" in warning, (
        "the warning must say why it matters, not only that the numbers differ"
    )


@pytest.mark.parametrize("output", ["", "\n", "some other tool 1.2.3", "componentize-py\n"])
def test_an_unreadable_version_is_reported_as_unreadable_not_as_a_match(output: str) -> None:
    """The check could not look, so it must not say OK."""
    warning = build_wasm.componentize_py_version_warning(output)
    assert warning is not None
    assert "could not be read" in warning


# ---------------------------------------------------------------------------
# The script prints it, before compiling, and still builds
# ---------------------------------------------------------------------------

STAND_IN = """#!/bin/sh
# A componentize-py that reports a chosen version and writes a file with the component header where -o says.
if [ "$1" = "--version" ]; then echo "componentize-py {version}"; exit 0; fi
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; fi
  shift
done
printf '\\000asm\\015\\000\\001\\000stand-in' > "$out"
"""


def _build(tmp_path: Path, version: str) -> subprocess.CompletedProcess[str]:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    stub = bin_dir / "componentize-py"
    stub.write_text(STAND_IN.format(version=version))
    stub.chmod(stub.stat().st_mode | stat.S_IXUSR)
    entry = tmp_path / "wf.py"
    entry.write_text(
        "from cleat_sdk import HostCalls, cleat_entry\n\n\n"
        '@cleat_entry("wf")\ndef wf(h: HostCalls, name: str) -> dict:\n    return {"ok": True}\n'
    )
    env = dict(os.environ)
    env["PATH"] = f"{bin_dir}:/usr/bin:/bin"
    env["PYTHONPATH"] = str(SDK)
    return subprocess.run(
        [
            sys.executable,
            str(SCRIPT),
            "--entry",
            f"{entry}:wf",
            "--output",
            str(tmp_path / "out.wasm"),
            "--runtime",
            "wasmtime",
        ],
        capture_output=True,
        text=True,
        timeout=120,
        env=env,
        cwd=str(SDK),
    )


def test_a_build_with_another_version_warns_and_still_builds(tmp_path: Path) -> None:
    r = _build(tmp_path, "0.24.0")
    assert "componentize-py 0.24.0 is installed" in r.stderr, r.stdout + r.stderr
    assert f"measured with componentize-py {PIN}" in r.stderr
    assert (tmp_path / "out.wasm").exists(), (
        "a warning must not refuse the build: " + r.stdout + r.stderr
    )
    assert "Build SUCCESS" in r.stdout, r.stdout + r.stderr


def test_a_build_with_the_measured_version_prints_no_such_warning(tmp_path: Path) -> None:
    """NEGATIVE CONTROL for the script, so the test above cannot pass by the warning always printing."""
    ok = tmp_path / "ok"
    ok.mkdir()
    r = _build(ok, PIN)
    assert "is installed, but this SDK was tested" not in r.stderr, r.stderr
    assert (ok / "out.wasm").exists(), r.stdout + r.stderr
