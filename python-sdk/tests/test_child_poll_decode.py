"""``poll_child`` and ``await_any_child`` decode the document the HOST writes.

cleat#3390. On a worker both calls return a JSON string
(``engine/children.go``): ``PollChild`` writes ``{"status": "running" |
"completed" | "failed", "result": ..., "error": ...}`` and ``AwaitAnyChild``
writes ``{"run_id": ..., "result": ..., "error": ...}``.

``poll_child`` used to build ``ChildResult(**data)`` from the first of those. The
document has a ``status`` key ``ChildResult`` does not accept and no ``run_id``
it requires, so every finished child raised ``TypeError``. The existing tests
could not see it: they run against ``LocalHost``, which constructs a
``ChildResult`` directly and never touches the host's document.
"""

import json

import pytest

import cleat_sdk.host_calls as host_calls
from cleat_sdk.host_calls import ChildResult, HostCalls


def _poll(monkeypatch: pytest.MonkeyPatch, text: str, run_id: str = "run-1") -> ChildResult | None:
    monkeypatch.setattr(host_calls, "_import_cleat_poll_child", lambda rid: text, raising=False)
    return HostCalls().poll_child(run_id)


def _await_any(monkeypatch: pytest.MonkeyPatch, text: str) -> ChildResult:
    monkeypatch.setattr(host_calls, "_import_cleat_await_any_child", lambda ids: text, raising=False)
    return HostCalls().await_any_child(["run-a", "run-b"])


class TestPollChildDecodesTheHostsDocument:
    def test_a_completed_child(self, monkeypatch: pytest.MonkeyPatch) -> None:
        got = _poll(monkeypatch, json.dumps({"status": "completed", "result": '{"ok":true}'}))
        assert got == ChildResult(run_id="run-1", result='{"ok":true}', error=None)

    def test_a_failed_child_carries_its_error(self, monkeypatch: pytest.MonkeyPatch) -> None:
        got = _poll(monkeypatch, json.dumps({"status": "failed", "error": "boom"}))
        assert got == ChildResult(run_id="run-1", result="", error="boom")

    def test_a_running_child_is_none(self, monkeypatch: pytest.MonkeyPatch) -> None:
        assert _poll(monkeypatch, json.dumps({"status": "running"})) is None

    def test_an_empty_answer_is_none(self, monkeypatch: pytest.MonkeyPatch) -> None:
        assert _poll(monkeypatch, "") is None

    def test_a_host_error_message_is_reported_not_swallowed(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        """The WIT function is ``-> string``: a store error arrives as plain text."""
        with pytest.raises(RuntimeError, match="connection refused"):
            _poll(monkeypatch, "poll_child: connection refused")


class TestAwaitAnyChildDecodesTheHostsDocument:
    def test_the_winner(self, monkeypatch: pytest.MonkeyPatch) -> None:
        got = _await_any(monkeypatch, json.dumps({"run_id": "run-a", "result": '{"v":1}'}))
        assert got == ChildResult(run_id="run-a", result='{"v":1}', error=None)

    def test_a_failed_winner_carries_its_error(self, monkeypatch: pytest.MonkeyPatch) -> None:
        got = _await_any(monkeypatch, json.dumps({"run_id": "run-b", "error": "boom"}))
        assert got == ChildResult(run_id="run-b", result="", error="boom")

    def test_a_pending_await_raises_rather_than_returning_a_result(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        with pytest.raises(RuntimeError, match="returned nothing"):
            _await_any(monkeypatch, "")

    def test_a_host_error_message_is_reported(self, monkeypatch: pytest.MonkeyPatch) -> None:
        with pytest.raises(RuntimeError, match="invalid runIDs"):
            _await_any(monkeypatch, "invalid runIDs: unexpected end of JSON input")

    def test_a_host_error_document_is_reported_not_returned_as_a_child(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        """The host answers a bad run-ID list with {"error": ...} and no run_id.

        That used to come back as ChildResult(run_id="", error=...), a "winner"
        nobody started, which a caller racing children would branch on.
        """
        with pytest.raises(RuntimeError, match="invalid runIDs"):
            _await_any(monkeypatch, json.dumps({"error": "invalid runIDs: unexpected end"}))
