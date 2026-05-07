"""High-level Python wrapper around the cleat WASM host function imports.

Provides the :class:`HostCalls` class that wraps all 29 WASM host function
imports in Pythonic methods matching the patterns from:

* Rust: ``crates/durable-sdk/src/host_calls.rs``
* AssemblyScript: ``packages/cleat-as/assembly/host-calls.ts``
* Java: ``crates/durable-java/src/main/java/cleat/HostCalls.java``
* ABI: ``ABI.md``

Usage::

    from cleat_sdk.host_calls import HostCalls, RetryPolicy

    host = HostCalls()

    # Recorded API call
    resp = host.durable_call("payment", "charge", {"amount": 100})

    # Durable sleep (suspends workflow on fresh execution)
    host.durable_sleep(5000)

    # Signal handling
    result = host.await_signals(["payment_received"], 30000)

    # Child workflows
    run_id = host.child_workflow("order_processor", {"order_id": "ord-42"})
    result = host.await_child(run_id)

**MVP note:** The 29 module-level ``_import_*`` functions are stubs that raise
:exc:`NotImplementedError`.  They are replaced by actual WASM FFI functions
when the SDK runs inside a cleat WASM runtime.  The stubs allow the SDK to be
imported and tested without WASM.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from typing import Any, Callable, Optional, TypeVar

from .memory import (
    OUT_BUF_SIZE,
    OUTPUT_OFFSET,
    SCRATCH_BASE,
    SLEEP_STATUS_SUSPEND,
    SUSPEND_SENTINEL,
    decode_await_promise_result,
    decode_await_signals_result,
    decode_durable_call_result,
    decode_poll_cancellation_result,
    decode_poll_signal_result,
    decode_simple_result,
    decode_sleep_result,
    read_string,
    write_string,
)

T = TypeVar("T")

# ========================================================================
# SuspendSentinel exception
# ========================================================================


class SuspendSentinel(Exception):
    """Raised when the workflow must suspend (e.g., for a timer or signal).

    The export wrapper catches this exception and returns the
    ``SUSPEND_SENTINEL`` value (``1 << 62``) to the host runtime, allowing
    the workflow to be resumed later.
    """

    pass


# ========================================================================
# Result dataclasses
# ========================================================================


@dataclass
class RetryPolicy:
    """Configuration for server-side retry of ``durable_call_with_retry``."""

    max_attempts: int = 3
    """Maximum number of retry attempts (including the original call)."""

    initial_interval_ms: int = 1000
    """Initial retry interval in milliseconds."""

    backoff_coefficient: float = 2.0
    """Backoff multiplier applied after each retry (e.g., 2.0 = double)."""

    max_interval_ms: int = 30000
    """Maximum retry interval in milliseconds."""

    non_retryable_errors: list[str] = field(default_factory=list)
    """Error code strings that should NOT be retried."""


@dataclass
class SignalResult:
    """Result of an :meth:`HostCalls.await_signals` call."""

    name: str
    """Name of the received signal (empty if timed out)."""

    payload: str
    """Payload of the received signal (empty if timed out)."""

    timed_out: bool
    """``True`` if the timeout expired before any signal arrived."""


@dataclass
class ChildResult:
    """Result of a single child workflow from :meth:`HostCalls.await_all_children`."""

    run_id: str
    """The child workflow's run ID."""

    result: str
    """The child's output JSON (empty if the child errored)."""

    error: Optional[str] = None
    """Error message if the child failed, or ``None`` on success."""


@dataclass
class PromiseResult:
    """Result of an :meth:`HostCalls.await_promise` call."""

    result: str
    """The resolved promise value (empty if timed out)."""

    timed_out: bool
    """``True`` if the timeout expired before the promise resolved."""

    rejected: bool = False
    """``True`` if the promise was rejected (i.e., the external caller
    called ``reject_promise`` instead of ``resolve_promise``)."""


# ========================================================================
# 22 WASM host import stubs
#
# Each stub raises NotImplementedError and is documented as a placeholder
# for the actual WASM FFI import that gets linked when running in a cleat
# WASM runtime.
# ========================================================================


# -- 1. durable_sleep ---------------------------------------------------------


def _import_durable_sleep(duration_ms: int) -> int:
    """Stub for WASM import ``(import "env" "durable_sleep") (param i64) (result i64)``.

    Replaced by actual FFI in the WASM runtime.
    """
    raise NotImplementedError(
        "durable_sleep can only be called within a cleat WASM runtime. "
        "This stub exists for import-time and test-time use."
    )


# -- 2. durable_now -----------------------------------------------------------


def _import_durable_now() -> int:
    """Stub for WASM import ``(import "env" "durable_now") (result i64)``."""
    raise NotImplementedError(
        "durable_now can only be called within a cleat WASM runtime."
    )


# -- 3. durable_random --------------------------------------------------------


def _import_durable_random() -> int:
    """Stub for WASM import ``(import "env" "durable_random") (result i64)``."""
    raise NotImplementedError(
        "durable_random can only be called within a cleat WASM runtime."
    )


# -- 4. durable_log -----------------------------------------------------------


def _import_durable_log(msg_ptr: int, msg_len: int) -> int:
    """Stub for WASM import ``(import "env" "durable_log") (param i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_log can only be called within a cleat WASM runtime."
    )


# -- 5. durable_version -------------------------------------------------------


def _import_durable_version() -> int:
    """Stub for WASM import ``(import "env" "durable_version") (result i64)``."""
    raise NotImplementedError(
        "durable_version can only be called within a cleat WASM runtime."
    )


# -- 6. durable_min_version ---------------------------------------------------


def _import_durable_min_version() -> int:
    """Stub for WASM import ``(import "env" "durable_min_version") (result i64)``."""
    raise NotImplementedError(
        "durable_min_version can only be called within a cleat WASM runtime."
    )


# -- 7. durable_defer ---------------------------------------------------------


def _import_durable_defer(
    desc_ptr: int,
    desc_len: int,
    defer_id_ptr: int,
    defer_id_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_defer") (param i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_defer can only be called within a cleat WASM runtime."
    )


# -- 8. durable_poll_cancellation ---------------------------------------------


def _import_durable_poll_cancellation(reason_ptr: int, reason_max_len: int) -> int:
    """Stub for WASM import ``(import "env" "durable_poll_cancellation") (param i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_poll_cancellation can only be called within a cleat WASM runtime."
    )


# -- 9. durable_poll_signal ---------------------------------------------------


def _import_durable_poll_signal(
    name_ptr: int,
    name_len: int,
    payload_ptr: int,
    payload_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_poll_signal") (param i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_poll_signal can only be called within a cleat WASM runtime."
    )


# -- 10. durable_continue_as_new ----------------------------------------------


