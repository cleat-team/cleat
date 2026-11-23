"""``cleat_sdk.vet`` warning for a real-time clock read or sleep in workflow code (cleat#4396).

A Python or C# workflow reads the host's own monotonic clock: ``wasi:clocks/monotonic-clock`` is not shadowed, because a shadow replaces the whole
interface and its ``subscribe-instant`` / ``subscribe-duration`` return pollables the host cannot supply. Measured 2026-11-23 on componentize-py
0.25.1 with the probe from cleat#4396: two runs of one workflow id read different monotonic values (``mono_delta`` 0.5029 and 0.5021 for a
``time.sleep(0.5)``) and a ``while monotonic() - t0 < 0.5`` loop ran 41 then 42 iterations. The owner decided to keep the clock real and say so, so what
changes is this warning.

- **PY016** (a WARNING, never an error) ``time.monotonic``, ``time.monotonic_ns``, ``time.perf_counter``, ``time.perf_counter_ns``, and a
  ``time.sleep`` written through an alias. A ``time.sleep`` written as ``time.sleep`` is the existing PY004 ERROR and is not reported twice.

What these tests pin is the RULE: which spellings are flagged, where, and what the message tells the author. The measurements are in the issue.
"""

from __future__ import annotations

import textwrap

import pytest

from cleat_sdk.vet import MONOTONIC_CLOCK_CALLS, analyze_file

MONOTONIC = ["monotonic", "monotonic_ns", "perf_counter", "perf_counter_ns"]


def _analyze(tmp_path, source: str):
    f = tmp_path / "wf.py"
    f.write_text(textwrap.dedent(source))
    return analyze_file(str(f))


def _by_code(result, code: str) -> list[dict]:
    return [w for w in result.warnings if w["code"] == code]


def _codes(items) -> set[str]:
    return {i["code"] for i in items}


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
# The calls
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("name", MONOTONIC)
def test_py016_flags_each_monotonic_call(tmp_path, name) -> None:
    """KNOWN POSITIVE: each of the four calls, written as the SDK's own docs would write them, is a warning and not an error."""
    result = _analyze(tmp_path, _wrap(f"started = time.{name}()", top="import time\n"))
    warnings = _by_code(result, "PY016")
    assert len(warnings) == 1, result.warnings
    w = warnings[0]
    assert f"time.{name}()" in w["message"]
    assert "replay" in w["message"]
    assert "h.now()" in w["suggestion"]
    assert "PY016" not in _codes(result.errors), "a warning must never refuse a build"
    assert w["line"] == 6 and w["file"].endswith("wf.py"), w


def test_py016_the_table_is_exactly_the_four_calls() -> None:
    """The table is the rule: a fifth entry or a missing one changes what authors are told."""
    assert sorted(MONOTONIC_CLOCK_CALLS) == sorted(MONOTONIC)


def test_py016_a_reference_without_a_call_is_flagged(tmp_path) -> None:
    """``clock = time.perf_counter`` and ``timeit``-style ``default_timer=time.monotonic`` read the clock later."""
    result = _analyze(tmp_path, _wrap("clock = time.perf_counter\nclock()", top="import time\n"))
    assert len(_by_code(result, "PY016")) == 1


def test_py016_the_message_names_the_remedy_and_the_reason(tmp_path) -> None:
    """The author is told what to use instead and why, which is what makes it a warning someone acts on."""
    result = _analyze(tmp_path, _wrap("time.monotonic()", top="import time\n"))
    w = _by_code(result, "PY016")[0]
    assert "differs between a run and its replay" in w["message"]
    assert "h.now()" in w["suggestion"] and "h.sleep(" in w["suggestion"]


# ---------------------------------------------------------------------------
# Spellings
# ---------------------------------------------------------------------------


def test_py016_module_alias(tmp_path) -> None:
    result = _analyze(tmp_path, _wrap("t.monotonic()", top="import time as t\n"))
    assert len(_by_code(result, "PY016")) == 1


@pytest.mark.parametrize("name", MONOTONIC)
def test_py016_from_import(tmp_path, name) -> None:
    result = _analyze(tmp_path, _wrap(f"{name}()", top=f"from time import {name}\n"))
    warnings = _by_code(result, "PY016")
    assert len(warnings) == 1, result.warnings
    assert f"time.{name}()" in warnings[0]["message"]


def test_py016_from_import_with_an_alias(tmp_path) -> None:
    result = _analyze(tmp_path, _wrap("clock()", top="from time import perf_counter_ns as clock\n"))
    warnings = _by_code(result, "PY016")
    assert len(warnings) == 1
    assert "time.perf_counter_ns()" in warnings[0]["message"], (
        "the message names the real function, not the alias"
    )


