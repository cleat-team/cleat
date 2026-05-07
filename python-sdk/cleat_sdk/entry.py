"""
Decorator for marking functions as Cleat workflow entry points.

Generates a WASM-export-compatible wrapper following the Cleat ABI::

    (args_ptr: i32, args_len: i32, out_ptr: i32, max_out_len: i32) -> i64

The decorator reads input JSON from linear memory, deserialises it, creates a
:class:`HostCalls` instance, calls the decorated function, serialises the
result back to JSON, and returns the packed i64 result code.

Usage::

    from cleat_sdk.entry import durable_entry
    from cleat_sdk.host_calls import HostCalls

    @durable_entry("PlaceOrder")
    def place_order(h: HostCalls, user_id: str, cart: list[dict]) -> str:
        ...
"""

from __future__ import annotations

import functools
import inspect
import json
from typing import Any, Callable, Optional, get_type_hints

from .host_calls import HostCalls, SuspendSentinel
from .memory import SUSPEND_SENTINEL, encode_export_result, read_string, write_string


# ---------------------------------------------------------------------------
# Helper
# ---------------------------------------------------------------------------

def _unwrap_result(result: Any) -> Any:
    """Unwrap Result-like types to get the inner value for serialisation.

    If *result* has both ``value`` and ``error`` attributes (duck-typed
    Result), return ``.value`` when ``.error is None``, otherwise return
    ``{"error": str(.error)}``.  For all other values return the value
    unchanged.
    """
    if hasattr(result, "value") and hasattr(result, "error"):
        return result.value if result.error is None else {"error": str(result.error)}
    return result


# ---------------------------------------------------------------------------
# Decorator
# ---------------------------------------------------------------------------

def durable_entry(name: Optional[str] = None) -> Callable:
    """Mark a function as a Cleat workflow entry point.

    The decorated function **must** accept a :class:`HostCalls` instance as
    its first parameter.  Additional parameters are deserialised from the
    workflow input JSON by name.

    Parameters
    ----------
    name:
        Optional explicit export name for the workflow.  Defaults to the
        Python function name.

    Returns
    -------
    Callable
        A wrapper function with the WASM-export ABI signature::

            def wrapper(args_ptr: int, args_len: int,
                        out_ptr: int, max_out_len: int) -> int: ...

    The returned wrapper carries the attribute
    ``wrapper._is_durable_entry = True`` for introspection.
    """

    def _make_entry(func: Callable) -> Callable:
        # ---- resolve workflow parameter names (skip injected HostCalls) ----
        hints = get_type_hints(func)
        sig = inspect.signature(func)
        all_param_names = list(sig.parameters.keys())

        workflow_param_names: list[str] = []
        for pname in all_param_names:
            # The HostCalls parameter is injected by the framework and is never
            # part of the serialised input JSON.
            if pname in hints and hints[pname] is HostCalls:
                continue
            workflow_param_names.append(pname)

        # For Python < 3.10 we cannot rely on ``get_type_hints`` preserving
        # order, so we additionally reconstruct via ``inspect.signature``.
        # Union both sources to get annotated param names reliably.
        hint_names = set(hints.keys())
        all_param_set = set(all_param_names)
        # ``get_type_hints`` may omit un-annotated params; merge both.
        params_with_host = [
            p for p in all_param_names
            if p in hint_names or p not in all_param_set - hint_names
        ]
        workflow_param_names = [p for p in params_with_host
                                if not (p in hints and hints[p] is HostCalls)]

        workflow_name = name if name is not None else func.__name__

        @functools.wraps(func)
        def export_wrapper(
            args_ptr: int, args_len: int,
            out_ptr: int, max_out_len: int,
        ) -> int:
            """Cleat ABI export wrapper.

            Called by the host runtime with raw memory pointers.  Reads JSON
            input, invokes the workflow function, serialises the result, and
            returns a bit-packed i64 status code.
            """
            try:
                # (a) Read input JSON from linear memory.
                input_json = read_string(args_ptr, args_len)
                input_data: dict[str, Any] = (
                    json.loads(input_json) if input_json else {}
                )

                # (b) Validate that every required workflow parameter appears
                #     in the deserialised input.
                missing = [p for p in workflow_param_names if p not in input_data]
                if missing:
                    raise ValueError(
                        f"Missing required parameters: {', '.join(missing)}"
                    )

                # (c) Build keyword arguments from the JSON keys that match
                #     workflow parameters.
                kwargs = {
                    k: v for k, v in input_data.items()
                    if k in workflow_param_names
                }

                # (d) Create the HostCalls instance and invoke the workflow.
                h = HostCalls()
                result = func(h, **kwargs)
                result = _unwrap_result(result)

                # (e) Serialise the return value, write it to the output
                #     buffer, and return a success-packed i64.
                output_json = json.dumps(result, default=str)
                bytes_written = write_string(out_ptr, output_json, max_out_len)
                return encode_export_result(0, bytes_written)

            except SuspendSentinel:
                # The workflow signalled suspension (e.g. durable_sleep on a
                # fresh execution).  Propagate the sentinel to the host.
                return SUSPEND_SENTINEL

            except Exception as exc:
                # Any other exception is treated as a workflow error.
                error_json = json.dumps({"error": str(exc)})
                bytes_written = write_string(out_ptr, error_json, max_out_len)
                return encode_export_result(1, bytes_written)

        # Mark the wrapper for introspection tooling.
        export_wrapper._is_durable_entry = True  # type: ignore[attr-defined]

        return export_wrapper

    # ------------------------------------------------------------------
    # Support both ``@durable_entry`` (without parentheses, legacy) and
    # ``@durable_entry(...)`` (with parentheses, preferred).
    # ------------------------------------------------------------------
    if callable(name):
        # ``name`` is actually the decorated function.
        return _make_entry(name)

    return _make_entry


