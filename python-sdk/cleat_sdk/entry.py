"""
Decorator for marking functions as Cleat workflow entry points.

Generates a WASM-export-compatible wrapper following the Cleat ABI::

    (args_ptr: i32, args_len: i32, out_ptr: i32, max_out_len: i32) -> i64

The decorator reads input JSON from linear memory, deserialises it, creates a
:class:`HostCalls` instance, calls the decorated function, serialises the
result back to JSON, and returns the packed i64 result code.

Usage::

    from cleat_sdk.entry import cleat_entry
    from cleat_sdk.host_calls import HostCalls

    @cleat_entry("PlaceOrder")
    def place_order(h: HostCalls, user_id: str, cart: list[dict]) -> str:
        ...
"""

from __future__ import annotations

import functools
import inspect
import json
import typing
from collections.abc import Callable
from typing import Any, get_type_hints

from .host_calls import HostCalls, SuspendSentinel

# String sentinel for workflow suspension (matches Go side check).
SUSPEND_SENTINEL_STR = "__CLEAT_SUSPEND__"


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
# Typed construction helper
# ---------------------------------------------------------------------------


def _from_dict(
    value: Any,
    target_type: Any,
    _cache: dict | None = None,
) -> Any:
    """Recursively construct typed objects from JSON-deserialised values.

    If *target_type* is a dataclass and *value* is a dict, constructs an
    instance of that dataclass.  Nested dataclass fields,
    ``list[Dataclass]``, ``Optional[Dataclass]``, and ``dict[str,
    Dataclass]`` are handled recursively.  For all other type/value
    combinations *value* is returned unchanged (safe fallback).

    Parameters
    ----------
    value:
        Raw value from ``json.loads`` (dict, list, str, int, float, bool,
        None).
    target_type:
        Expected type (from ``get_type_hints`` or a dataclass field
        annotation).
    _cache:
        Internal cache mapping dataclass types to their resolved field
        type hints, used to avoid repeated ``get_type_hints`` calls
        during recursive construction.

    Returns
    -------
    Any
        An instance of *target_type* constructed from *value*, or
        *value* itself when construction is not applicable.
    """
    import dataclasses

    # ---- Terminal values ----
    if value is None:
        return None

    if isinstance(target_type, str):
        # Unresolved forward reference / string annotation.
        return value

    origin = typing.get_origin(target_type)
    args = typing.get_args(target_type)

    # ---- Union / Optional (typing.Union) ----
    if origin is typing.Union:
        non_none_args = [a for a in args if a is not type(None)]
        if len(non_none_args) == 1:
            # Single non-None type inside Optional -> unwrap and recurse.
            return _from_dict(value, non_none_args[0], _cache)
        # Multiple union arms -- cannot choose; return raw value.
        return value

    # ---- Union / Optional (PEP 604: X | Y, Python 3.10+) ----
    if origin is not None and getattr(origin, "__name__", None) == "UnionType":
        non_none_args = [a for a in args if a is not type(None)]
        if len(non_none_args) == 1:
            return _from_dict(value, non_none_args[0], _cache)
        return value

    # ---- list[Element] / List[Element] ----
    if origin in (list, list):
        if args and isinstance(value, (list, tuple)):
            return [_from_dict(item, args[0], _cache) for item in value]
        return value

    # ---- dict[str, Value] / Dict[str, Value] ----
    if origin in (dict, dict):
        if args and len(args) == 2 and isinstance(value, dict):
            return {k: _from_dict(v, args[1], _cache) for k, v in value.items()}
        return value

    # ---- Dataclass ----
    try:
        is_dc = dataclasses.is_dataclass(target_type)
    # Deliberate: target_type is caller-supplied and may be any object,
    # including one with a hostile __class__ or a broken metaclass. A failure
    # to identify it as a dataclass just means "treat it as a plain value".
    except Exception:  # noqa: BLE001
        is_dc = False

    if is_dc:
        if not isinstance(value, dict):
            # Cannot construct a dataclass from a non-dict value.
            return value

        if _cache is None:
            _cache = {}
        type_hints = _cache.get(target_type)
        if type_hints is None:
            try:
                type_hints = typing.get_type_hints(target_type)
            # Deliberate: get_type_hints evaluates annotations, so it raises
            # whatever a user's forward reference raises -- NameError for an
            # unresolvable name, TypeError, or anything a module-level
            # __getattr__ chooses. Unresolvable hints degrade to no coercion.
            except Exception:  # noqa: BLE001
                type_hints = {}
            _cache[target_type] = type_hints

        # Build kwargs from the input dict, recursing per field.
        kwargs = {}
        for f in dataclasses.fields(target_type):
            if f.name not in value:
                # Field absent from input -- rely on dataclass field
                # default, or let __init__ raise TypeError.
                continue
            field_type = type_hints.get(f.name, f.type)
            kwargs[f.name] = _from_dict(value[f.name], field_type, _cache)

        return target_type(**kwargs)

    # ---- Fallthrough: return value unchanged ----
    return value


