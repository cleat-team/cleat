"""The SDK preloads the standard-library modules that a call imports on first use (cleat#4266).

componentize-py keeps only the modules imported while the component is built, and every workflow imports ``cleat_sdk``. The component-level proof
is engine/a_python_workflow_can_call_the_stdlib_functions_that_import_late_test.go, which builds a real workflow and runs every call. What these
tests pin is the Python side of it: the preload is imported by the package, the vet agrees with it, and a module that is NOT preloaded still warns.
"""

from __future__ import annotations

import json
import subprocess
import sys
import textwrap
from pathlib import Path

import pytest

import cleat_sdk.vet as vet_module
from cleat_sdk._stdlib_preload import PRELOADED_MODULES
from cleat_sdk.vet import LAZY_IMPORT_CODECS, LAZY_IMPORT_METHODS, analyze_file

SDK_ROOT = Path(__file__).resolve().parent.parent  # the checkout this test lives in, never an installed copy


def _warnings(tmp_path, body: str) -> list[dict]:
    f = tmp_path / "wf.py"
    f.write_text(
        "from cleat_sdk import HostCalls, cleat_entry\n"
        "import datetime, collections\n"
        '@cleat_entry("wf")\n'
        "def wf(h: HostCalls, request: dict) -> str:\n" + textwrap.indent(textwrap.dedent(body).strip("\n"), "    ") + "\n"
    )
    return [w for w in analyze_file(str(f)).warnings if w["code"] == "PY015"]


def test_importing_the_sdk_imports_every_preloaded_module() -> None:
    """In a FRESH interpreter, read BEFORE anything else touches the preload.

    The names come in as an argument and ``sys.modules`` is read straight after ``import cleat_sdk``: an earlier draft imported
    ``cleat_sdk._stdlib_preload`` itself first and then asserted it was loaded, which is true of any package and survived deleting the
    package's own import of it.
    """
    code = (
        "import json, sys, cleat_sdk\n"
        "loaded = 'cleat_sdk._stdlib_preload' in sys.modules\n"
        "missing = [m for m in json.loads(sys.argv[1]) if m not in sys.modules]\n"
        "assert loaded, 'importing cleat_sdk does not import cleat_sdk._stdlib_preload'\n"
        "assert not missing, missing\n"
    )
    done = subprocess.run(
        [sys.executable, "-c", code, json.dumps(list(PRELOADED_MODULES))],
        capture_output=True,
        text=True,
        cwd=SDK_ROOT,
        env={"PYTHONPATH": str(SDK_ROOT)},
    )
    assert done.returncode == 0, done.stdout + done.stderr


def test_the_preload_names_exactly_what_the_measured_calls_need() -> None:
    assert set(PRELOADED_MODULES) == {
        "_strptime",
        "heapq",
        "encodings.cp1252",
        "encodings.shift_jis",
        "encodings.iso8859_15",
        "encodings.utf_8_sig",
        "encodings.idna",
        "encodings.punycode",
    }


@pytest.mark.parametrize(
    "call",
    [
        "datetime.datetime.strptime('2026-11-23', '%Y-%m-%d')",
        "collections.Counter('abca').most_common(1)",
        "'caf\\u00e9'.encode('cp1252')",
        "b'\\x82\\xa0'.decode('shift_jis')",
        "'caf\\u00e9'.encode('iso-8859-15')",
        "'a'.encode('utf-8-sig')",
        "'b\\u00fccher.de'.encode('idna')",
    ],
)
def test_a_preloaded_call_no_longer_warns(tmp_path, call) -> None:
    assert _warnings(tmp_path, call) == []


def test_every_table_row_is_silent_under_the_real_preload(tmp_path) -> None:
    """The rows are what the preload was measured against: each module any row names must be preloaded."""
    named = {m for mods, _ in LAZY_IMPORT_METHODS.values() for m in mods} | {m for mods in LAZY_IMPORT_CODECS.values() for m in mods}
    assert named <= set(PRELOADED_MODULES), sorted(named - set(PRELOADED_MODULES))


def test_a_module_that_is_not_preloaded_still_warns(tmp_path, monkeypatch) -> None:
    """The known-positive: remove ONE module from the set and exactly the call that needs it warns, the others stay silent."""
    monkeypatch.setattr(vet_module, "PRELOADED_MODULES", tuple(m for m in PRELOADED_MODULES if m != "heapq"))
    warned = _warnings(tmp_path, "collections.Counter('abca').most_common(1)")
    assert len(warned) == 1 and "`import heapq`" in warned[0]["suggestion"], warned
    assert _warnings(tmp_path, "datetime.datetime.strptime('2026-11-23', '%Y-%m-%d')") == []
