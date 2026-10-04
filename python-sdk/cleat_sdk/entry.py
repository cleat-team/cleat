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

from .defer import run_deferred
from .host_calls import HostCalls, SuspendSentinel

try:
    from wit_world import RunOutcome_Completed, RunOutcome_Suspended
except ImportError:  # not running in WASM -- see host_calls._USING_WASM

    class RunOutcome_Completed:  # type: ignore[no-redef]
        """Stand-in for the generated binding outside the WASM runtime."""

        def __init__(self, value: str) -> None:
            self.value = value

    class RunOutcome_Suspended:  # type: ignore[no-redef]
        """Stand-in for the generated binding outside the WASM runtime."""


class _Suspended:
    """What the entry wrapper returns when the workflow did not finish.

    A singleton object rather than the string ``"__CLEAT_SUSPEND__"``, which
    is what this used to be and what engine/component_cgo.go compared the
    ``run`` export's result against verbatim. That comparison is gone -- a
    suspension is a case of ``run-outcome`` now (wit/cleat.wit) -- and with it
    the reason to spell one as text. An object cannot be produced by
    ``json.dumps`` of anything a workflow returns, which is a stronger
    statement than "no workflow has returned that string yet".
    """

    __slots__ = ()

    def __repr__(self) -> str:  # pragma: no cover - diagnostics only
        return "<cleat: workflow suspended>"


SUSPENDED = _Suspended()


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

        # Build kwargs from the input dict, recursing per parameter.
        #
        # The source is `inspect.signature`, NOT `dataclasses.fields`. fields()
        # omits InitVar pseudo-fields -- they are not stored attributes -- so
        # an InitVar was invisible here, and a dataclass with a REQUIRED one
        # could not be constructed through this binding at all, whatever the
        # payload carried (cleat#2940). signature() is the callable that
        # `target_type(**kwargs)` below actually invokes, and it is the same
        # source `jsonschema_emitter.py` reads, so the emitted schema and this
        # binding agree by construction rather than by coincidence.
        #
        # The same source also excludes `field(init=False)`, which fields()
        # lists and __init__ does not accept -- passing one used to raise
        # "unexpected keyword argument" from the call below.
        #
        # A payload the constructor takes NOTHING from is refused rather than
        # tolerated, and the line between the two is the point: a key the
        # signature does not have is a stray key, but a payload from which
        # nothing at all was read is loss -- the object would be built
        # entirely from its defaults and nothing would say so.
        #
        # This is ONE of the two shapes cleat#2940 changed for the worse -- the
        # other is the VAR_KEYWORD case handled below. For
        # `@dataclass(init=False)` with no hand-written `__init__` the
        # signature is empty, and reading fields() used to raise "takes no
        # arguments" -- by accident, because it passed a keyword the class did
        # not accept. Restoring the failure, with a message naming the type and
        # the keys, is scoped to what that change altered; the stray-key rule
        # below is left exactly as it was (cleat#3058).
        params = inspect.signature(target_type).parameters
        if not params and value:
            raise TypeError(
                f"{target_type.__name__}() takes no constructor arguments, so "
                f"none of the payload keys {sorted(value)} can be supplied; "
                f"refusing to build it from its defaults alone"
            )

        kwargs = {}
        for name, param in params.items():
            if name not in value:
                # Absent from input -- rely on the dataclass default, or let
                # __init__ raise TypeError for a required one. An ordinary
                # required field and a required InitVar fail identically
                # there, because the payload is equally incomplete for both.
                continue
            field_type = type_hints.get(name, param.annotation)
            # An InitVar's annotation is the WRAPPER -- `InitVar[int]`, not
            # `int` -- so recursing with it would convert against a non-type.
            # Unwrap through the public `.type` attribute.
            if isinstance(field_type, dataclasses.InitVar):
                field_type = field_type.type
            kwargs[name] = _from_dict(value[name], field_type, _cache)

        # A VAR_KEYWORD parameter accepts keywords the signature does not name,
        # so payload keys it did not match above belong to it. Measured on
        # three revisions: the fields() loop passed a field-matching key here
        # and `**kw` took it; reading the signature made that key look
        # unmatched and it STOPPED arriving -- working -> quiet loss, with no
        # error either side. Restoring it is what the parameter is for
        # (cleat#3058). Their types are unknown to this binding, so they are
        # passed exactly as they came.
        if any(p.kind is inspect.Parameter.VAR_KEYWORD for p in params.values()):
            kwargs.update({k: v for k, v in value.items() if k not in params})

        return target_type(**kwargs)

    # ---- Fallthrough: return value unchanged ----
    return value


# ---------------------------------------------------------------------------
# Shared binding-shape derivation
# ---------------------------------------------------------------------------