# ---------------------------------------------------------------------------
# Decorator
# ---------------------------------------------------------------------------


def _inject_witworld(func: Callable, export_wrapper: Callable, entry_name: str) -> None:
    """Inject a ``WitWorld`` class into *func*'s module.

    ``componentize-py`` requires the entry module to export a class named
    ``WitWorld`` whose ``run`` method matches the world's entry-point
    signature.  We create a lightweight class that delegates to the
    ``@cleat_entry`` wrapper.

    Multiple ``@cleat_entry`` functions in the same module are supported:
    all wrappers are stored in a registry keyed by entry name, and
    ``WitWorld.run`` dispatches based on the ``__cleat_entry__`` field in
    the input JSON.
    """
    import sys

    module_name = getattr(func, "__module__", None)
    if module_name is None:
        return
    module = sys.modules.get(module_name)
    if module is None:
        return

    # Initialise registry on the module (shared across all entries).
    if not hasattr(module, "_cleat_entry_wrappers"):
        module._cleat_entry_wrappers = {}

    # Store this wrapper keyed by entry name.
    module._cleat_entry_wrappers[entry_name] = export_wrapper

    def _dispatcher_run(args_str: str) -> str:
        """WitWorld.run dispatcher -- selects the right entry and delegates."""
        wrappers = module._cleat_entry_wrappers
        if not wrappers:
            raise RuntimeError("No cleat_entry functions registered")

        if len(wrappers) == 1:
            return next(iter(wrappers.values()))(args_str)

        # Multiple entries: dispatch based on __cleat_entry__ in input JSON.
        input_data: dict = json.loads(args_str) if args_str else {}

        entry_key = input_data.pop("__cleat_entry__", None)
        if entry_key is None:
            raise ValueError(
                f"Multiple cleat_entry functions registered "
                f"({list(wrappers.keys())}), "
                f"but input JSON does not contain '__cleat_entry__' field"
            )

        wrapper = wrappers.get(entry_key)
        if wrapper is None:
            raise ValueError(
                f"No cleat_entry named '{entry_key}'. Available entries: {list(wrappers.keys())}"
            )

        # Pass modified input (without __cleat_entry__) to the wrapper.
        modified_json = json.dumps(input_data)
        return wrapper(modified_json)

    wrapped = staticmethod(_dispatcher_run)
    module.WitWorld = type("WitWorld", (), {"run": wrapped})


