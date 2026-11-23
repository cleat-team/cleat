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


def test_py016_a_module_level_call_is_not_flagged_because_it_is_a_build_time_constant(
    tmp_path,
) -> None:
    """``T0 = time.monotonic()`` at module level is evaluated once, while componentize-py builds, so a run and its replay read the same value.

    Measured in review (cleat#4428): the same ``2.607878167`` on both runs. The scope is the functions an entry point reaches, as for every other rule here.
    """
    result = _analyze(
        tmp_path, _wrap("return_value = T0", top="import time\nT0 = time.monotonic()\n")
    )
    assert _by_code(result, "PY016") == []


def test_py016_a_module_level_reference_is_flagged_where_it_is_called(tmp_path) -> None:
    """KNOWN POSITIVE for the half that was missed: ``clock = time.perf_counter`` at module level, ``clock()`` in the entry.

    Measured in review (cleat#4428): ``clock()`` read 0.0199 and then 0.0109 on two runs of one workflow id, the real clock at run time, while the
    module-level CALL above was frozen. The warning is at the use, line 6 of the module, because that is where the clock is read.
    """
    result = _analyze(
        tmp_path, _wrap("t0 = clock()", top="import time\nclock = time.perf_counter\n")
    )
    warnings = _by_code(result, "PY016")
    assert len(warnings) == 1, result.warnings
    assert "time.perf_counter()" in warnings[0]["message"]
    assert warnings[0]["line"] == 7, warnings


def test_py016_module_level_references_in_every_spelling(tmp_path) -> None:
    """Plain, annotated and aliased-module forms, and ``time.sleep`` held in a name (PY004 matches the spelling ``time.sleep`` only)."""
    for top, body, function in (
        ("import time\nclock: object = time.monotonic_ns\n", "clock()", "time.monotonic_ns()"),
        ("import time as t\nclock = t.perf_counter_ns\n", "clock()", "time.perf_counter_ns()"),
        ("import time\nnap = time.sleep\n", "nap(1)", "time.sleep()"),
    ):
        warnings = _by_code(_analyze(tmp_path, _wrap(body, top=top)), "PY016")
        assert len(warnings) == 1, (top, body, warnings)
        assert function in warnings[0]["message"]


def test_py016_a_module_level_reference_that_is_never_called_warns_nowhere(tmp_path) -> None:
    """The warning is at the use. An alias nothing calls reads no clock, and an unrelated module-level assignment is not an alias."""
    result = _analyze(
        tmp_path,
        _wrap(
            "unused = (label, other)",
            top="import time\nclock = time.perf_counter\nlabel = time.timezone\nother = len\n",
        ),
    )
    assert _by_code(result, "PY016") == []


def test_py016_a_module_level_reference_to_an_unrelated_object_is_not_an_alias(tmp_path) -> None:
    """``tick = meter.monotonic`` is a method of a name that is not the ``time`` module. Calling ``tick()`` reads whatever that object reads."""
    top = "class Meter:\n    def monotonic(self):\n        return 0\n\nmeter = Meter()\ntick = meter.monotonic\n"
    assert _by_code(_analyze(tmp_path, _wrap("tick()", top=top)), "PY016") == []