def _import_durable_continue_as_new(input_ptr: int, input_len: int) -> int:
    """Stub for WASM import ``(import "env" "durable_continue_as_new") (param i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_continue_as_new can only be called within a cleat WASM runtime."
    )


# -- 11. durable_child_workflow -----------------------------------------------


def _import_durable_child_workflow(
    name_ptr: int,
    name_len: int,
    input_ptr: int,
    input_len: int,
    run_id_ptr: int,
    run_id_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_child_workflow") (param i32 i32 i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_child_workflow can only be called within a cleat WASM runtime."
    )


# -- 12. durable_await_child --------------------------------------------------


def _import_durable_await_child(
    run_id_ptr: int,
    run_id_len: int,
    result_ptr: int,
    result_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_await_child") (param i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_await_child can only be called within a cleat WASM runtime."
    )


# -- 13. durable_await_signals ------------------------------------------------


def _import_durable_await_signals(
    names_ptr: int,
    names_len: int,
    timeout_ms: int,
    sig_name_ptr: int,
    sig_name_max_len: int,
    payload_ptr: int,
    payload_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_await_signals") (param i32 i32 i64 i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_await_signals can only be called within a cleat WASM runtime."
    )


# -- 14. set_query_state ------------------------------------------------------


def _import_set_query_state(
    key_ptr: int,
    key_len: int,
    val_ptr: int,
    val_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "set_query_state") (param i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "set_query_state can only be called within a cleat WASM runtime."
    )


# -- 15. durable_call ---------------------------------------------------------


def _import_durable_call(
    svc_ptr: int,
    svc_len: int,
    op_ptr: int,
    op_len: int,
    req_ptr: int,
    req_len: int,
    resp_ptr: int,
    resp_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_call") (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_call can only be called within a cleat WASM runtime."
    )


# -- 16. durable_call_retry ---------------------------------------------------


def _import_durable_call_retry(
    svc_ptr: int,
    svc_len: int,
    op_ptr: int,
    op_len: int,
    req_ptr: int,
    req_len: int,
    max_attempts: int,
    initial_interval_ms: int,
    backoff_coefficient_100x: int,
    max_interval_ms: int,
    non_retryable_errors_ptr: int,
    non_retryable_errors_len: int,
    resp_ptr: int,
    resp_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_call_retry") (param i32 i32 i32 i32 i32 i32 i64 i64 i64 i64 i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_call_retry can only be called within a cleat WASM runtime."
    )


# -- 17. durable_call_heartbeat -----------------------------------------------


def _import_durable_call_heartbeat(
    svc_ptr: int,
    svc_len: int,
    op_ptr: int,
    op_len: int,
    req_ptr: int,
    req_len: int,
    heartbeat_interval_ms: int,
    resp_ptr: int,
    resp_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_call_heartbeat") (param i32 i32 i32 i32 i32 i32 i64 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_call_heartbeat can only be called within a cleat WASM runtime."
    )


# -- 18. durable_await_all_children -------------------------------------------


def _import_durable_await_all_children(
    run_ids_json_ptr: int,
    run_ids_json_len: int,
    results_ptr: int,
    results_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_await_all_children") (param i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_await_all_children can only be called within a cleat WASM runtime."
    )


# -- 19. plugin_call ----------------------------------------------------------


def _import_plugin_call(
    plugin_name_ptr: int,
    plugin_name_len: int,
    function_name_ptr: int,
    function_name_len: int,
    input_ptr: int,
    input_len: int,
    response_ptr: int,
    response_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "plugin_call") (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "plugin_call can only be called within a cleat WASM runtime."
    )


# -- 20. durable_create_promise -----------------------------------------------


def _import_durable_create_promise(
    name_ptr: int,
    name_len: int,
    id_out_ptr: int,
    id_out_max: int,
    ttl_ms: int = 0,
) -> int:
    """Stub for WASM import ``(import "env" "durable_create_promise") (param i32 i32 i32 i32 i64) (result i64)``."""
    raise NotImplementedError(
        "durable_create_promise can only be called within a cleat WASM runtime."
    )


# -- 21. durable_await_promise ------------------------------------------------


def _import_durable_await_promise(
    id_ptr: int,
    id_len: int,
    timeout_ms: int,
    result_out_ptr: int,
    result_out_max: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_await_promise") (param i32 i32 i64 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_await_promise can only be called within a cleat WASM runtime."
    )


# -- 22. durable_resolve_promise -----------------------------------------------


def _import_durable_resolve_promise(
    id_ptr: int,
    id_len: int,
    value_ptr: int,
    value_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_resolve_promise") (param i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_resolve_promise can only be called within a cleat WASM runtime."
    )


# -- 23. durable_reject_promise ------------------------------------------------


def _import_durable_reject_promise(
    id_ptr: int,
    id_len: int,
    error_ptr: int,
    error_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_reject_promise") (param i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_reject_promise can only be called within a cleat WASM runtime."
    )


# -- 22. durable_register_update_handler --------------------------------------


def _import_durable_register_update_handler(name_ptr: int, name_len: int) -> int:
    """Stub for WASM import ``(import "env" "durable_register_update_handler") (param i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_register_update_handler can only be called within a cleat WASM runtime."
    )


# -- 23. durable_workflow_id ---------------------------------------------------


def _import_durable_workflow_id(
    id_ptr: int,
    id_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_workflow_id") (param i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_workflow_id can only be called within a cleat WASM runtime."
    )


# -- 24. durable_run_id --------------------------------------------------------


def _import_durable_run_id(
    id_ptr: int,
    id_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_run_id") (param i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_run_id can only be called within a cleat WASM runtime."
    )


# -- 25. durable_send ---------------------------------------------------------


def _import_durable_send(
    svc_ptr: int,
    svc_len: int,
    op_ptr: int,
    op_len: int,
    req_ptr: int,
    req_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_send") (param i32 i32 i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_send can only be called within a cleat WASM runtime."
    )


# -- 26. durable_schedule_invoke ----------------------------------------------


