"""``cleat_sdk.vet`` warnings for imports that happen only when a workflow RUNS (cleat#4266).

componentize-py keeps only the standard-library modules that were imported while the component
was being BUILT. Measured 2026-11-22 (componentize-py 0.25.1, CPython 3.14.0 for wasi): of the
297 names in the guest's ``sys.stdlib_module_names``, 130 import at run time in a minimal guest
and 167 do not, and ``csv``, ``difflib``, ``textwrap`` and ``pprint`` are four of the 119 that a
top-level import recovers. Two rules, both WARNINGS (a warning never refuses a build):

- **PY014** a standard-library ``import`` inside a function.
- **PY015** a call known to import a module on first use (``LAZY_IMPORT_METHODS`` and
  ``LAZY_IMPORT_CODECS``).

Each PY015 row below was measured in a real component, and so was its remedy: a guest whose ONLY
extra import is the one named in the row ran the call correctly, while every other lazy call in
the same guest still failed. These tests cannot run a component, so what they pin is that the
vet says what was measured: the row exists, it names exactly those modules, and the remedy
silences it.
"""

from __future__ import annotations

import os
import subprocess
import sys
import textwrap
from pathlib import Path

import pytest

from cleat_sdk.vet import (
    INTERPRETER_BUILTIN_MODULES,
    LAZY_IMPORT_CODECS,
    LAZY_IMPORT_METHODS,
    analyze_file,
)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _analyze(tmp_path, source: str):
    f = tmp_path / "wf.py"
    f.write_text(textwrap.dedent(source))
    return analyze_file(str(f))


def _by_code(result, code: str) -> list[dict]:
    return [w for w in result.warnings if w["code"] == code]


def _wrap(body: str, top: str = "") -> str:
    """A workflow module: ``top`` at module level, ``body`` inside the entry function."""
    inner = textwrap.indent(textwrap.dedent(body).strip("\n"), "    ")
    return (
        "from cleat_sdk import HostCalls, cleat_entry\n"
        f"{textwrap.dedent(top)}\n"
        '@cleat_entry("wf")\n'
        "def wf(h: HostCalls, request: dict) -> str:\n"
        f"{inner}\n"
        "    return ''\n"
    )


# ---------------------------------------------------------------------------
# PY014: a standard-library import inside a function
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("module", ["csv", "difflib", "textwrap", "pprint"])
def test_py014_the_four_modules_of_the_issue(tmp_path, module) -> None:
    """KNOWN POSITIVE: the four modules cleat#4266 reported, imported late, are flagged."""
    warnings = _by_code(_analyze(tmp_path, _wrap(f"import {module}")), "PY014")
    assert len(warnings) == 1
    assert f"import {module}" in warnings[0]["message"]
    assert "wf()" in warnings[0]["message"]
    assert f"Move `import {module}` to the top level" in warnings[0]["suggestion"]


@pytest.mark.parametrize("module", ["csv", "difflib", "textwrap", "pprint"])
def test_py014_top_level_import_is_the_remedy(tmp_path, module) -> None:
    """NEGATIVE CONTROL: the same module imported at module level is not flagged."""
    result = _analyze(tmp_path, _wrap(f"{module}.__name__", top=f"import {module}\n"))
    assert _by_code(result, "PY014") == []


def test_py014_imported_at_both_levels_is_not_flagged(tmp_path) -> None:
    """A redundant in-function import is harmless once the module is imported at build time."""
    result = _analyze(tmp_path, _wrap("import csv", top="import csv\n"))
    assert _by_code(result, "PY014") == []


def test_py014_from_import_inside_a_function(tmp_path) -> None:
    warnings = _by_code(_analyze(tmp_path, _wrap("from textwrap import fill")), "PY014")
    assert len(warnings) == 1
    assert "from textwrap import ..." in warnings[0]["message"]


def test_py014_dotted_module_is_checked_by_its_top_level_package(tmp_path) -> None:
    warnings = _by_code(_analyze(tmp_path, _wrap("import xml.etree.ElementTree")), "PY014")
    assert len(warnings) == 1


def test_py014_a_module_level_import_inside_try_or_if_counts_as_build_time(tmp_path) -> None:
    top = "try:\n    import csv\nexcept ImportError:\n    csv = None\n"
    assert _by_code(_analyze(tmp_path, _wrap("import csv", top=top)), "PY014") == []