def _classify_entry_params(
    func: Callable, hints: dict[str, Any]
) -> tuple[list[str], list[str]]:
    """Splits *func*'s parameters into workflow params and required params,
    skipping the injected HostCalls parameter.

    The SINGLE derivation of this binding shape -- ``_make_entry`` (runtime
    dispatch, below) and ``cleat_sdk.jsonschema_emitter`` (cleat#2914, the
    static JSON Schema emitter) both call this rather than each re-deriving
    the field list independently, so the schema cannot drift from what the
    binding actually does. ``internal/jsonschema``'s Go emitter states the
    identical reason for reading ``analyzer.EntryPointFields`` rather than
    recomputing it.

    A parameter is "required" if it carries no default -- ``export_wrapper``
    below refuses an input missing any name in this list
    (``missing = [p for p in required_param_names if p not in input_data]``)
    before the function is ever called; a parameter WITH a default is left
    out of input_data-presence checking entirely and simply keeps its
    Python-level default when absent.
    """
    sig = inspect.signature(func)
    workflow_param_names: list[str] = []
    required_param_names: list[str] = []
    for pname in sig.parameters:
        # The HostCalls parameter is injected by the framework and is never
        # part of the serialised input JSON.
        if pname in hints and hints[pname] is HostCalls:
            continue
        workflow_param_names.append(pname)
        param = sig.parameters[pname]
        if param.default is inspect.Parameter.empty:
            required_param_names.append(pname)
    return workflow_param_names, required_param_names


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

    def _select(args_str: str) -> Any:
        """Select the right entry wrapper and delegate to it."""
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

    def _dispatcher_run(args_str: str) -> Any:
        """WitWorld.run -- the world's entry point, returning a ``run-outcome``.

        The conversion from the wrapper's Python-level result to the WIT
        variant happens HERE and nowhere else, so ``export_wrapper`` keeps
        returning a plain JSON string and every test that calls a decorated
        workflow directly keeps working.
        """
        out = _select(args_str)
        if out is SUSPENDED:
            return RunOutcome_Suspended()
        return RunOutcome_Completed(out)

    def _dispatcher_run_deferred() -> int:
        """WitWorld.run-deferred -- the HOST's drain of the defer table.

        Reached only after ``run`` returned ``suspended`` during a defer
        segment, and only because the host calls it: the guest deliberately
        does not drain on suspension. The host brackets this call so the defer
        bodies' own durable calls go through while the workflow body's are
        stopped, which is the whole reason the drain is a separate export
        rather than something the wrapper does on its way out.

        ``propagate_suspend=False`` because there is no result left to protect
        and the bodies are already off the table -- see run_deferred's
        docstring for the two drains and why they differ.
        """
        return run_deferred(propagate_suspend=False)

    module.WitWorld = type(
        "WitWorld",
        (),
        {
            "run": staticmethod(_dispatcher_run),
            "run_deferred": staticmethod(_dispatcher_run_deferred),
        },
    )