def _import_durable_schedule_invoke(
    svc_ptr: int,
    svc_len: int,
    op_ptr: int,
    op_len: int,
    req_ptr: int,
    req_len: int,
    delay_ms: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_schedule_invoke") (param i32 i32 i32 i32 i32 i32 i64) (result i64)``."""
    raise NotImplementedError(
        "durable_schedule_invoke can only be called within a cleat WASM runtime."
    )


# -- 27. durable_register_query_handler -------------------------------------------


def _import_durable_register_query_handler(name_ptr: int, name_len: int) -> int:
    """Stub for WASM import ``(import "env" "durable_register_query_handler") (param i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_register_query_handler can only be called within a cleat WASM runtime."
    )


# -- 28. durable_send_signal_and_wait ----------------------------------------------


def _import_durable_send_signal_and_wait(
    target_run_id_ptr: int,
    target_run_id_len: int,
    signal_name_ptr: int,
    signal_name_len: int,
    payload_ptr: int,
    payload_len: int,
    timeout_ms: int,
    response_ptr: int,
    response_max_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_send_signal_and_wait") (param i32 i32 i32 i32 i32 i32 i64 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_send_signal_and_wait can only be called within a cleat WASM runtime."
    )


# -- 29. durable_reply_to_signal ---------------------------------------------------


def _import_durable_reply_to_signal(
    correlation_id_ptr: int,
    correlation_id_len: int,
    response_ptr: int,
    response_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_reply_to_signal") (param i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_reply_to_signal can only be called within a cleat WASM runtime."
    )


# -- 30. durable_signal_workflow ---------------------------------------------------


def _import_durable_signal_workflow(
    target_run_id_ptr: int,
    target_run_id_len: int,
    signal_name_ptr: int,
    signal_name_len: int,
    payload_ptr: int,
    payload_len: int,
) -> int:
    """Stub for WASM import ``(import "env" "durable_signal_workflow") (param i32 i32 i32 i32 i32 i32) (result i64)``."""
    raise NotImplementedError(
        "durable_signal_workflow can only be called within a cleat WASM runtime."
    )


# ========================================================================
# HostCalls — high-level wrapper
# ========================================================================


class HostCalls:
    """High-level Python wrapper around all 29 cleat WASM host function imports.

    Each method handles string I/O (encoding input strings to the scratch
    buffer, decoding output strings from the output buffer), calls the
    corresponding raw import, and decodes the bit-packed result into an
    idiomatic Python return value.

    Reference implementations:
        * Rust: ``crates/durable-sdk/src/host_calls.rs``
        * AssemblyScript: ``packages/cleat-as/assembly/host-calls.ts``
        * Java: ``crates/durable-java/src/main/java/cleat/HostCalls.java``
    """

    def __init__(self) -> None:
        """Initialize the HostCalls instance."""
        self._update_handlers: dict[str, tuple[Callable[[str], str], Optional[Callable[[str], bool]]]] = {}
        self._query_handlers: dict[str, Callable[[str], str]] = {}
        self._scope_prefix: str = ""

    # --------------------------------------------------------------------
    # Internal helpers
    # --------------------------------------------------------------------

    @staticmethod
    def _marshal(value: Any) -> str:
        """Convert *value* to a JSON string suitable for WASM host calls.

        Strings are passed through unchanged; all other types are
        ``json.dumps``'d.
        """
        if isinstance(value, str):
            return value
        return json.dumps(value)

    # --------------------------------------------------------------------
    # Scope management for virtual object instances
    # --------------------------------------------------------------------

    def _scoped_key(self, key: str) -> str:
        """Apply the current scope prefix to *key*, if a scope is active."""
        if self._scope_prefix:
            return self._scope_prefix + key
        return key

    def set_scope(self, object_type: str, instance_key: str) -> str:
        """Set the state key prefix for virtual object instances.

        All subsequent ``set_state``/``get_state``/etc calls are
        automatically prefixed with ``"vo:<object_type>:<instance_key>:"``.
        Returns the previous scope prefix for stack-style save/restore.

        Parameters
        ----------
        object_type : str
            The virtual object type name.
        instance_key : str
            The instance key for this specific object.

        Returns
        -------
        str
            The previous scope prefix (empty string if none was set).
        """
        prev = self._scope_prefix
        self._scope_prefix = (
            f"vo:{object_type}:{instance_key}:"
            if object_type and instance_key
            else ""
        )
        return prev

    def get_scope(self) -> tuple[str, str]:
        """Get the current virtual object scope.

        Returns
        -------
        tuple[str, str]
            ``(object_type, instance_key)`` or ``("", "")`` if no scope is
            active.
        """
        if not self._scope_prefix:
            return "", ""
        # Parse "vo:<type>:<key>:" format
        prefix = self._scope_prefix.rstrip(":")
        parts = prefix.split(":", 2)
        if len(parts) == 3 and parts[0] == "vo":
            return parts[1], parts[2]
        return "", ""

    def clear_scope(self) -> str:
        """Remove the current scope and return the previous scope prefix.

        Returns
        -------
        str
            The scope prefix that was active before clearing (empty string
            if none was set).
        """
        prev = self._scope_prefix
        self._scope_prefix = ""
        return prev

    # --------------------------------------------------------------------
    # UUID — deterministic ID generation
    # --------------------------------------------------------------------

    def uuid(self, seed: str) -> str:
        """Return a deterministic UUID scoped to the current workflow
        and the given *seed*. The same seed always produces the same UUID
        for this workflow instance.

        Useful for generating predictable entity IDs, correlation IDs, or
        other identifiers that must be stable across workflow replays.

        Parameters
        ----------
        seed : str
            A seed string that determines the UUID within this workflow.

        Returns
        -------
        str
            A UUID-formatted string (e.g. ``"550e8400-e29b-... "``).
        """
        wf_id = self.current_workflow_id()
        data = (wf_id + ":" + seed).encode("utf-8")
        h = hashlib.sha256(data).digest()[:16]
        # Set UUIDv5 version and variant bits
        h_bytes = bytearray(h)
        h_bytes[6] = (h_bytes[6] & 0x0f) | 0x50  # Version 5
        h_bytes[8] = (h_bytes[8] & 0x3f) | 0x80  # Variant 1
        return (
            f"{h_bytes[0:4].hex()}-"
            f"{h_bytes[4:6].hex()}-"
            f"{h_bytes[6:8].hex()}-"
            f"{h_bytes[8:10].hex()}-"
            f"{h_bytes[10:16].hex()}"
        )

    # --------------------------------------------------------------------
    # 1. now — wall-clock time
    # --------------------------------------------------------------------

    def now(self) -> int:
        """Get the current wall-clock time.

        Returns the time in milliseconds since the Unix epoch.  The same
        value is returned on replay as during the original execution,
        ensuring deterministic re-execution.

        Returns
        -------
        int
            Milliseconds since Unix epoch (full 64-bit value).
        """
        return _import_durable_now()

    # --------------------------------------------------------------------
    # 2. random — deterministic random value
    # --------------------------------------------------------------------

    def random(self) -> int:
        """Get a deterministic random value.

        The same value is returned on replay as during the original
        execution, ensuring deterministic re-execution.

        Returns
        -------
        int
            A deterministic 64-bit random value.
        """
        return _import_durable_random()

    # --------------------------------------------------------------------
    # 4. current_workflow_id — get workflow identity
    # --------------------------------------------------------------------

    def current_workflow_id(self) -> str:
        """Get the current workflow's unique identifier.

        Returns
        -------
        str
            The workflow ID string.
        """
        result = _import_durable_workflow_id(OUTPUT_OFFSET, OUT_BUF_SIZE)
        id_len, err_code = decode_simple_result(result)
        if err_code != 0:
            raise RuntimeError(
                f"current_workflow_id failed with error code: {err_code}"
            )
        return read_string(OUTPUT_OFFSET, id_len)

    # --------------------------------------------------------------------
    # 5. current_run_id — get current run identity
    # --------------------------------------------------------------------

    def current_run_id(self) -> str:
        """Get the current workflow run's unique identifier.

        Returns
        -------
        str
            The run ID string.
        """
        result = _import_durable_run_id(OUTPUT_OFFSET, OUT_BUF_SIZE)
        id_len, err_code = decode_simple_result(result)
        if err_code != 0:
            raise RuntimeError(
                f"current_run_id failed with error code: {err_code}"
            )
        return read_string(OUTPUT_OFFSET, id_len)

    # --------------------------------------------------------------------
    # 6. version — workflow definition version
    # --------------------------------------------------------------------

    def version(self) -> int:
        """Get the workflow definition version.

        Returns
        -------
        int
            The current workflow version as a 32-bit integer.
        """
        return _import_durable_version()

    # --------------------------------------------------------------------
    # 4. min_version — minimum supported version
    # --------------------------------------------------------------------

    def min_version(self) -> int:
        """Get the minimum supported version for this workflow definition.

        Returns
        -------
        int
            The minimum version as a 32-bit integer.
        """
        return _import_durable_min_version()

    # --------------------------------------------------------------------
    # 5. durable_log — log a message
    # --------------------------------------------------------------------

    def durable_log(self, message: str) -> None:
        """Log a message to the workflow event history.

        Log messages are recorded deterministically and replayed during
        workflow re-execution.  This is intended for debugging and
        observability, not for side-effect logic.

        Parameters
        ----------
        message : str
            The log message to record.
        """
        msg_len = write_string(SCRATCH_BASE, message, OUT_BUF_SIZE)
        _import_durable_log(SCRATCH_BASE, msg_len)

    # --------------------------------------------------------------------
    # 5b. log_kv — structured key-value log
    # --------------------------------------------------------------------

    def log_kv(self, message: str, *kvs: Any) -> None:
        """Log a structured key-value message to the event history.

        Key-value pairs are formatted as alternating key=value entries
        appended to the message, separated by newlines.  Intended for
        structured observability data that aids debugging during replay.

        Parameters
        ----------
        message : str
            The main log message.
        *kvs : Any
            Alternating key-value pairs for structured logging (e.g.
            ``"user_id", "u-42", "status", "active"``).  An odd count
            results in the trailing key having an empty value.
        """
        if kvs:
            parts = [message]
            for i in range(0, len(kvs), 2):
                key = kvs[i]
                val = kvs[i + 1] if i + 1 < len(kvs) else ""
                parts.append(f"  {key}={val}")
            formatted = "\n".join(parts)
        else:
            formatted = message
        self.durable_log(formatted)

    # --------------------------------------------------------------------
    # 6. durable_call — recorded API call
    # --------------------------------------------------------------------

    def durable_call(self, service: str, operation: str, request: Any) -> str:
        """Make a durable (deterministically replayed) call to an external service.

        The call is recorded in the workflow event history.  On replay, the
        recorded response is returned without making the real call, ensuring
        deterministic re-execution.

        Parameters
        ----------
        service : str
            Service name (e.g., ``"payment"``, ``"email"``).
        operation : str
            Operation name (e.g., ``"charge"``, ``"send"``).
        request : Any
            Request payload.  If a :class:`dict`, it is JSON-serialised
            automatically.  Strings are passed through as-is.

        Returns
        -------
        str
            The response JSON string.

        Raises
        ------
        RuntimeError
            If the host reports an error from the service call.
        """
        req_str = self._marshal(request)

        svc_len = write_string(SCRATCH_BASE, service, OUT_BUF_SIZE)
        op_offset = SCRATCH_BASE + svc_len
        remaining = OUT_BUF_SIZE - svc_len
        op_len = write_string(op_offset, operation, remaining)
        req_offset = op_offset + op_len
        remaining -= op_len
        req_len = write_string(req_offset, req_str, remaining)

        result = _import_durable_call(
            SCRATCH_BASE,
            svc_len,
            op_offset,
            op_len,
            req_offset,
            req_len,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
        )

        response_len, call_error_code, err_code = decode_durable_call_result(result)
        if err_code != 0:
            err_msg = read_string(OUTPUT_OFFSET, response_len)
            raise RuntimeError(f"durable_call failed: {err_msg}")

        return read_string(OUTPUT_OFFSET, response_len)

    # --------------------------------------------------------------------
    # 7. durable_call_typed — typed recorded API call
    # --------------------------------------------------------------------

    def durable_call_typed(
        self, service: str, operation: str, request: Any, result_type: type[T]
    ) -> T:
        """Make a durable call and deserialise the JSON response.

        Parameters
        ----------
        service : str
            Service name.
        operation : str
            Operation name.
        request : Any
            Request payload (dict or str).
        result_type : type[T]
            Target type for deserialisation.  If the response JSON is an
            object, ``result_type(**data)`` is used.  Otherwise
            ``result_type(data)`` is used.

        Returns
        -------
        T
            An instance of *result_type* constructed from the response JSON.

        Raises
        ------
        RuntimeError
            If the host reports an error from the service call.
        """
        response = self.durable_call(service, operation, request)
        data = json.loads(response)
        if isinstance(data, dict):
            return result_type(**data)
        return result_type(data)

    # --------------------------------------------------------------------
    # 8. durable_call_with_retry — server-side retry
    # --------------------------------------------------------------------

    def durable_call_with_retry(
        self, service: str, operation: str, request: Any, retry: RetryPolicy
    ) -> str:
        """Make a durable call with server-side retry.

        Retries happen inside the host; one event is recorded regardless of
        attempt count.

        Parameters
        ----------
        service : str
            Service name.
        operation : str
            Operation name.
        request : Any
            Request payload (dict or str).
        retry : RetryPolicy
            Retry configuration.

        Returns
        -------
        str
            The response JSON string.

        Raises
        ------
        RuntimeError
            If all retry attempts are exhausted and the host reports an
            error.
        """
        req_str = self._marshal(request)
        non_retryable_str = json.dumps(retry.non_retryable_errors)
        backoff_100x = round(retry.backoff_coefficient * 100)

        svc_len = write_string(SCRATCH_BASE, service, OUT_BUF_SIZE)
        op_offset = SCRATCH_BASE + svc_len
        remaining = OUT_BUF_SIZE - svc_len
        op_len = write_string(op_offset, operation, remaining)
        req_offset = op_offset + op_len
        remaining -= op_len
        req_len = write_string(req_offset, req_str, remaining)
        nre_offset = req_offset + req_len
        remaining -= req_len
        nre_len = write_string(nre_offset, non_retryable_str, remaining)

        result = _import_durable_call_retry(
            SCRATCH_BASE,
            svc_len,
            op_offset,
            op_len,
            req_offset,
            req_len,
            retry.max_attempts,
            retry.initial_interval_ms,
            backoff_100x,
            retry.max_interval_ms,
            nre_offset,
            nre_len,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
        )

        response_len, call_error_code, err_code = decode_durable_call_result(result)
        if err_code != 0:
            err_msg = read_string(OUTPUT_OFFSET, response_len)
            raise RuntimeError(f"durable_call_with_retry failed: {err_msg}")

        return read_string(OUTPUT_OFFSET, response_len)

    # --------------------------------------------------------------------
    # 9. durable_call_with_heartbeat — long-running call with progress
    # --------------------------------------------------------------------

    def durable_call_with_heartbeat(
        self,
        service: str,
        operation: str,
        request: Any,
        heartbeat_interval_ms: int,
        progress: Callable[[str], None],
    ) -> str:
        """Make a durable call with periodic heartbeat / progress updates.

        The host sends periodic progress updates while the call is running.
        Each progress update is delivered to the *progress* callback as a
        JSON string.  (In the current MVP the callback is accepted but not
       invoked by the stub; it will be wired in a future runtime.)

        Parameters
        ----------
        service : str
            Service name.
        operation : str
            Operation name.
        request : Any
            Request payload (dict or str).
        heartbeat_interval_ms : int
            Heartbeat interval in milliseconds.
        progress : Callable[[str], None]
            Callback invoked with progress JSON strings from the host.

        Returns
        -------
        str
            The final response JSON string.

        Raises
        ------
        RuntimeError
            If the host reports an error from the service call.
        """
        req_str = self._marshal(request)

        svc_len = write_string(SCRATCH_BASE, service, OUT_BUF_SIZE)
        op_offset = SCRATCH_BASE + svc_len
        remaining = OUT_BUF_SIZE - svc_len
        op_len = write_string(op_offset, operation, remaining)
        req_offset = op_offset + op_len
        remaining -= op_len
        req_len = write_string(req_offset, req_str, remaining)

        result = _import_durable_call_heartbeat(
            SCRATCH_BASE,
            svc_len,
            op_offset,
            op_len,
            req_offset,
            req_len,
            heartbeat_interval_ms,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
        )

        response_len, call_error_code, err_code = decode_durable_call_result(result)
        if err_code != 0:
            err_msg = read_string(OUTPUT_OFFSET, response_len)
            raise RuntimeError(f"durable_call_with_heartbeat failed: {err_msg}")

        return read_string(OUTPUT_OFFSET, response_len)

    # --------------------------------------------------------------------
    # 10. durable_sleep — suspend for a duration
    # --------------------------------------------------------------------

    def durable_sleep(self, duration_ms: int) -> bool:
        """Suspend workflow execution for a duration.

        On fresh execution the host signals suspension and this method
        raises :exc:`SuspendSentinel`.  On replay the sleep has already
        completed and ``False`` is returned.

        Parameters
        ----------
        duration_ms : int
            Sleep duration in milliseconds.

        Returns
        -------
        bool
            Always ``False`` on replay (suspend scenarios raise
            :exc:`SuspendSentinel` instead).

        Raises
        ------
        SuspendSentinel
            If the workflow should suspend (fresh execution).
        """
        result = _import_durable_sleep(duration_ms)

        # Some host runtimes return SUSPEND_SENTINEL directly.
        if result == SUSPEND_SENTINEL:
            raise SuspendSentinel()

        status, _ = decode_sleep_result(result)
        if status == SLEEP_STATUS_SUSPEND:
            raise SuspendSentinel()

        return False

    # --------------------------------------------------------------------
    # 11. durable_fetch — durable HTTP fetch (convenience)
    # --------------------------------------------------------------------

    def durable_fetch(
        self,
        url: str,
        method: str = "GET",
        headers: Optional[dict] = None,
        body: str = "",
    ) -> tuple:
        """Perform a durable HTTP fetch via the ``"http"`` service.

        This is a convenience method that delegates to :meth:`durable_call`
        with the ``"http"`` service and ``"fetch"`` operation.

        Parameters
        ----------
        url : str
            The URL to fetch.
        method : str
            HTTP method (``"GET"``, ``"POST"``, etc.).  Default ``"GET"``.
        headers : dict or None
            Optional HTTP headers.
        body : str
            Request body for POST/PUT requests.  Default ``""``.

        Returns
        -------
        tuple[str, int]
            ``(response_body, status_code)``.
        """
        request = {"url": url, "method": method, "headers": headers or {}, "body": body}
        result = self.durable_call("http", "fetch", request)
        data = json.loads(result)
        return data.get("body", ""), data.get("status", 200)

    # --------------------------------------------------------------------
    # 11b. durable_fetch_json — fetch with JSON deserialization
    # --------------------------------------------------------------------

    def durable_fetch_json(
        self,
        url: str,
        method: str = "GET",
        headers: Optional[dict] = None,
        body: str = "",
        result_type: type[T] = dict,
    ) -> T:
        """Perform a durable HTTP fetch and deserialize the JSON response.

        Like :meth:`durable_fetch` but deserializes the response body into
        the specified result type.

        Parameters
        ----------
        url : str
            The URL to fetch.
        method : str
            HTTP method (``"GET"``, ``"POST"``, etc.).  Default ``"GET"``.
        headers : dict or None
            Optional HTTP headers.
        body : str
            Request body for POST/PUT requests.  Default ``""``.
        result_type : type[T]
            Target type for deserialisation.  If the response JSON is an
            object, ``result_type(**data)`` is used.

        Returns
        -------
        T
            An instance of *result_type* constructed from the response.
        """
        resp_body, status = self.durable_fetch(url, method, headers, body)
        data = json.loads(resp_body)
        if isinstance(data, dict) and result_type is not dict:
            return result_type(**data)
        return data  # type: ignore[return-value]

    # --------------------------------------------------------------------
    # 11c. fetch_get — shorthand GET
    # --------------------------------------------------------------------

    def fetch_get(self, url: str) -> tuple:
        """Shorthand for a durable GET request via :meth:`durable_fetch`.

        Parameters
        ----------
        url : str
            The URL to fetch.

        Returns
        -------
        tuple[str, int]
            ``(response_body, status_code)``.
        """
        return self.durable_fetch(url, "GET")

    # --------------------------------------------------------------------
    # 11d. fetch_get_json — shorthand GET with JSON deserialization
    # --------------------------------------------------------------------

    def fetch_get_json(
        self,
        url: str,
        result_type: type[T] = dict,
    ) -> T:
        """Shorthand for a durable GET request with JSON deserialization.

        Parameters
        ----------
        url : str
            The URL to fetch.
        result_type : type[T]
            Target type for deserialisation.

        Returns
        -------
        T
            An instance of *result_type* constructed from the response.
        """
        return self.durable_fetch_json(url, "GET", result_type=result_type)

    # --------------------------------------------------------------------
    # 12. await_signals — wait for signals
    # --------------------------------------------------------------------

    def await_signals(self, signal_names: list[str], timeout_ms: int) -> SignalResult:
        """Wait for one or more external signals, with a timeout.

        Blocks until one of the named signals is received or the timeout
        expires.

        Parameters
        ----------
        signal_names : list[str]
            List of signal names to wait for.
        timeout_ms : int
            Maximum wait time in milliseconds.

        Returns
        -------
        SignalResult
            The received signal name, payload, and timeout indicator.

        Raises
        ------
        SuspendSentinel
            If the host indicates the workflow should suspend (no signal
            ready and a non-zero timeout was specified).
        RuntimeError
            If the host reports an error.
        """
        names_json = json.dumps(signal_names)

        # Lower half of scratch buffer: signal names JSON.
        # Upper half of scratch buffer: signal payload output.
        names_len = write_string(SCRATCH_BASE, names_json, OUT_BUF_SIZE // 2)
        payload_offset = SCRATCH_BASE + OUT_BUF_SIZE // 2
        payload_max = OUT_BUF_SIZE // 2

        result = _import_durable_await_signals(
            SCRATCH_BASE,
            names_len,
            timeout_ms,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
            payload_offset,
            payload_max,
        )

        # Host returns SUSPEND_SENTINEL when no signal is available and a
        # non-zero timeout was specified.
        if result == SUSPEND_SENTINEL:
            raise SuspendSentinel()

        sig_name_len, payload_len, timed_out, err_code = decode_await_signals_result(
            result
        )
        if err_code != 0:
            raise RuntimeError(
                f"await_signals failed with error code: {err_code}"
            )

        sig_name = (
            read_string(OUTPUT_OFFSET, sig_name_len) if sig_name_len > 0 else ""
        )
        payload = (
            read_string(payload_offset, payload_len)
            if not timed_out and payload_len > 0
            else ""
        )

        return SignalResult(name=sig_name, payload=payload, timed_out=timed_out)

    # --------------------------------------------------------------------
    # 13. poll_signal — non-blocking signal check
    # --------------------------------------------------------------------

    def poll_signal(self, name: str) -> tuple:
        """Poll for a specific pending signal (non-blocking).

        Unlike :meth:`await_signals`, this call checks once and returns
        immediately.  If the signal is not pending, ``(payload, found)``
        will have *found* as ``False``.

        Parameters
        ----------
        name : str
            The signal name to poll for.

        Returns
        -------
        tuple[str, bool]
            ``(payload, found)`` where *found* is ``True`` if a signal was
            pending and *payload* contains its data.

        Raises
        ------
        RuntimeError
            If the host reports an internal error.
        """
        name_len = write_string(SCRATCH_BASE, name, OUT_BUF_SIZE)

        result = _import_durable_poll_signal(
            SCRATCH_BASE,
            name_len,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
        )

        payload_len, found, err_code = decode_poll_signal_result(result)
        if err_code != 0:
            raise RuntimeError(f"poll_signal failed with error code: {err_code}")

        payload = (
            read_string(OUTPUT_OFFSET, payload_len)
            if found and payload_len > 0
            else ""
        )

        return (payload, found)

    # --------------------------------------------------------------------
    # 14. poll_cancellation — check for cancellation
    # --------------------------------------------------------------------

    def poll_cancellation(self) -> tuple:
        """Check if workflow cancellation has been requested.

        Workflows should periodically check for cancellation and perform
        cleanup if cancelled.

        Returns
        -------
        tuple[bool, str]
            ``(cancelled, reason)`` where *cancelled* is ``True`` if
            cancellation was requested and *reason* is the cancellation
            reason string (may be empty).
        """
        result = _import_durable_poll_cancellation(OUTPUT_OFFSET, OUT_BUF_SIZE)

        reason_len, cancelled = decode_poll_cancellation_result(result)
        reason = (
            read_string(OUTPUT_OFFSET, reason_len) if cancelled and reason_len > 0 else ""
        )

        return (cancelled, reason)

    # --------------------------------------------------------------------
    # 15. child_workflow — start a child workflow
    # --------------------------------------------------------------------

    def child_workflow(self, name: str, input: Any) -> str:
        """Start a child workflow instance.

        The child runs asynchronously.  Use :meth:`await_child` or
        :meth:`await_all_children` to wait for completion.

        Parameters
        ----------
        name : str
            Child workflow definition name.
        input : Any
            Input for the child workflow.  Dicts are JSON-serialised
            automatically; strings are passed through.

        Returns
        -------
        str
            The child workflow's run ID.

        Raises
        ------
        RuntimeError
            If the host reports an error starting the child.
        """
        input_str = self._marshal(input)

        name_len = write_string(SCRATCH_BASE, name, OUT_BUF_SIZE)
        input_offset = SCRATCH_BASE + name_len
        remaining = OUT_BUF_SIZE - name_len
        input_len = write_string(input_offset, input_str, remaining)

        result = _import_durable_child_workflow(
            SCRATCH_BASE,
            name_len,
            input_offset,
            input_len,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
        )

        run_id_len, err_code = decode_simple_result(result)
        if err_code != 0:
            raise RuntimeError(
                f"child_workflow failed with error code: {err_code}"
            )

        return read_string(OUTPUT_OFFSET, run_id_len)

    # --------------------------------------------------------------------
    # 16. await_child — wait for a child workflow
    # --------------------------------------------------------------------

    def await_child(self, run_id: str) -> str:
        """Wait for a child workflow to complete.

        If the child has not yet completed, the workflow suspends.

        Parameters
        ----------
        run_id : str
            The child workflow's run ID (from :meth:`child_workflow`).

        Returns
        -------
        str
            The child's output JSON.

        Raises
        ------
        SuspendSentinel
            If the child has not yet completed and the workflow should
            suspend.
        RuntimeError
            If the host reports an error.
        """
        run_id_len = write_string(SCRATCH_BASE, run_id, OUT_BUF_SIZE)

        result = _import_durable_await_child(
            SCRATCH_BASE,
            run_id_len,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
        )

        # The host returns SUSPEND_SENTINEL when the child has not completed.
        if result == SUSPEND_SENTINEL:
            raise SuspendSentinel()

        result_len, err_code = decode_simple_result(result)
        if err_code != 0:
            raise RuntimeError(
                f"await_child failed with error code: {err_code}"
            )

        return read_string(OUTPUT_OFFSET, result_len)

    # --------------------------------------------------------------------
    # 17. await_all_children — batch await for children
    # --------------------------------------------------------------------

    def await_all_children(self, run_ids: list[str]) -> list[ChildResult]:
        """Wait for multiple child workflows to complete (batch).

        Parameters
        ----------
        run_ids : list[str]
            List of child workflow run IDs.

        Returns
        -------
        list[ChildResult]
            Results for each child workflow, in the order of *run_ids*.

        Raises
        ------
        RuntimeError
            If the host reports an error.
        """
        run_ids_json = json.dumps(run_ids)
        run_ids_len = write_string(SCRATCH_BASE, run_ids_json, OUT_BUF_SIZE)

        result = _import_durable_await_all_children(
            SCRATCH_BASE,
            run_ids_len,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
        )

        result_len, err_code = decode_simple_result(result)
        if err_code != 0:
            raise RuntimeError(
                f"await_all_children failed with error code: {err_code}"
            )

        results_json = read_string(OUTPUT_OFFSET, result_len)
        if not results_json:
            return []

        results_data = json.loads(results_json)
        return [ChildResult(**item) for item in results_data]

    # --------------------------------------------------------------------
    # 18. set_query_state — set queryable key-value state
    # --------------------------------------------------------------------

    def set_query_state(self, key: str, value: str) -> None:
        """Set a key-value pair in the workflow's queryable state.

        External clients can query this state while the workflow is running
        or after completion.

        Parameters
        ----------
        key : str
            Query state key.
        value : str
            Query state value (typically a JSON string).
        """
        key_len = write_string(SCRATCH_BASE, key, OUT_BUF_SIZE)
        val_offset = SCRATCH_BASE + key_len
        remaining = OUT_BUF_SIZE - key_len
        val_len = write_string(val_offset, value, remaining)

        _import_set_query_state(SCRATCH_BASE, key_len, val_offset, val_len)

    # --------------------------------------------------------------------
    # 19. set_state — set typed durable state
    # --------------------------------------------------------------------

    def set_state(self, key: str, value: Any) -> None:
        """Set typed durable state (marshals *value* to JSON).

        Internally delegates to the ``"state"`` service via
        :meth:`durable_call`.

        Parameters
        ----------
        key : str
            State key.  If a scope is active via :meth:`set_scope`, the
            key is automatically prefixed with the scope prefix.
        value : Any
            State value.  Dicts and lists are JSON-serialised automatically.
        """
        self.durable_call("state", "set", {"key": self._scoped_key(key), "value": value})

    # --------------------------------------------------------------------
    # 20. get_state — get typed durable state
    # --------------------------------------------------------------------

    def get_state(self, key: str, result_type: type[T]) -> T:
        """Get typed durable state, deserialised into *result_type*.

        Internally delegates to the ``"state"`` service via
        :meth:`durable_call`.

        Parameters
        ----------
        key : str
            State key.
        result_type : type[T]
            Target type for deserialisation.  If the stored value is a JSON
            object, ``result_type(**data)`` is used.

        Returns
        -------
        T
            An instance of *result_type* constructed from the stored state.
        """
        result = self.durable_call("state", "get", {"key": key})
        data = json.loads(result)
        if isinstance(data, dict):
            return result_type(**data)
        return result_type(data)

    # --------------------------------------------------------------------
    # 21. delete_state — delete durable state
    # --------------------------------------------------------------------

    def delete_state(self, key: str) -> None:
        """Delete a durable state key.

        Internally delegates to the ``"state"`` service via
        :meth:`durable_call`.

        Parameters
        ----------
        key : str
            State key to delete.
        """
        self.durable_call("state", "delete", {"key": key})

    # --------------------------------------------------------------------
    # 22. incr_state — atomically increment numeric state
    # --------------------------------------------------------------------

    def incr_state(self, key: str, delta: int = 1) -> int:
        """Atomically increment a numeric durable state value.

        Internally delegates to the ``"state"`` service via
        :meth:`durable_call`.

        Parameters
        ----------
        key : str
            State key to increment.
        delta : int
            Amount to increment by (default ``1``).

        Returns
        -------
        int
            The new value after incrementing.
        """
        result = self.durable_call(
            "state", "incr", {"key": key, "delta": delta}
        )
        return int(json.loads(result))

    # --------------------------------------------------------------------
    # 23. has_state — check if a state key exists
    # --------------------------------------------------------------------

    def has_state(self, key: str) -> bool:
        """Check if a durable state key exists.

        Internally delegates to the ``"state"`` service via
        :meth:`durable_call`.

        Parameters
        ----------
        key : str
            State key to check.

        Returns
        -------
        bool
            ``True`` if the key exists in durable state.
        """
        result = self.durable_call(
            "state", "has", {"key": self._scoped_key(key)}
        )
        return bool(json.loads(result))

    # --------------------------------------------------------------------
    # 24. list_state — list state keys by prefix
    # --------------------------------------------------------------------

    def list_state(self, prefix: str = "") -> list[str]:
        """List all durable state keys matching the given prefix.

        Internally delegates to the ``"state"`` service via
        :meth:`durable_call`.

        Parameters
        ----------
        prefix : str
            Optional prefix to filter keys by.  Empty string lists all
            keys in the current scope.

        Returns
        -------
        list[str]
            List of matching state keys.
        """
        inp: dict = {"prefix": prefix}
        result = self.durable_call("state", "list", inp)
        return json.loads(result)

    # --------------------------------------------------------------------
    # 25. create_promise — create a durable promise
    # --------------------------------------------------------------------

    def create_promise(self, name: str, ttl_ms: Optional[int] = None) -> str:
        """Create a durable promise with the given name.

        The host allocates a promise ID that can be used to resolve or
        await the promise.

        Parameters
        ----------
        name : str
            The promise name.
        ttl_ms : int or None
            Optional time-to-live in milliseconds.  The promise is
            automatically garbage-collected after this duration.
            ``None`` means no TTL (host default).

        Returns
        -------
        str
            The promise ID.

        Raises
        ------
        RuntimeError
            If the host reports an error.
        """
        name_len = write_string(SCRATCH_BASE, name, OUT_BUF_SIZE)

        result = _import_durable_create_promise(
            SCRATCH_BASE,
            name_len,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
            ttl_ms or 0,
        )

        id_len, err_code = decode_simple_result(result)
        if err_code != 0:
            raise RuntimeError(
                f"create_promise failed with error code: {err_code}"
            )

        return read_string(OUTPUT_OFFSET, id_len)

    # --------------------------------------------------------------------
    # 24. await_promise — await a durable promise
    # --------------------------------------------------------------------

    def await_promise(self, promise_id: str, timeout_ms: int) -> PromiseResult:
        """Wait for a durable promise to resolve, with a timeout.

        Parameters
        ----------
        promise_id : str
            The promise ID to wait for.
        timeout_ms : int
            Maximum wait time in milliseconds.

        Returns
        -------
        PromiseResult
            The resolved value, timeout indicator, and rejection flag.

        Raises
        ------
        RuntimeError
            If the host reports an error.
        """
        id_len = write_string(SCRATCH_BASE, promise_id, OUT_BUF_SIZE)

        result = _import_durable_await_promise(
            SCRATCH_BASE,
            id_len,
            timeout_ms,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
        )

        result_len, timed_out, rejected, err_code = decode_await_promise_result(result)
        if err_code != 0:
            raise RuntimeError(
                f"await_promise failed with error code: {err_code}"
            )

        result_str = (
            read_string(OUTPUT_OFFSET, result_len) if result_len > 0 else ""
        )
        return PromiseResult(
            result=result_str,
            timed_out=timed_out,
            rejected=rejected,
        )

    # --------------------------------------------------------------------
    # 25. resolve_promise — resolve a durable promise
    # --------------------------------------------------------------------

    def resolve_promise(self, promise_id: str, value: str) -> None:
        """Resolve a durable promise with a value.

        External callers can use this to fulfill a promise that the workflow
        is awaiting.

        Parameters
        ----------
        promise_id : str
            The promise ID to resolve.
        value : str
            The JSON value to resolve the promise with.

        Raises
        ------
        RuntimeError
            If the host reports an error.
        """
        id_len = write_string(SCRATCH_BASE, promise_id, OUT_BUF_SIZE)
        val_offset = SCRATCH_BASE + id_len
        remaining = OUT_BUF_SIZE - id_len
        val_len = write_string(val_offset, value, remaining)

        result = _import_durable_resolve_promise(
            SCRATCH_BASE,
            id_len,
            val_offset,
            val_len,
        )

        _, err_code = decode_simple_result(result)
        if err_code != 0:
            raise RuntimeError(
                f"resolve_promise failed with error code: {err_code}"
            )

    # --------------------------------------------------------------------
    # 26. reject_promise — reject a durable promise
    # --------------------------------------------------------------------

    def reject_promise(self, promise_id: str, error: str) -> None:
        """Reject a durable promise with an error.

        External callers can use this to reject a promise that the workflow
        is awaiting.  The workflow will receive the promise result with
        ``rejected=True``.

        Parameters
        ----------
        promise_id : str
            The promise ID to reject.
        error : str
            The error message to reject the promise with.

        Raises
        ------
        RuntimeError
            If the host reports an error.
        """
        id_len = write_string(SCRATCH_BASE, promise_id, OUT_BUF_SIZE)
        err_offset = SCRATCH_BASE + id_len
        remaining = OUT_BUF_SIZE - id_len
        err_len = write_string(err_offset, error, remaining)

        result = _import_durable_reject_promise(
            SCRATCH_BASE,
            id_len,
            err_offset,
            err_len,
        )

        _, err_code = decode_simple_result(result)
        if err_code != 0:
            raise RuntimeError(
                f"reject_promise failed with error code: {err_code}"
            )

    # --------------------------------------------------------------------
    # 25. register_update_handler — register an update handler
    # --------------------------------------------------------------------

    def register_update_handler(
        self,
        name: str,
        handler: Callable[[str], str],
        validator: Optional[Callable[[str], bool]] = None,
    ) -> None:
        """Register a handler for update calls on this workflow.

        Update handlers allow external clients to send update requests to
        the workflow while it is executing.  The handler receives a JSON
        payload string and returns a JSON result string.  An optional
        validator can be provided to validate the payload before the
        handler is invoked.

        Parameters
        ----------
        name : str
            The update handler name.
        handler : Callable[[str], str]
            Handler function that takes a JSON payload string and returns
            a JSON result string.
        validator : Callable[[str], bool] or None
            Optional validator function that takes a JSON payload string
            and returns True if the payload is valid.
        """
        self._update_handlers[name] = (handler, validator)
        name_len = write_string(SCRATCH_BASE, name, OUT_BUF_SIZE)
        _import_durable_register_update_handler(SCRATCH_BASE, name_len)

    def _handle_update(self, name: str, payload: str) -> str:
        """Internal: look up and invoke a registered update handler.

        Parameters
        ----------
        name : str
            The update handler name.
        payload : str
            The JSON payload string.

        Returns
        -------
        str
            The handler's JSON result string.

        Raises
        ------
        RuntimeError
            If no handler is registered for the given name.
        """
        entry = self._update_handlers.get(name)
        if entry is None:
            raise RuntimeError(
                f"No update handler registered for '{name}'"
            )
        handler, _ = entry
        return handler(payload)

    def _validate_update(self, name: str, payload: str) -> bool:
        """Internal: look up and invoke a registered update validator.

        Parameters
        ----------
        name : str
            The update handler name.
        payload : str
            The JSON payload string.

        Returns
        -------
        bool
            True if the payload is valid (or no validator is registered).
            False if the validator rejects the payload.
        """
        entry = self._update_handlers.get(name)
        if entry is None:
            return False
        _, validator = entry
        if validator is None:
            return True
        return validator(payload)

    # --------------------------------------------------------------------
    # 27. register_query_handler — register a read-only query handler
    # --------------------------------------------------------------------

    def register_query_handler(
        self,
        name: str,
        handler: Callable[[str], str],
    ) -> None:
        """Register a read-only handler for query calls on this workflow.

        Query handlers allow external clients to read workflow state without
        journaling.  Unlike update handlers, query handlers are deterministic
        and read-only.

        Parameters
        ----------
        name : str
            The query handler name.
        handler : Callable[[str], str]
            Handler function that takes a JSON payload string and returns
            a JSON result string.
        """
        self._query_handlers[name] = handler
        name_len = write_string(SCRATCH_BASE, name, OUT_BUF_SIZE)
        _import_durable_register_query_handler(SCRATCH_BASE, name_len)

    def _handle_query(self, name: str, payload: str) -> str:
        """Internal: look up and invoke a registered query handler.

        Parameters
        ----------
        name : str
            The query handler name.
        payload : str
            The JSON payload string.

        Returns
        -------
        str
            The handler's JSON result string.

        Raises
        ------
        RuntimeError
            If no handler is registered for the given name.
        """
        handler = self._query_handlers.get(name)
        if handler is None:
            raise RuntimeError(
                f"No query handler registered for '{name}'"
            )
        return handler(payload)

    # --------------------------------------------------------------------
    # 26. durable_defer — register deferred cleanup
    # --------------------------------------------------------------------

    def durable_defer(self, description: str) -> str:
        """Register a deferred cleanup action to run on workflow exit.

        Deferred actions are executed in LIFO order, analogous to Python's
        ``try/finally`` or Go's ``defer``.

        Parameters
        ----------
        description : str
            Human-readable description of the cleanup action.

        Returns
        -------
        str
            The defer ID, which can be used to cancel the deferred action.

        Raises
        ------
        RuntimeError
            If the host reports an error.
        """
        desc_len = write_string(SCRATCH_BASE, description, OUT_BUF_SIZE)

        result = _import_durable_defer(
            SCRATCH_BASE,
            desc_len,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
        )

        id_len, err_code = decode_simple_result(result)
        if err_code != 0:
            raise RuntimeError(f"durable_defer failed with error code: {err_code}")

        return read_string(OUTPUT_OFFSET, id_len)

    # --------------------------------------------------------------------
    # 27. continue_as_new — history compaction
    # --------------------------------------------------------------------

    def continue_as_new(self, input: Any) -> None:
        """Replace the workflow's input and restart execution from scratch.

        This is used for workflow history compaction.  After this call the
        workflow should raise :exc:`SuspendSentinel` to let the host
        restart it with the new input.

        Parameters
        ----------
        input : Any
            New input for the restarted workflow.  Dicts are JSON-serialised
            automatically.

        Raises
        ------
        RuntimeError
            If the host reports an error.
        """
        input_str = self._marshal(input)
        input_len = write_string(SCRATCH_BASE, input_str, OUT_BUF_SIZE)

        result = _import_durable_continue_as_new(SCRATCH_BASE, input_len)

        _, err_code = decode_simple_result(result)
        if err_code != 0:
            raise RuntimeError(
                f"continue_as_new failed with error code: {err_code}"
            )

    # --------------------------------------------------------------------
    # 28. run_detached — execute detached from cancellation
    # --------------------------------------------------------------------

    def run_detached(self, fn: Callable[["HostCalls"], Any]) -> None:
        """Execute a function that is detached from workflow cancellation.

        The function receives this ``HostCalls`` instance so it can make
        host calls.  In a full WASM runtime the host would ensure the
        detached execution continues even if the parent workflow is
        cancelled.

        Parameters
        ----------
        fn : Callable[[HostCalls], Any]
            Function to execute in a detached context.
        """
        fn(self)

    # --------------------------------------------------------------------
    # 29. durable_send — fire-and-forget
    # --------------------------------------------------------------------

    def durable_send(self, service: str, operation: str, request: Any) -> None:
        """Send a fire-and-forget request to an external service.

        Unlike :meth:`durable_call`, this method does not wait for a
        response.  The request is enqueued and the workflow continues
        immediately.

        Parameters
        ----------
        service : str
            Service name (e.g., ``"email"``, ``"notification"``).
        operation : str
            Operation name (e.g., ``"send"``, ``"notify"``).
        request : Any
            Request payload.  Dicts are JSON-serialised automatically.
        """
        req_str = self._marshal(request)

        svc_len = write_string(SCRATCH_BASE, service, OUT_BUF_SIZE)
        op_offset = SCRATCH_BASE + svc_len
        remaining = OUT_BUF_SIZE - svc_len
        op_len = write_string(op_offset, operation, remaining)
        req_offset = op_offset + op_len
        remaining -= op_len
        req_len = write_string(req_offset, req_str, remaining)

        _import_durable_send(
            SCRATCH_BASE,
            svc_len,
            op_offset,
            op_len,
            req_offset,
            req_len,
        )

    # --------------------------------------------------------------------
    # 30. schedule_invoke — delayed one-shot
    # --------------------------------------------------------------------

    def schedule_invoke(
        self,
        service: str,
        operation: str,
        request: Any,
        delay_ms: int,
    ) -> None:
        """Schedule a delayed one-shot invocation.

        The request is enqueued and will be delivered to the service
        after the specified delay.  The workflow continues immediately
        without waiting for the invocation to complete.

        Parameters
        ----------
        service : str
            Service name.
        operation : str
            Operation name.
        request : Any
            Request payload.  Dicts are JSON-serialised automatically.
        delay_ms : int
            Delay in milliseconds before the invocation is sent.
        """
        req_str = self._marshal(request)

        svc_len = write_string(SCRATCH_BASE, service, OUT_BUF_SIZE)
        op_offset = SCRATCH_BASE + svc_len
        remaining = OUT_BUF_SIZE - svc_len
        op_len = write_string(op_offset, operation, remaining)
        req_offset = op_offset + op_len
        remaining -= op_len
        req_len = write_string(req_offset, req_str, remaining)

        _import_durable_schedule_invoke(
            SCRATCH_BASE,
            svc_len,
            op_offset,
            op_len,
            req_offset,
            req_len,
            delay_ms,
        )

    # --------------------------------------------------------------------
    # 31. plugin_call — call a plugin host function
    # --------------------------------------------------------------------

    def plugin_call(
        self, plugin_name: str, function_name: str, input: Any
    ) -> str:
        """Call a plugin host function.

        Plugins extend the host runtime with custom functionality beyond
        the standard 22 host imports.

        Parameters
        ----------
        plugin_name : str
            Name of the plugin.
        function_name : str
            Name of the function to call within the plugin.
        input : Any
            Input for the plugin function.  Dicts are JSON-serialised
            automatically.

        Returns
        -------
        str
            The plugin function's response JSON.

        Raises
        ------
        RuntimeError
            If the host reports an error from the plugin call.
        """
        input_str = self._marshal(input)

        pn_len = write_string(SCRATCH_BASE, plugin_name, OUT_BUF_SIZE)
        fn_offset = SCRATCH_BASE + pn_len
        remaining = OUT_BUF_SIZE - pn_len
        fn_len = write_string(fn_offset, function_name, remaining)
        inp_offset = fn_offset + fn_len
        remaining -= fn_len
        inp_len = write_string(inp_offset, input_str, remaining)

        result = _import_plugin_call(
            SCRATCH_BASE,
            pn_len,
            fn_offset,
            fn_len,
            inp_offset,
            inp_len,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
        )

        response_len, call_error_code, err_code = decode_durable_call_result(result)
        if err_code != 0:
            err_msg = read_string(OUTPUT_OFFSET, response_len)
            raise RuntimeError(f"plugin_call failed: {err_msg}")

        return read_string(OUTPUT_OFFSET, response_len)

    # --------------------------------------------------------------------
    # 32. send_signal_and_wait — send a signal and wait for a response
    # --------------------------------------------------------------------

    def send_signal_and_wait(
        self,
        target_run_id: str,
        signal_name: str,
        payload: str,
        timeout_ms: int,
    ) -> str:
        """Send a signal to a target workflow and wait for a response.

        The signal carries an embedded correlation ID. The target workflow
        can call :meth:`reply_to_signal` to send a response back.

        Parameters
        ----------
        target_run_id : str
            The target workflow's run ID.
        signal_name : str
            The signal name to send.
        payload : str
            The signal payload as a JSON string (will be enriched with a
            correlation ID).
        timeout_ms : int
            Maximum wait time in milliseconds for the response.

        Returns
        -------
        str
            The response payload from the target workflow.

        Raises
        ------
        RuntimeError
            If the host reports an error or the timeout expires.
        """
        target_len = write_string(SCRATCH_BASE, target_run_id, OUT_BUF_SIZE)
        sig_offset = SCRATCH_BASE + target_len
        remaining = OUT_BUF_SIZE - target_len
        sig_len = write_string(sig_offset, signal_name, remaining)
        payload_offset = sig_offset + sig_len
        remaining -= sig_len
        payload_len = write_string(payload_offset, self._marshal(payload), remaining)

        result = _import_durable_send_signal_and_wait(
            SCRATCH_BASE,
            target_len,
            sig_offset,
            sig_len,
            payload_offset,
            payload_len,
            timeout_ms,
            OUTPUT_OFFSET,
            OUT_BUF_SIZE,
        )

        response_len, err_code = decode_simple_result(result)
        if err_code != 0:
            err_msg = read_string(OUTPUT_OFFSET, response_len)
            raise RuntimeError(f"send_signal_and_wait failed: {err_msg}")

        return read_string(OUTPUT_OFFSET, response_len)

    # --------------------------------------------------------------------
    # 33. reply_to_signal — respond to a signal from within a handler
    # --------------------------------------------------------------------

    def reply_to_signal(self, correlation_id: str, response: str) -> None:
        """Send a response back to the sender of a signal.

        Only valid inside a signal handler context where the correlation ID
        was embedded in the received signal payload (via
        :meth:`send_signal_and_wait`).

        Parameters
        ----------
        correlation_id : str
            The correlation ID from the received signal's payload.
        response : str
            The response payload as a JSON string.

        Raises
        ------
        RuntimeError
            If the host reports an error.
        """
        cid_len = write_string(SCRATCH_BASE, correlation_id, OUT_BUF_SIZE)
        resp_offset = SCRATCH_BASE + cid_len
        remaining = OUT_BUF_SIZE - cid_len
        resp_len = write_string(resp_offset, response, remaining)

        result = _import_durable_reply_to_signal(
            SCRATCH_BASE,
            cid_len,
            resp_offset,
            resp_len,
        )

        _, err_code = decode_simple_result(result)
        if err_code != 0:
            raise RuntimeError(
                f"reply_to_signal failed with error code: {err_code}"
            )

    # --------------------------------------------------------------------
    # 34. await_signals_with_quorum — wait for quorum of signals
    # --------------------------------------------------------------------

    def await_signals_with_quorum(
        self,
        signal_names: list[str],
        min_count: int,
        max_rejections: int,
        timeout_ms: int,
    ) -> list[SignalResult]:
        """Wait for at least ``min_count`` signals from the named set.

        Collects signals until ``min_count`` is reached, ``max_rejections``
        is exceeded (if >= 0), or the timeout expires.

        Parameters
        ----------
        signal_names : list[str]
            List of signal names to wait for.
        min_count : int
            Minimum number of signals required to proceed.
        max_rejections : int
            Maximum number of rejection signals tolerated before aborting.
            Set to -1 to disable rejection tracking.
        timeout_ms : int
            Maximum wait time in milliseconds.

        Returns
        -------
        list[SignalResult]
            The collected signals.

        Raises
        ------
        RuntimeError
            If the timeout expires or max rejections is exceeded.
        """
        deadline_ns = time.monotonic_ns() + timeout_ms * 1_000_000
        results: list[SignalResult] = []
        rejection_count = 0

        while len(results) < min_count:
            remaining_ms = max(0, (deadline_ns - time.monotonic_ns()) // 1_000_000)

            if remaining_ms <= 0:
                raise RuntimeError(
                    f"quorum timeout: got {len(results)}/{min_count} signals"
                )

            result = self.await_signals(signal_names, remaining_ms)
            if result.timed_out:
                raise RuntimeError(
                    f"quorum timeout: got {len(results)}/{min_count} signals"
                )
            results.append(result)

            # Check for rejection if max_rejections >= 0.
            if max_rejections >= 0 and result.payload:
                import json
                try:
                    payload_data = json.loads(result.payload)
                    if isinstance(payload_data, dict) and payload_data.get("rejected"):
                        rejection_count += 1
                        if rejection_count > max_rejections:
                            raise RuntimeError(
                                f"quorum exceeded max rejections ({max_rejections})"
                            )
                except (json.JSONDecodeError, TypeError):
                    pass

        return results

    # --------------------------------------------------------------------
    # 35. signal_workflow — send a signal to another workflow
    # --------------------------------------------------------------------

    def signal_workflow(
        self,
        target_run_id: str,
        signal_name: str,
        payload: Any,
    ) -> None:
        """Send a signal to a target workflow (fire-and-forget).

        Unlike :meth:`send_signal_and_wait`, this method does not wait for a
        response.  The signal is enqueued and the workflow continues
        immediately.  This is a recorded (journaled) operation.

        Parameters
        ----------
        target_run_id : str
            The target workflow's run ID.
        signal_name : str
            The signal name to send.
        payload : Any
            The signal payload. Dicts are JSON-serialised automatically.
        """
        payload_str = self._marshal(payload)

        target_len = write_string(SCRATCH_BASE, target_run_id, OUT_BUF_SIZE)
        sig_offset = SCRATCH_BASE + target_len
        remaining = OUT_BUF_SIZE - target_len
        sig_len = write_string(sig_offset, signal_name, remaining)
        payload_offset = sig_offset + sig_len
        remaining -= sig_len
        payload_len = write_string(payload_offset, payload_str, remaining)

        result = _import_durable_signal_workflow(
            SCRATCH_BASE,
            target_len,
            sig_offset,
            sig_len,
            payload_offset,
            payload_len,
        )

        _, err_code = decode_simple_result(result)
        if err_code != 0:
            raise RuntimeError(
                f"signal_workflow failed with error code: {err_code}"
            )