def virtual_object(name: Optional[str] = None) -> Callable:
    """Register a function as a virtual object entry point.

    This decorator wraps :func:`durable_entry` and marks the function as
    a virtual object handler for key-scoped stateful services.

    Usage::

        from cleat_sdk import HostCalls, virtual_object

        @virtual_object("counter")
        def counter(h: HostCalls, input: str) -> str:
            vo = HostCalls()  # or use h directly with set_scope
            ...
            return "{\"result\": \"ok\"}"

    The decorated function carries the attribute
    ``wrapper._is_virtual_object = True`` for introspection.

    Parameters
    ----------
    name:
        The virtual object type name.  Defaults to the Python function name.

    Returns
    -------
    Callable
        A wrapper function with the WASM-export ABI signature, identical
        to :func:`durable_entry` but additionally marked as a virtual object
        handler.
    """

    def _make_entry(func: Callable) -> Callable:
        entry_name = name if name is not None else func.__name__
        decorated = durable_entry(entry_name)(func)
        decorated._is_virtual_object = True  # type: ignore[attr-defined]
        return decorated

    if callable(name):
        return _make_entry(name)

    return _make_entry


def query_handler(name: Optional[str] = None) -> Callable:
    """Mark a function as a read-only query handler (no journaling).

    Unlike :func:`durable_entry`, which marks a workflow entry point that
    journalises all durable operations, this decorator marks a function as a
    read-only query handler.  Query handlers are invoked on-demand by external
    callers without recording events in the workflow history.

    The decorated function **must** accept a :class:`HostCalls` instance as
    its first parameter.  Additional parameters are deserialised from the
    query input JSON by name.

    Usage::

        from cleat_sdk import HostCalls, query_handler

        @query_handler("get_status")
        def get_status(h: HostCalls, order_id: str) -> str:
            # Read-only — no durable_call, durable_sleep, etc.
            state = h.get_state(order_id, dict)
            return json.dumps({"status": state.get("status", "unknown")})

    The decorated function carries ``wrapper._is_query_handler = True``
    and ``wrapper._is_durable_entry = False`` for introspection.

    Parameters
    ----------
    name:
        Optional explicit query name.  Defaults to the Python function name.

    Returns
    -------
    Callable
        A wrapper function that behaves like :func:`durable_entry` but is
        marked as a query handler.  The actual WASM export is identical; the
        host runtime distinguishes queries by the ``_is_query_handler`` flag.
    """

    def _make_entry(func: Callable) -> Callable:
        entry_name = name if name is not None else func.__name__
        decorated = durable_entry(entry_name)(func)
        decorated._is_query_handler = True  # type: ignore[attr-defined]
        return decorated

    if callable(name):
        return _make_entry(name)

    return _make_entry