def _resolve_dual_form(
    name: str | Callable | None,
    make_entry: Callable[[Callable, str | None], Callable],
) -> Callable:
    """Resolve ``@d``, ``@d()`` and ``@d("name")`` into one decorator call.

    ``make_entry(func, explicit_name)`` builds the wrapper, where
    ``explicit_name`` is the caller's name or ``None`` when the caller gave
    none -- and ``None`` is not "no name is possible", it is the instruction to
    use the FUNCTION'S OWN NAME.

    THIS IS ONE FUNCTION RATHER THAN THREE COPIES, and that is the fix rather
    than a tidy-up. ``cleat_entry``, ``virtual_object`` and ``query_handler``
    each carried this branch verbatim:

        if callable(name):
            # ``name`` is actually the decorated function.
            return _make_entry(name)

    and each of them was wrong in the same way: the single argument of the
    bare form is the decorated FUNCTION, and it was passed straight into the
    slot the workflow NAME is read from. So ``entry_name`` was a function
    object, ``module._cleat_entry_wrappers`` was keyed by it, and
    ``jsonschema_emitter._find_entry`` -- which matches on that key being the
    workflow name -- found nothing.

    Three copies meant three places to fix and three places to forget, which is
    the shape the bug arrived in. cleat#2976.
    """
    if callable(name):
        # BARE FORM: ``@cleat_entry`` with no parentheses. The argument is the
        # decorated function and the name slot is EMPTY, so make_entry falls
        # through to func.__name__ -- the same key the parenthesised form
        # produces for a function of that name, because that is what "defaults
        # to the Python function name" means in both.
        return make_entry(name, None)

    return lambda func: make_entry(func, name)


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

        @cleat_entry("place_order")
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

    def _make_entry(func: Callable, explicit_name: str | None) -> Callable:
        # ---- resolve workflow parameter names (skip injected HostCalls) ----
        hints = get_type_hints(func)
        sig = inspect.signature(func)
        all_param_names = list(sig.parameters.keys())

        # REFUSE AN UNANNOTATED HostCalls AT DECORATION TIME. cleat#1637.
        #
        # The injected parameter is identified by its TYPE HINT below. Without
        # the hint it is not skipped, so it joins workflow_param_names and --
        # having no default -- required_param_names, and the presence check
        # refuses every payload: no caller ever sends a key called "h", because
        # the framework injects it. The workflow never runs, on any input.
        #
        # THE OLD FAILURE NAMED THE WRONG THING, which is why this is an error
        # and not a doc note. It said "Missing required parameters: h", which
        # reads as a caller problem -- and adding "h" to the start payload DOES
        # make it go away, handing the workflow a JSON value where it expects a
        # HostCalls. The fix that suggests itself is worse than the defect.
        #
        # Java refuses the analogous shape in its annotation processor ("@CleatEntry
        # method first parameter must be cleat.HostCalls, got ..."), so this
        # brings Python to the same place: caught where it is written, not on
        # the first start.
        #
        # SCOPED TO "a first parameter that is not HostCalls". A function with NO
        # parameters is left alone: testdata/vet-checks/python/* declares several
        # (`def workflow() -> None`) as fixtures for other rules, and whether a
        # zero-parameter entry should be legal is a separate question this must
        # not decide by accident.
        if all_param_names:
            first = all_param_names[0]
            if hints.get(first) is not HostCalls:
                raise TypeError(
                    f"@cleat_entry {func.__name__}({first}, ...): the first parameter is the "
                    f"injected HostCalls runtime and must be annotated "
                    f"`{first}: HostCalls`. Without the annotation it is treated as a workflow "
                    f"parameter, and every start fails with "
                    f'"Missing required parameters: {first}" -- which names the payload rather '
                    f"than the annotation. Import it with "
                    f"`from cleat_sdk.host_calls import HostCalls`."
                )

        workflow_param_names, required_param_names = _classify_entry_params(func, hints)

        # The name this entry is registered under, and the key every reader of
        # the registry matches on. explicit_name is None for the bare form and
        # for @cleat_entry(), both of which mean the function's own name.
        workflow_name = explicit_name if explicit_name is not None else func.__name__

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

                # (e) Run the workflow's own defers before reporting, so
                #     anything they record lands inside this segment. See
                #     IMPROVEMENT-PLAN 3.73. A defer that itself suspends
                #     raises SuspendSentinel, which the handler below catches
                #     -- suspension wins over the result, exactly as it does
                #     when the workflow body suspends.
                run_deferred()

                # (f) Serialise the return value and return as a string.
                return json.dumps(result, default=str)

            except SuspendSentinel:
                # The workflow signalled suspension (e.g. sleep on a
                # fresh execution).  Propagate a sentinel string.
                #
                # Defers deliberately do NOT run here. A suspended workflow has
                # not exited; its cleanup is still pending, and firing it at the
                # first sleep would release locks a workflow that is about to
                # continue still holds. The final segment replays the entry
                # point, re-registers the same defers, and runs them when it
                # completes.
                return SUSPENDED

            # Deliberate, and load-bearing: this is the workflow error
            # boundary. Everything the user's workflow body can raise has to
            # become a JSON error payload here, or it crosses the WASM ABI as
            # a trap and the engine sees a dead guest instead of a failed step.
            except Exception as exc:  # noqa: BLE001
                # Any other exception is treated as a workflow error.
                #
                # Defers still run: cleanup exists for the run that did not
                # finish the way it meant to, and a defer that only fires on
                # the happy path is close to useless. A defer that suspends
                # here turns the segment into a suspension, which is why this
                # is a nested try rather than a bare call -- the outer
                # SuspendSentinel handler is already committed to this branch.
                try:
                    run_deferred()
                except SuspendSentinel:
                    return SUSPENDED
                return json.dumps({"error": str(exc)})

        # Mark the wrapper for introspection tooling.
        export_wrapper._is_cleat_entry = True  # type: ignore[attr-defined]

        # Inject WitWorld into the decorated function's module so
        # componentize-py can discover the entry point at build time.
        _inject_witworld(func, export_wrapper, workflow_name)

        return export_wrapper

    # Support both ``@cleat_entry`` (without parentheses, legacy) and
    # ``@cleat_entry(...)`` (with parentheses, preferred). See
    # _resolve_dual_form for why this is not inlined here.
    return _resolve_dual_form(name, _make_entry)


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

    def _make_entry(func: Callable, explicit_name: str | None) -> Callable:
        entry_name = explicit_name if explicit_name is not None else func.__name__
        decorated = cleat_entry(entry_name)(func)
        decorated._is_virtual_object = True  # type: ignore[attr-defined]
        return decorated

    return _resolve_dual_form(name, _make_entry)


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

    def _make_entry(func: Callable, explicit_name: str | None) -> Callable:
        entry_name = explicit_name if explicit_name is not None else func.__name__
        decorated = cleat_entry(entry_name)(func)
        decorated._is_query_handler = True  # type: ignore[attr-defined]
        return decorated

    return _resolve_dual_form(name, _make_entry)
