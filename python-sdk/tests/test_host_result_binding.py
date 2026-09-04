"""The SDK reads what the host answered.

Every host import that returns a scalar hands back a packed word carrying an
error code, and -- during a defer segment -- possibly a stop sentinel. A call
site that throws that word away cannot tell a refusal from a success. Thirteen
of them did (IMPROVEMENT-PLAN 3.201); the structural test at the bottom of this
file is what stops a fourteenth being added.

The tests patch the module-level ``_import_*`` stubs, which is the only way to
drive these paths off-WASM: outside a cleat runtime the real stubs raise
NotImplementedError long before a result could be decoded.
"""

from typing import ClassVar
from unittest import mock

import pytest

from cleat_sdk.host_calls import HostCalls, SuspendSentinel
from cleat_sdk.memory import CALL_SUSPEND_SENTINEL

# engine/memory.go: errSignalAuthRequired = 0xFFFFFFFF_00000002, returned by
# execSession.SignalWorkflow when signalAuthCheck refuses the send. The
# component dispatcher passes it through unchanged (setResultU64 in
# engine/component_callbacks.go dispatchSignalWorkflow), so this is the exact
# word a Python guest receives.
ERR_SIGNAL_AUTH_REQUIRED = 0xFFFFFFFF_00000002

# engine/lifecycle.go SetState/DeleteState: 1 when the replayed event does not
# match the call the guest is now making -- a non-determinism report.
ERR_REPLAY_DIVERGED = 1


@pytest.fixture
def hc() -> HostCalls:
    return HostCalls()


class TestARefusalIsNotReportedAsSuccess:
    """3.201's stated close condition: the refusal has to reach the workflow."""

    def test_signal_workflow_raises_when_the_host_refuses_it(self, hc: HostCalls) -> None:
        with mock.patch(
            "cleat_sdk.host_calls._import_cleat_signal_workflow",
            return_value=ERR_SIGNAL_AUTH_REQUIRED,
        ) as imp, pytest.raises(RuntimeError) as excinfo:
            hc.signal_workflow("run-1", "approve", {"ok": True})
        assert imp.called, "the import must actually be reached"
        # The message has to name the call and carry the code, or a refused
        # signal is only marginally more debuggable than a silent one.
        assert "signal_workflow" in str(excinfo.value)
        assert "run-1" in str(excinfo.value)
        assert "code 2" in str(excinfo.value)

    def test_signal_workflow_stays_silent_when_the_host_accepts_it(self, hc: HostCalls) -> None:
        with mock.patch("cleat_sdk.host_calls._import_cleat_signal_workflow", return_value=0):
            assert hc.signal_workflow("run-1", "approve", {"ok": True}) is None

    def test_a_diverged_replay_is_reported_on_state_mutation(self, hc: HostCalls) -> None:
        """SetState/DeleteState return 1 when replay diverges.

        Discarding that let a Python workflow replay non-deterministically and
        carry on against a state store the host had refused to update.
        """
        with mock.patch(
            "cleat_sdk.host_calls._import_stream_set_state", return_value=ERR_REPLAY_DIVERGED
        ), pytest.raises(RuntimeError, match="stream_set_state"):
            hc.stream_set_state("k", "v")

        with mock.patch(
            "cleat_sdk.host_calls._import_stream_delete_state", return_value=ERR_REPLAY_DIVERGED
        ), pytest.raises(RuntimeError, match="stream_delete_state"):
            hc.stream_delete_state("k")


class TestTheStopSentinelIsTestedBeforeTheWordIsDecoded:
    """Order, not just presence. See CALL_SUSPEND_SENTINEL in memory.py."""

    def test_a_stop_unwinds_the_workflow(self, hc: HostCalls) -> None:
        with mock.patch(
            "cleat_sdk.host_calls._import_cleat_send", return_value=CALL_SUSPEND_SENTINEL
        ), pytest.raises(SuspendSentinel):
            hc.send("email", "notify", {"to": "a@b.c"})

    def test_a_stop_is_not_mistaken_for_an_error_code(self, hc: HostCalls) -> None:
        """The sentinel's low byte is zero, so an err_code-only check misses it.

        This is the assertion that distinguishes 'checks the result' from
        'checks the result correctly': a call site that tested only
        ``r & 0xFF`` would pass every test above and still run past a stop.
        """
        assert CALL_SUSPEND_SENTINEL & 0xFF == 0
        with mock.patch(
            "cleat_sdk.host_calls._import_cleat_signal_workflow",
            return_value=CALL_SUSPEND_SENTINEL,
        ), pytest.raises(SuspendSentinel):
            hc.signal_workflow("run-1", "go", {})

    def test_a_stop_wins_over_an_error_code_in_the_same_word(self, hc: HostCalls) -> None:
        with mock.patch(
            "cleat_sdk.host_calls._import_cleat_send",
            return_value=CALL_SUSPEND_SENTINEL | 0x02,
        ), pytest.raises(SuspendSentinel):
            hc.send("email", "notify", {})


class TestNoScalarHostCallDiscardsItsResult:
    """The structural guard. Without it this fix decays one call site at a time.

    Parses the SDK rather than exercising it, so a newly added call site is
    caught the moment it lands rather than when someone writes a test for it.
    """

    # Both are unfixable from the SDK, for reasons recorded at the call sites.
    KNOWN_UNCHECKED: ClassVar[set[tuple[str, str]]] = {
        ("log", "_import_cleat_log"),
        ("delete_cron", "_import_cleat_delete_cron"),
    }

    def test_every_other_import_result_is_bound(self) -> None:
        import ast
        import pathlib

        import cleat_sdk.host_calls as hcmod

        src = pathlib.Path(hcmod.__file__).read_text()
        cls = next(
            n
            for n in ast.parse(src).body
            if isinstance(n, ast.ClassDef) and n.name == "HostCalls"
        )
        discarded = set()
        for fn in [n for n in cls.body if isinstance(n, ast.FunctionDef)]:
            for node in ast.walk(fn):
                # A bare expression statement whose value is an import call is
                # a discarded result -- nothing can read what it returned.
                if (
                    isinstance(node, ast.Expr)
                    and isinstance(node.value, ast.Call)
                    and isinstance(node.value.func, ast.Name)
                    and node.value.func.id.startswith("_import")
                ):
                    discarded.add((fn.name, node.value.func.id))

        assert discarded == self.KNOWN_UNCHECKED, (
            "a host import result is being discarded. Bind it and pass it to "
            "_check_host_result, or add it to KNOWN_UNCHECKED with the reason "
            f"recorded at the call site. Unexpected: {discarded - self.KNOWN_UNCHECKED}"
        )