def cleat_entry(name: str | None = None) -> Callable:
    """Mark a function as a Cleat workflow entry point.

    The decorated function **must** accept a :class:`HostCalls` instance as
    its first parameter.  Additional parameters are deserialised from the
    workflow input JSON by name.

    **Typed parameter construction:** If a parameter's type annotation is a
    :func:`dataclasses.dataclass`, the decorator automatically constructs an
    instance from the corresponding JSON dict.  Nested dataclass fields,
    ``list[Dataclass]``, ``Optional[Dataclass]``, and
    ``dict[str, Dataclass]`` are handled recursively.  All other parameter
    types receive the raw JSON-deserialised value (``str``, ``int``,
    ``float``, ``bool``, ``list``, ``dict``).

    Dataclass field names must match the corresponding JSON keys in the
    workflow input.  Extra JSON keys that do not correspond to any dataclass
    field are silently ignored.  Fields missing from the JSON input are
    omitted from the dataclass constructor (the field's default value is
    used, or :class:`TypeError` is raised if the field has no default).

    Example::

        from dataclasses import dataclass
        from cleat_sdk.entry import cleat_entry
        from cleat_sdk.host_calls import HostCalls

        @dataclass
        class Address:
            street: str
            city: str

        @dataclass
        class OrderInput:
            order_id: str
            amount: float
            shipping_address: Address

        @cleat_entry
        def place_order(h: HostCalls, input: OrderInput) -> str:
            # ``input`` is an ``OrderInput`` instance, not a raw dict.
            # ``input.shipping_address`` is an ``Address`` instance.
            return '{"status": "ok"}'

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
    ``wrapper._is_cleat_entry = True`` for introspection.
    """

    def _make_entry(func: Callable) -> Callable:
        # ---- resolve workflow parameter names (skip injected HostCalls) ----
        hints = get_type_hints(func)
        sig = inspect.signature(func)
        all_param_names = list(sig.parameters.keys())

        workflow_param_names: list[str] = []
        required_param_names: list[str] = []
        for pname in all_param_names:
            # The HostCalls parameter is injected by the framework and is never
            # part of the serialised input JSON.
            if pname in hints and hints[pname] is HostCalls:
                continue
            workflow_param_names.append(pname)
            param = sig.parameters[pname]
            if param.default is inspect.Parameter.empty:
                required_param_names.append(pname)

        workflow_name = name if name is not None else func.__name__

        @functools.wraps(func)
        def export_wrapper(args_str: str) -> str:
            """Cleat ABI export wrapper.

            Called by the host runtime with the input JSON string.  Parses
            input, invokes the workflow function, serialises the result, and
            returns the result JSON string.
            """
            try:
                # (a) Parse input JSON from the string argument.
                input_data: dict[str, Any] = json.loads(args_str) if args_str else {}

                # (b) Validate that every required (no-default) workflow parameter
                #     appears in the deserialised input.
                missing = [p for p in required_param_names if p not in input_data]
                if missing:
                    raise ValueError(f"Missing required parameters: {', '.join(missing)}")

                # (c) Build keyword arguments from the JSON keys that match
                #     workflow parameters, constructing dataclass instances
                #     where the parameter type annotation is a dataclass.
                _type_cache: dict = {}
                kwargs = {}
                for pname in workflow_param_names:
                    if pname not in input_data:
                        continue
                    raw_val = input_data[pname]
                    ptype = hints.get(pname)
                    if ptype is not None:
                        kwargs[pname] = _from_dict(raw_val, ptype, _type_cache)
                    else:
                        kwargs[pname] = raw_val

                # (d) Create the HostCalls instance and invoke the workflow.
                h = HostCalls()
                result = func(h, **kwargs)
                result = _unwrap_result(result)

                # (e) Serialise the return value and return as a string.
                return json.dumps(result, default=str)

            except SuspendSentinel:
                # The workflow signalled suspension (e.g. sleep on a
                # fresh execution).  Propagate a sentinel string.
                return SUSPEND_SENTINEL_STR

            # Deliberate, and load-bearing: this is the workflow error
            # boundary. Everything the user's workflow body can raise has to
            # become a JSON error payload here, or it crosses the WASM ABI as
            # a trap and the engine sees a dead guest instead of a failed step.
            except Exception as exc:  # noqa: BLE001
                # Any other exception is treated as a workflow error.
                return json.dumps({"error": str(exc)})

        # Mark the wrapper for introspection tooling.
        export_wrapper._is_cleat_entry = True  # type: ignore[attr-defined]

        # Inject WitWorld into the decorated function's module so
        # componentize-py can discover the entry point at build time.
        _inject_witworld(func, export_wrapper, workflow_name)

        return export_wrapper

    # ------------------------------------------------------------------
    # Support both ``@cleat_entry`` (without parentheses, legacy) and
    # ``@cleat_entry(...)`` (with parentheses, preferred).
    # ------------------------------------------------------------------
    if callable(name):
        # ``name`` is actually the decorated function.
        return _make_entry(name)

    return _make_entry


def virtual_object(name: str | None = None) -> Callable:
    """Register a function as a virtual object entry point.

    This decorator wraps :func:`cleat_entry` and marks the function as
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
        to :func:`cleat_entry` but additionally marked as a virtual object
        handler.
    """

    def _make_entry(func: Callable) -> Callable:
        entry_name = name if name is not None else func.__name__
        decorated = cleat_entry(entry_name)(func)
        decorated._is_virtual_object = True  # type: ignore[attr-defined]
        return decorated

    if callable(name):
        return _make_entry(name)

    return _make_entry


def query_handler(name: str | None = None) -> Callable:
    """Mark a function as a read-only query handler (no journaling).

    Unlike :func:`cleat_entry`, which marks a workflow entry point that
    journalises all cleat operations, this decorator marks a function as a
    read-only query handler.  Query handlers are invoked on-demand by external
    callers without recording events in the workflow history.

    The decorated function **must** accept a :class:`HostCalls` instance as
    its first parameter.  Additional parameters are deserialised from the
    query input JSON by name.

    Usage::

        from cleat_sdk import HostCalls, query_handler

        @query_handler("get_status")
        def get_status(h: HostCalls, order_id: str) -> str:
            # Read-only — no call, sleep, etc.
            state = h.get_state(order_id, dict)
            return json.dumps({"status": state.get("status", "unknown")})

    The decorated function carries ``wrapper._is_query_handler = True``
    and ``wrapper._is_cleat_entry = False`` for introspection.

    Parameters
    ----------
    name:
        Optional explicit query name.  Defaults to the Python function name.

    Returns
    -------
    Callable
        A wrapper function that behaves like :func:`cleat_entry` but is
        marked as a query handler.  The actual WASM export is identical; the
        host runtime distinguishes queries by the ``_is_query_handler`` flag.
    """

    def _make_entry(func: Callable) -> Callable:
        entry_name = name if name is not None else func.__name__
        decorated = cleat_entry(entry_name)(func)
        decorated._is_query_handler = True  # type: ignore[attr-defined]
        return decorated

    if callable(name):
        return _make_entry(name)

    return _make_entry