def test_py016_an_import_inside_the_function_is_resolved_too(tmp_path) -> None:
    result = _analyze(tmp_path, _wrap("from time import monotonic\nmonotonic()"))
    assert len(_by_code(result, "PY016")) == 1


# ---------------------------------------------------------------------------
# time.sleep: PY004 stays the error, aliases are the warning
# ---------------------------------------------------------------------------


def test_time_sleep_written_plainly_is_the_existing_error_and_not_reported_twice(tmp_path) -> None:
    """The direct spelling is PY004 (an error, unchanged); PY016 must not repeat it."""
    result = _analyze(tmp_path, _wrap("time.sleep(1)", top="import time\n"))
    assert "PY004" in _codes(result.errors)
    assert _by_code(result, "PY016") == []


def test_py016_flags_time_sleep_through_an_alias(tmp_path) -> None:
    """PY004 matches the spelling ``time.sleep`` and nothing else. The same call through an alias is invisible to it."""
    for top, body in (
        ("import time as t\n", "t.sleep(1)"),
        ("from time import sleep\n", "sleep(1)"),
        ("from time import sleep as nap\n", "nap(1)"),
    ):
        result = _analyze(tmp_path, _wrap(body, top=top))
        warnings = _by_code(result, "PY016")
        assert len(warnings) == 1, (top, body, result.warnings, result.errors)
        assert "time.sleep()" in warnings[0]["message"]
        assert "h.sleep(" in warnings[0]["suggestion"]
        assert "PY004" not in _codes(result.errors), (
            "the alias is a warning; it must not newly refuse a build"
        )


def test_the_sleep_message_states_what_was_measured_about_the_instance_timeout(tmp_path) -> None:
    """cleat#4403, measured: an in-guest sleep blocks its execution for the whole duration and the instance timeout does not interrupt it."""
    sleep = _by_code(_analyze(tmp_path, _wrap("t.sleep(1)", top="import time as t\n")), "PY016")[0][
        "message"
    ]
    assert "instance timeout" in sleep and "cleat#4403" in sleep
    errors = _analyze(tmp_path, _wrap("time.sleep(1)", top="import time\n")).errors
    error = next(e["message"] for e in errors if e["code"] == "PY004")
    assert "instance timeout" in error and "cleat#4403" in error


# ---------------------------------------------------------------------------
# Not flagged
# ---------------------------------------------------------------------------


def test_py016_the_durable_clock_is_not_flagged(tmp_path) -> None:
    """NEGATIVE CONTROL: the remedy the warning recommends is itself clean."""
    result = _analyze(
        tmp_path,
        _wrap("started = h.now()\nh.sleep(1)\nelapsed_ms = h.now() - started", top="import time\n"),
    )
    assert _by_code(result, "PY016") == []


def test_py016_wall_clock_and_other_time_functions_are_not_this_rule(tmp_path) -> None:
    """``time.time`` is PY005's; ``time.strftime`` is not a clock read this rule is about."""
    result = _analyze(tmp_path, _wrap("time.strftime('%Y')\ntime.gmtime(0)", top="import time\n"))
    assert _by_code(result, "PY016") == []


def test_py016_an_unrelated_object_with_the_same_method_name_is_not_flagged(tmp_path) -> None:
    """``monotonic`` is a common name. Only a name that resolves to the ``time`` module counts."""
    result = _analyze(
        tmp_path,
        _wrap(
            "m = Meter()\nm.monotonic()\nperf_counter = 3\nprint_nothing = perf_counter",
            top="class Meter:\n    def monotonic(self):\n        return 0\n",
        ),
    )
    assert _by_code(result, "PY016") == []


def test_py016_only_what_the_workflow_executes_is_checked(tmp_path) -> None:
    """Scope is the determinism scope: a helper the entry reaches is checked, a harness it never calls is not (the PY010 rule, same reason)."""
    source = (
        "import time\n"
        "from cleat_sdk import HostCalls, cleat_entry\n"
        "def reached():\n"
        "    return time.monotonic()\n"
        "def harness_only():\n"
        "    return time.perf_counter()\n"
        '@cleat_entry("wf")\n'
        "def wf(h: HostCalls, request: dict) -> str:\n"
        "    reached()\n"
        "    return ''\n"
    )
    warnings = _by_code(_analyze(tmp_path, source), "PY016")
    assert [w["line"] for w in warnings] == [4], warnings


def test_py016_module_level_code_is_outside_the_scope(tmp_path) -> None:
    """The scope is the functions the entry points reach, as for every other rule here; a read at module level is not in it.

    Not a claim that a module-level read is safe: it is a statement of what this rule looks at, so the limit is written down and not discovered.
    """
    result = _analyze(
        tmp_path,
        _wrap("pass", top="import time\nSTARTED = time.monotonic()\nCLOCK = time.perf_counter\n"),
    )
    assert _by_code(result, "PY016") == []