def test_py014_a_pure_helper_outside_the_durable_closure_is_checked_too(tmp_path) -> None:
    """The whole file is checked: a function that runs hits the limit whether or not it calls the host."""
    source = (
        "from cleat_sdk import HostCalls, cleat_entry\n"
        "def helper(x):\n"
        "    import difflib\n"
        "    return x\n"
        '@cleat_entry("wf")\n'
        "def wf(h: HostCalls, request: dict) -> str:\n"
        "    return helper('a')\n"
    )
    warnings = _by_code(_analyze(tmp_path, source), "PY014")
    assert [w["message"].split("inside function `")[1].split("()")[0] for w in warnings] == [
        "helper"
    ]


def test_py014_methods_nested_functions_and_async_functions(tmp_path) -> None:
    source = (
        "class C:\n"
        "    def m(self):\n"
        "        import csv\n"
        "def outer():\n"
        "    def inner():\n"
        "        import difflib\n"
        "async def a():\n"
        "    import pprint\n"
    )
    names = sorted(
        w["message"].split("inside function `")[1].split("()")[0]
        for w in _by_code(_analyze(tmp_path, source), "PY014")
    )
    assert names == ["a", "inner", "m"]


def test_py014_a_class_body_import_runs_at_build_time_and_is_not_flagged(tmp_path) -> None:
    assert _by_code(_analyze(tmp_path, "class C:\n    import csv\n"), "PY014") == []


@pytest.mark.parametrize("module", sorted(INTERPRETER_BUILTIN_MODULES))
def test_py014_modules_compiled_into_the_interpreter_are_exempt(tmp_path, module) -> None:
    """NEGATIVE CONTROL: modules built into the component's interpreter can never be missing."""
    assert _by_code(_analyze(tmp_path, _wrap(f"import {module}")), "PY014") == []


def test_py014_third_party_and_relative_imports_are_not_checked(tmp_path) -> None:
    """The measurement covers the standard library only, so nothing else is claimed."""
    source = _wrap("import cleat_sdk\nimport numpy\nfrom . import sibling\nfrom .pkg import thing")
    assert _by_code(_analyze(tmp_path, source), "PY014") == []


# ---------------------------------------------------------------------------
# PY014 and PY015 are warnings: they must never refuse a build
# ---------------------------------------------------------------------------


def test_the_new_rules_are_warnings_and_never_errors(tmp_path) -> None:
    result = _analyze(tmp_path, _wrap("import csv\nx = 'a'.encode('cp1252')"))
    assert {w["code"] for w in result.warnings} == {"PY014", "PY015"}
    assert [e for e in result.errors if e["code"] in ("PY014", "PY015")] == []


def test_a_file_with_only_these_warnings_exits_zero_and_prints_them(tmp_path) -> None:
    """`cleat build` refuses on a non-zero exit and prints the output either way."""
    f = tmp_path / "wf.py"
    f.write_text(_wrap("import csv"))
    sdk_root = (
        Path(__file__).resolve().parent.parent
    )  # the checkout this test lives in, never an installed copy
    env = {**os.environ, "PYTHONPATH": str(sdk_root)}
    done = subprocess.run(
        [sys.executable, "-m", "cleat_sdk.vet", str(f)],
        capture_output=True,
        text=True,
        cwd=sdk_root,
        env=env,
    )
    assert done.returncode == 0, done.stdout + done.stderr
    assert "PY014" in done.stdout and "Move `import csv` to the top level" in done.stdout


# ---------------------------------------------------------------------------
# PY015: one case per table row. Each remedy below was measured in a real component.
# ---------------------------------------------------------------------------

# (table key, a call that must warn, the modules the message must tell the user to import)
METHOD_CASES = [
    (
        "strptime",
        "import datetime\ndatetime.datetime.strptime('2026-11-22', '%Y-%m-%d')",
        ["_strptime"],
    ),
    ("strptime", "import time\ntime.strptime('2026-11-22', '%Y-%m-%d')", ["_strptime"]),
    ("most_common", "import collections\ncollections.Counter('abca').most_common(1)", ["heapq"]),
]
CODEC_CASES = [
    ("cp1252", "'caf\\u00e9'.encode('cp1252')", ["encodings.cp1252"]),
    ("cp1252", "b'caf\\xe9'.decode('windows-1252')", ["encodings.cp1252"]),  # an alias spelling
    ("cp1252", "'caf\\u00e9'.encode(encoding='cp1252')", ["encodings.cp1252"]),  # keyword form
    ("shift_jis", "b'\\x82\\xa0'.decode('shift_jis')", ["encodings.shift_jis"]),
    ("shift_jis", "b'\\x82\\xa0'.decode('sjis')", ["encodings.shift_jis"]),
    ("iso8859_15", "'caf\\u00e9'.encode('iso-8859-15')", ["encodings.iso8859_15"]),
    ("iso8859_15", "'caf\\u00e9'.encode('latin9')", ["encodings.iso8859_15"]),
    ("utf_8_sig", "'a'.encode('utf-8-sig')", ["encodings.utf_8_sig"]),
    ("idna", "'b\\u00fccher.de'.encode('idna')", ["encodings.idna", "encodings.punycode"]),
]
ALL_CASES = METHOD_CASES + CODEC_CASES


