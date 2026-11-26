"""``cleat_sdk.vet`` warning for a wait that blocks the worker (cleat#4403).

PY017 (a WARNING, never an error): ``select.select`` and a ``queue.Queue.get`` that can block, in code a workflow executes.

Measured 2026-11-26 on cleat develop ``4f2addca`` (PostgreSQL, macOS arm64, a worker run directly, componentize-py 0.25.1): ``select.select([], [], [], t)``
blocked for the whole timeout in the host (0.44 CPU-s over 5 s, one ``--concurrency`` slot, one OS thread, and ``--wasm-instance-timeout 250ms`` did
not interrupt a 1 s call: 1.11 to 1.16 s); ``queue.Queue().get(timeout=t)`` busy-looped in the guest (5.17 CPU-s over 5 s, one slot) and the instance
timeout did stop it (0.35 s). Neither was flagged by vet before this.

What these tests pin is the RULE: which spellings are flagged, which are not (a ``get`` that does not block, ``get_nowait``, a queue that is not read
as one), and what the message tells the author. The measurements are in the issue.
"""

from __future__ import annotations

import textwrap

import pytest

from cleat_sdk.vet import analyze_file


def _analyze(tmp_path, source: str):
    f = tmp_path / "wf.py"
    f.write_text(textwrap.dedent(source))
    return analyze_file(str(f))


def _py017(result) -> list[dict]:
    return [w for w in result.warnings if w["code"] == "PY017"]


def _wrap(body: str, top: str = "") -> str:
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
# select.select
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "top, body",
    [
        ("import select\n", "select.select([], [], [], 1.0)"),
        ("import select as s\n", "s.select([], [], [], 1.0)"),
        ("from select import select\n", "select([], [], [], 1.0)"),
        ("from select import select as wait\n", "wait([], [], [], 1.0)"),
    ],
)
def test_select_select_is_flagged_under_every_spelling(tmp_path, top, body) -> None:
    result = _analyze(tmp_path, _wrap(body, top=top))
    warnings = _py017(result)
    assert len(warnings) == 1, result.warnings
    assert "select.select()" in warnings[0]["message"]
    assert "cleat#4403" in warnings[0]["message"]
    assert "h.sleep(" in warnings[0]["suggestion"]
    assert not result.errors, "a warning, never an error: it must not newly refuse a build that runs today"


def test_select_in_a_helper_the_entry_calls_is_flagged_and_outside_the_workflow_is_not(tmp_path) -> None:
    flagged = _analyze(
        tmp_path,
        'import select\nfrom cleat_sdk import HostCalls, cleat_entry\n'
        "def helper(h: HostCalls):\n    select.select([], [], [], 1)\n"
        '@cleat_entry("wf")\ndef wf(h: HostCalls, request: dict) -> str:\n    helper(h)\n    return ""\n',
    )
    assert len(_py017(flagged)) == 1
    unused = _analyze(
        tmp_path,
        'import select\nfrom cleat_sdk import HostCalls, cleat_entry\n'
        "def never_called():\n    select.select([], [], [], 1)\n"
        '@cleat_entry("wf")\ndef wf(h: HostCalls, request: dict) -> str:\n    return ""\n',
    )
    assert not _py017(unused), "code no entry point reaches is not what the workflow executes"


def test_a_module_called_select_that_is_not_the_stdlib_one_is_not_flagged(tmp_path) -> None:
    result = _analyze(tmp_path, _wrap("db.select('x')", top="import db\n"))
    assert not _py017(result)


# ---------------------------------------------------------------------------
# queue.Queue.get
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "top, body",
    [
        ("import queue\n", "queue.Queue().get(timeout=1.0)"),
        ("import queue\n", "q = queue.Queue()\nq.get(timeout=1.0)"),
        ("import queue\n", "q = queue.Queue()\nq.get()"),
        ("import queue\n", "q = queue.Queue()\nq.get(True, 1.0)"),
        ("import queue\n", "q = queue.LifoQueue()\nq.get(timeout=1.0)"),
        ("import queue\n", "q = queue.PriorityQueue()\nq.get(timeout=1.0)"),
        ("import queue as qq\n", "q = qq.Queue()\nq.get(timeout=1.0)"),
        ("from queue import Queue\n", "q = Queue()\nq.get(timeout=1.0)"),
        ("from queue import Queue as Q\n", "q = Q(maxsize=3)\nq.get(timeout=1.0)"),
        ("from queue import SimpleQueue\n", "q = SimpleQueue()\nq.get(timeout=1.0)"),
    ],
)
def test_a_queue_get_that_can_block_is_flagged(tmp_path, top, body) -> None:
    result = _analyze(tmp_path, _wrap(body, top=top))
    warnings = _py017(result)
    assert len(warnings) == 1, result.warnings
    assert "queue.Queue.get()" in warnings[0]["message"]
    assert "busy-loops" in warnings[0]["message"] and "cleat#4403" in warnings[0]["message"]
    assert not result.errors


@pytest.mark.parametrize(
    "body",
    [
        "q = queue.Queue()\nq.get(False)",
        "q = queue.Queue()\nq.get(block=False)",
        "q = queue.Queue()\nq.get(block=0)",
        "q = queue.Queue()\nq.get(timeout=0)",
        "q = queue.Queue()\nq.get(True, 0)",
        "q = queue.Queue()\nq.get(0)",
        "q = queue.Queue()\nq.get_nowait()",
        "q = queue.Queue()\nq.put(1)",
    ],
)
def test_a_queue_read_that_does_not_block_is_not_flagged(tmp_path, body) -> None:
    result = _analyze(tmp_path, _wrap(body, top="import queue\n"))
    assert not _py017(result), result.warnings


def test_get_on_something_that_is_not_a_queue_is_not_flagged(tmp_path) -> None:
    """``dict.get`` is everywhere; only a name bound to a queue constructor, or the constructor call, is read as a queue."""
    result = _analyze(tmp_path, _wrap("d = {}\nd.get('k', 1)\nrequest.get('timeout', 5)", top="import queue\n"))
    assert not _py017(result), result.warnings


def test_a_queue_held_as_an_attribute_is_not_recognised(tmp_path) -> None:
    """The documented limit, with the parameter case: ``self.q`` is not a name bound to a queue constructor in the file."""
    result = _analyze(tmp_path, _wrap("holder.q.get(timeout=1)", top="import queue\n"))
    assert not _py017(result)


def test_a_queue_that_arrives_as_a_parameter_is_not_recognised(tmp_path) -> None:
    """The documented limit: this is a name-level reading, not type inference."""
    result = _analyze(tmp_path, _wrap("request['q'].get(timeout=1)", top="import queue\n"))
    assert not _py017(result)


# ---------------------------------------------------------------------------
# Existing rules are untouched
# ---------------------------------------------------------------------------


def test_the_durable_sleep_is_not_flagged(tmp_path) -> None:
    result = _analyze(tmp_path, _wrap("h.sleep(1.0)", top="import select, queue\n"))
    assert not _py017(result)
    assert not result.errors
