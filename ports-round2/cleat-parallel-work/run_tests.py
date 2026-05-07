"""
Simple test runner for the Cleat parallel work port.

Runs all tests without pytest.  Uses the ``CleatTestHarness`` to simulate
the host runtime.

Usage::

    PYTHONPATH=/path/to/python-sdk:$PYTHONPATH python3 run_tests.py
"""

import json
import sys
import traceback

# Ensure cleat_sdk is importable
try:
    from cleat_sdk.test_harness import CleatTestHarness
except ImportError:
    sys.path.insert(0, "/localssd/rcownie/cleat-agent1/python-sdk")
    from cleat_sdk.test_harness import CleatTestHarness

# Import the unwrapped original functions
from app import fan_out_worker, execute_subtask, fan_out_worker_stop_on_error


# ---------------------------------------------------------------------------
# Test helpers
# ---------------------------------------------------------------------------

pass_count = 0
fail_count = 0


def check(description: str, condition: bool, detail: str = ""):
    global pass_count, fail_count
    if condition:
        pass_count += 1
        print(f"  PASS: {description}")
    else:
        fail_count += 1
        print(f"  FAIL: {description}")
        if detail:
            print(f"        {detail}")


def check_eq(description: str, actual, expected):
    if actual == expected:
        check(description, True)
    else:
        check(description, False,
              f"expected {expected!r}, got {actual!r}")


# ======================================================================
# Tests
# ======================================================================


def test_execute_subtask_basic():
    """A subtask is logged, slept for, and returns a completion string."""
    h = CleatTestHarness()
    result = execute_subtask.__wrapped__(h, subtask="hello")
    check_eq("basic execution", result, "hello: DONE")


def test_execute_subtask_special_chars():
    """Subtask strings with punctuation are handled."""
    h = CleatTestHarness()
    result = execute_subtask.__wrapped__(h, subtask="process_order#42")
    check_eq("special characters", result, "process_order#42: DONE")


def test_execute_subtask_empty():
    """An empty subtask string still returns a valid result."""
    h = CleatTestHarness()
    result = execute_subtask.__wrapped__(h, subtask="")
    check_eq("empty subtask", result, ": DONE")


# ---------------------------------------------------------------------------
# Fan-out worker tests
# ---------------------------------------------------------------------------


def test_fan_out_single_subtask():
    """A single subtask fans out to one child and aggregates its result."""
    h = CleatTestHarness()
    h.stub_child_workflow("execute_subtask", '"single: DONE"')
    result = fan_out_worker.__wrapped__(h, task="single")
    check_eq("single subtask", result, "single: DONE")


def test_fan_out_multiple_subtasks():
    """Multiple comma-separated subtasks are all executed in parallel."""
    h = CleatTestHarness()
    h.stub_child_workflow("execute_subtask", '"item: DONE"')
    result = fan_out_worker.__wrapped__(h, task="item1,item2,item3")
    check_eq("multiple subtasks", result, "item: DONE, item: DONE, item: DONE")


def test_fan_out_whitespace():
    """Extra whitespace around comma separators is stripped."""
    h = CleatTestHarness()
    h.stub_child_workflow("execute_subtask", '"DONE"')
    result = fan_out_worker.__wrapped__(h, task="  a , b , c  ")
    check_eq("whitespace handling", result, "DONE, DONE, DONE")


def test_fan_out_empty_task():
    """An empty task string produces an empty result."""
    h = CleatTestHarness()
    result = fan_out_worker.__wrapped__(h, task="")
    check_eq("empty task", result, "")


def test_fan_out_whitespace_only():
    """A task with only whitespace/empty commas produces an empty result."""
    h = CleatTestHarness()
    result = fan_out_worker.__wrapped__(h, task="   ,   ,   ")
    check_eq("whitespace only", result, "")


def test_fan_out_json_decode():
    """Verify the ChildResult JSON parsing works end-to-end."""
    h = CleatTestHarness()
    child_output = json.dumps("hello: DONE")
    h.stub_child_workflow("execute_subtask", child_output)
    result = fan_out_worker.__wrapped__(h, task="hello")
    check_eq("JSON decode round-trip", result, "hello: DONE")


# ---------------------------------------------------------------------------
# Error handling tests
# ---------------------------------------------------------------------------


def test_child_error_collected():
    """When a child workflow fails, the error is collected in the result."""
    h = CleatTestHarness()
    h.stub_child_workflow("execute_subtask", "", error="processing failed")
    result = fan_out_worker.__wrapped__(h, task="broken")
    check("error in result",
          "processing failed" in result,
          f"got: {result!r}")
    check("ERROR prefix in result",
          result.startswith("ERROR"),
          f"got: {result!r}")


def test_stop_on_error_raises():
    """The stop-on-error variant raises RuntimeError on first failure."""
    h = CleatTestHarness()
    h.stub_child_workflow("execute_subtask", "", error="processing failed")
    try:
        fan_out_worker_stop_on_error.__wrapped__(h, task="broken")
        check("stop_on_error raises", False, "no exception was raised")
    except RuntimeError as e:
        check("stop_on_error raises", "processing failed" in str(e),
              f"got: {e}")
    except Exception as e:
        check("stop_on_error raises", False,
              f"expected RuntimeError, got {type(e).__name__}: {e}")


# ---------------------------------------------------------------------------
# Run all
# ---------------------------------------------------------------------------


def main():
    global pass_count, fail_count
    pass_count = 0
    fail_count = 0

    tests = [
        ("execute_subtask basic", test_execute_subtask_basic),
        ("execute_subtask special chars", test_execute_subtask_special_chars),
        ("execute_subtask empty", test_execute_subtask_empty),
        ("fan-out single subtask", test_fan_out_single_subtask),
        ("fan-out multiple subtasks", test_fan_out_multiple_subtasks),
        ("fan-out whitespace handling", test_fan_out_whitespace),
        ("fan-out empty task", test_fan_out_empty_task),
        ("fan-out whitespace only", test_fan_out_whitespace_only),
        ("fan-out JSON decode round-trip", test_fan_out_json_decode),
        ("child error collected", test_child_error_collected),
        ("stop-on-error raises", test_stop_on_error_raises),
    ]

    print("=" * 60)
    print("Cleat Parallel Work - Test Suite")
    print("=" * 60)
    print()

    for name, fn in tests:
        print(f"[{name}]")
        try:
            fn()
        except Exception as e:
            fail_count += 1
            print(f"  FAIL: {name} crashed: {e}")
            traceback.print_exc()
        print()

    print("=" * 60)
    total = pass_count + fail_count
    print(f"Results: {pass_count}/{total} passed, {fail_count} failed")
    print("=" * 60)

    return 0 if fail_count == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