def test_every_table_row_has_a_test() -> None:
    """Adding a row to either table without a case here fails, so a row cannot ship untested."""
    assert {key for key, _, _ in ALL_CASES} == set(LAZY_IMPORT_METHODS) | set(LAZY_IMPORT_CODECS)


@pytest.mark.parametrize(("key", "call", "modules"), ALL_CASES)
def test_py015_known_positive_names_the_modules_to_import(tmp_path, key, call, modules) -> None:
    warnings = _by_code(_analyze(tmp_path, _wrap(call)), "PY015")
    assert len(warnings) == 1, [w["message"] for w in warnings]
    for module in modules:
        assert f"`import {module}`" in warnings[0]["suggestion"]
    assert "top level of the workflow module" in warnings[0]["suggestion"]


@pytest.mark.parametrize(("key", "call", "modules"), ALL_CASES)
def test_py015_negative_control_the_remedy_import_silences_it(tmp_path, key, call, modules) -> None:
    top = "".join(f"import {m}\n" for m in modules)
    assert _by_code(_analyze(tmp_path, _wrap(call, top=top)), "PY015") == []


def test_py015_a_partial_remedy_still_warns_and_names_what_is_left(tmp_path) -> None:
    """Measured: `import encodings.idna` alone is NOT enough, the codec then fails on punycode."""
    warnings = _by_code(
        _analyze(tmp_path, _wrap("'x.de'.encode('idna')", top="import encodings.idna\n")), "PY015"
    )
    assert len(warnings) == 1
    assert "`import encodings.punycode`" in warnings[0]["suggestion"]
    assert "`import encodings.idna`" not in warnings[0]["suggestion"]


def test_py015_a_remedy_for_one_row_does_not_silence_another(tmp_path) -> None:
    """Measured: each single import fixed exactly its own call and left the others failing."""
    source = _wrap("'a'.encode('cp1252')\n'a'.encode('utf-8-sig')", top="import encodings.cp1252\n")
    warnings = _by_code(_analyze(tmp_path, source), "PY015")
    assert len(warnings) == 1 and "utf-8-sig" in warnings[0]["message"]


@pytest.mark.parametrize("codec", ["utf-8", "utf8", "ascii", "latin-1", "utf-16", "utf-32"])
def test_py015_codecs_measured_to_need_nothing_are_not_flagged(tmp_path, codec) -> None:
    assert _by_code(_analyze(tmp_path, _wrap(f"'a'.encode({codec!r})")), "PY015") == []


def test_py015_a_codec_that_was_not_measured_is_not_flagged(tmp_path) -> None:
    """Fail open: advice nobody measured is how a wrong remedy ships. koi8_r is probably the same, but unmeasured."""
    assert _by_code(_analyze(tmp_path, _wrap("'a'.encode('koi8_r')")), "PY015") == []


def test_py015_a_call_at_module_level_runs_at_build_time_and_is_not_flagged(tmp_path) -> None:
    source = "import time\nt = time.strptime('2026-11-22', '%Y-%m-%d')\n"
    assert _by_code(_analyze(tmp_path, source), "PY015") == []


def test_py015_a_bare_strptime_name_is_flagged(tmp_path) -> None:
    warnings = _by_code(
        _analyze(tmp_path, _wrap("from time import strptime\nstrptime('2026', '%Y')")), "PY015"
    )
    assert len(warnings) == 1


def test_py015_a_dynamic_codec_argument_is_not_guessed_at(tmp_path) -> None:
    assert _by_code(_analyze(tmp_path, _wrap("name = 'cp1252'\n'a'.encode(name)")), "PY015") == []


# ---------------------------------------------------------------------------
# The shipped examples are the control on real code
# ---------------------------------------------------------------------------


def test_the_shipped_examples_raise_no_new_warning() -> None:
    """Every example is a workflow someone is meant to copy; none may teach the pattern that fails."""
    examples = sorted((Path(__file__).resolve().parent.parent / "examples").glob("*.py"))
    assert examples, "no examples found: the control would be vacuous"
    flagged = {}
    for path in examples:
        result = analyze_file(str(path))
        hits = [(w["code"], w["line"]) for w in result.warnings if w["code"] in ("PY014", "PY015")]
        if hits:
            flagged[path.name] = hits
    assert flagged == {}, flagged
