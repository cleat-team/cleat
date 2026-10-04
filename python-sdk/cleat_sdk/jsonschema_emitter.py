"""JSON Schema emitter for ``@cleat_entry``-decorated Python workflows.

Converts a Python entry point's type hints into the JSON Schema fragment
``cleat_entry``'s own ``export_wrapper`` (``cleat_sdk/entry.py``) actually
accepts -- the Python-side counterpart of ``internal/jsonschema`` (cleat#1980,
cleat#2892) on the Go side, written to close cleat#2914.

THE GOVERNING RULE IS THE SAME ONE ``internal/jsonschema`` states for Go: a
schema here must mirror the binding, not improve on it. Where
``export_wrapper`` is permissive, this module must be equally permissive,
even where a hand-written schema would plausibly be stricter.

THE BINDING IS NOT GO'S BINDING, AND THE SCHEMA SHAPE DIFFERS IN WAYS THAT
ARE DELIBERATE, NOT OVERSIGHTS:

1. UNIVERSAL NULLABILITY. ``_from_dict``'s very first line is
   ``if value is None: return None``, unconditionally, before any
   type-specific branch -- for EVERY target type, not only a pointer, slice,
   or map the way Go's ``nullable()`` scopes it. A plain ``str`` or ``int``
   parameter (no ``Optional``) sent JSON ``null`` is bound to Python
   ``None`` with no error, same as everything else. So every schema node
   this module emits is nullable, not a Go-mirroring subset of kinds --
   verified empirically (see the PR for the probe script and its results),
   not assumed from ``_from_dict``'s source alone.

2. NO "WHOLE RAW INPUT" SPECIAL CASE. Go's ``EntryPointParamSchema`` treats
   a single ``string``-typed parameter specially: the whole undecoded
   "input" payload is assigned to it verbatim (``wasm/exports.go``'s
   ``argsJSON := readString(...)``, no ``json.Unmarshal``, no key lookup).
   Python's ``export_wrapper`` has no equivalent: it ALWAYS does
   ``input_data = json.loads(args_str)`` and then a KEYED lookup
   (``input_data[pname]``) for every parameter, one or many, string or not.
   So this module never emits ``anySchema()`` for a lone string parameter
   the way Go does -- it always describes an object.

3. "required" ON A NESTED DATACLASS OBJECT -- OWNER DECISION, 2026-10-01
   (cleat#2933). A nested dataclass field with no default DOES raise if
   absent (``target_type(**kwargs)`` -- a plain ``TypeError``), unlike Go's
   own struct fields, which ``encoding/json`` never enforces the presence
   of. That ``TypeError`` is raised INSIDE ``export_wrapper``'s own
   ``try/except Exception`` -- the same boundary that turns a missing
   TOP-LEVEL required parameter into a completed run's ``{"error": ...}``
   result rather than a WASM trap (contrast Go, where the analogous failure
   ``panic``s and the engine sees a dead guest -- cleat#1981's whole reason
   to validate before dispatch) -- so enforcing "required" here is a
   DELIBERATE, DOCUMENTED BREAKING CHANGE: a payload missing a nested
   required field now gets a pre-start 400 where it used to get a
   201-with-error-payload. This module omitted "required" at nested levels
   until this decision, flagged explicitly as an open scope question rather
   than decided unilaterally; the owner ruled it should be enforced, so the
   schema describes correct shape at every level, not only the top one. See
   CHANGELOG.md's "UPGRADE NOTES" for the concrete before/after.

   WHAT "REQUIRED" ACTUALLY MEANS IS __init__'S SIGNATURE, NOT
   dataclasses.fields(). A ``field(init=False)`` field (typically computed
   in ``__post_init__``) is listed by ``fields()`` but is NOT a parameter
   ``__init__`` accepts at all -- passing one is itself the
   "unexpected keyword argument" ``TypeError``, and omitting it is what
   works. The first version of this branch read ``fields()`` directly and
   had this exactly backwards (cleat-review G6 on #2933): it demanded the
   one payload that crashes construction and rejected the one that
   works. Reading ``inspect.signature(target_type)`` instead -- the actual
   callable ``target_type(**kwargs)`` exposes -- fixes that direction with
   one source of truth instead of patching ``fields()``'s output with an
   exclusion list.

   THE MIRROR CASE, AN InitVar FIELD, IS NOW FIXED IN BOTH HALVES, AND
   SAYING SO PRECISELY IS WHAT MAKES THAT USABLE (cleat-review A2 on
   #2933, resolved by cleat#2940). An ``InitVar`` IS a real ``__init__``
   parameter that ``fields()`` never lists, so reading the signature made
   this module aware of its NAME and TYPE. That was only HALF the repair:
   ``_from_dict``'s own kwargs-building loop still iterated
   ``dataclasses.fields(target_type)``, which excludes ``InitVar``, so no
   payload that binding constructed could supply one. Measured at the
   time: ``InitVar[int]`` with no default (required, by this module's own
   rule) made ``target_type(**kwargs)`` raise "missing 1 required
   positional argument" for EVERY payload, with or without that key --
   the dataclass was uninstantiable through that binding, while this
   module correctly reported the field as present and required.

   cleat#2940 closed that by moving ``_from_dict``'s loop onto
   ``inspect.signature`` too -- the same source this module reads -- so a
   required InitVar is both reported here and passable there, and the two
   agree by construction rather than by a shared exclusion list.
   An ``InitVar`` WITH a default was never affected: omitting it from
   the payload works, since ``__init__`` itself falls back to the default
   the same way it would for an ordinary optional field.

   The one asymmetry left is CORRECT and is not a gap: a payload that
   OMITS a required InitVar still raises from ``__init__``, exactly as
   omitting a required ordinary field does. The payload is equally
   incomplete either way, and no amount of reflection can invent a value.

4. RESULT IS ALWAYS UNCONSTRAINED. ``export_wrapper`` finishes with
   ``json.dumps(result, default=str)`` over whatever the workflow body
   returned -- a plain string, a dict, a duck-typed Result's unwrapped
   ``.value`` of any shape, or (via ``default=str``) the ``str()`` of
   anything else JSON cannot encode directly. The return type annotation
   does not pin the wire shape down enough to describe honestly, for the
   same reason Go's own ``EntryPointResultSchema`` doc comment gives for its
   own, narrower case ("whatever the function's own code marshaled... is
   invisible to static analysis, and claiming to know its shape would be
   guessing") -- more so here, since ``default=str`` can serialise an
   object the return annotation never promised.

5. LEAF/CONTAINER TYPE CONSTRAINTS ARE A DELIBERATE, OWNER-RULED TIGHTENING
   BEYOND TODAY'S BINDING (cleat#2933, 2026-10-01: ship strict). Measured
   directly against ``export_wrapper`` (not assumed): ``_from_dict``
   does not actually reject ANY value/type mismatch below the top level.
   ``{"user_id": 42}`` against a declared ``user_id: str`` is bound as the
   Python int ``42``, unchanged; ``{"cart": "notalist"}`` against a declared
   ``cart: list[int]`` is bound as the raw string, unchanged; a dataclass
   field given a non-dict value is likewise passed through un-coerced rather
   than refused. ALL of these return a normal completed run, no error of any
   kind -- confirmed with a throwaway harness calling the real wrapper
   directly, four cases, zero raised. The ONLY things ``export_wrapper``
   itself ever treats specially are (a) a top-level required parameter's
   KEY being absent from the input object, and (b) a nested dataclass's
   required FIELD being absent when its container value already happens to
   be a dict -- and neither of those is today a hard refusal either; both
   are caught by ``export_wrapper``'s own ``except Exception`` and returned
   as an ordinary completed run carrying ``{"error": ...}``, same as any
   workflow-body exception.

   A STRICTLY faithful schema -- "describe what the binding does, not what
   might be nicer", this issue's own words -- would instead emit
   ``anySchema()`` for every scalar and container VALUE, keeping type
   information only at the object-shape level (which parameters/fields
   exist, which are required). This module emits real ``"type"``
   constraints at every level instead: raised explicitly as an open
   question rather than decided unilaterally, the owner ruled to ship
   strict, broader than #2927's scalar-null-only ruling -- a caller who was
   previously (silently) tolerated sending a wrong-shaped leaf value now
   gets a 400. See CHANGELOG.md's "UPGRADE NOTES" for the concrete
   before/after.
"""

from __future__ import annotations

import dataclasses
import importlib.util
import inspect
import json
import sys
import typing
from collections.abc import Callable
from typing import Any

from .entry import _classify_entry_params


def _nullable(schema: dict) -> dict:
    """Widens schema's "type" to the two-element list [T, "null"].

    Mirrors ``_from_dict``'s unconditional null check -- every schema node
    this module builds passes through here, unlike
    ``internal/jsonschema.nullable``, which only three of Go's kinds ever
    call (see this module's docstring, point 1).

    A schema with no string "type" (anySchema, or one a recursive call
    already widened into a list) is returned unchanged -- nothing to add to,
    same reasoning ``internal/jsonschema.nullable`` gives for its own
    no-op case.
    """
    t = schema.get("type")
    if not isinstance(t, str):
        return schema
    out = dict(schema)
    out["type"] = [t, "null"]
    return out


def _any_schema() -> dict:
    return {}


def _schema_from_type(target_type: Any, visiting: frozenset[Any] = frozenset()) -> dict:
    """The JSON Schema fragment describing what ``_from_dict`` accepts for
    *target_type* -- walks the exact same branches ``_from_dict`` does, in
    the same order, over the TYPE rather than a runtime value.

    Always returns a nullable schema (see module docstring, point 1): there
    is no separate "non-null" inner form to wrap, because every branch
    recurses back into this function, and this function itself nullable-
    wraps its own result before returning.

    *visiting* guards a self-referential dataclass (a tree node with a field
    of its own type) the same way ``internal/jsonschema.fromGoType`` guards
    a self-referential Go struct -- pass the default for a top-level call.
    """
    if target_type is None or isinstance(target_type, str):
        # No annotation, or an unresolved forward reference _from_dict
        # itself treats as a terminal "return value unchanged" case.
        return _any_schema()

    origin = typing.get_origin(target_type)
    args = typing.get_args(target_type)

    # ---- Union / Optional (typing.Union, and PEP 604 X | Y) ----
    if origin is typing.Union or (
        origin is not None and getattr(origin, "__name__", None) == "UnionType"
    ):
        non_none_args = [a for a in args if a is not type(None)]
        if len(non_none_args) == 1:
            # _from_dict unwraps to the single non-None arm and recurses;
            # that recursive call is itself nullable, so the Optional
            # wrapper here adds nothing further.
            return _schema_from_type(non_none_args[0], visiting)
        # Multiple non-None arms: _from_dict "cannot choose", returns the
        # raw value. Already null-accepting.
        return _any_schema()

    # ---- list[Element] ----
    if origin is list:
        if not args:
            # A bare, unparameterized `list` annotation: _from_dict's own
            # `if args and ...` guard never recurses into one either.
            return _any_schema()
        return _nullable({"type": "array", "items": _schema_from_type(args[0], visiting)})

    # ---- dict[str, Value] ----
    if origin is dict:
        if not args or len(args) != 2:
            return _any_schema()
        return _nullable(
            {"type": "object", "additionalProperties": _schema_from_type(args[1], visiting)}
        )

    # ---- Dataclass ----
    try:
        is_dc = dataclasses.is_dataclass(target_type)
    # Deliberate, same reasoning as _from_dict's own try/except around this
    # same call: target_type is caller-controlled via annotations this
    # module did not write, down to a hostile __class__.
    except Exception:  # noqa: BLE001
        is_dc = False

    if is_dc:
        if target_type in visiting:
            # Self-referential dataclass (a linked-list/tree node). Breaking
            # the cycle with anySchema is honest for the same reason
            # internal/jsonschema's namedSchema gives: _from_dict itself has
            # no depth limit, so any fixed-depth schema here would be a
            # guess at how deep a real payload goes.
            return _any_schema()
        nested_visiting = visiting | {target_type}
        try:
            field_hints = typing.get_type_hints(target_type)
        # Deliberate, mirroring _from_dict's own except around this same
        # call: an unresolvable forward reference degrades to no coercion
        # there, and to no schema (anySchema per field) here.
        except Exception:  # noqa: BLE001
            field_hints = {}
        properties = {}
        required = []
        # cleat-review G6: iterate target_type(**kwargs)'s actual __init__
        # SIGNATURE, not dataclasses.fields(). The two are not the same
        # set: a field declared `field(init=False)` (e.g. a value computed
        # in __post_init__) appears in fields() but NOT in __init__'s
        # parameters -- passing it as a kwarg is the exact
        # "unexpected keyword argument" TypeError the governing rule
        # exists to describe faithfully, and the first version of this
        # branch had that backwards: it required the one key that crashes
        # construction and accepted a payload without it that works fine,
        # confirmed with a throwaway probe (field(init=False), no explicit
        # default). An InitVar pseudo-field is the opposite mismatch -- it
        # IS a constructor keyword but fields() never lists it -- so
        # reading the signature makes this module AWARE of one for the
        # first time (see module docstring for why that is only a partial
        # fix: _from_dict can never actually supply a required InitVar's
        # value, independent of what this schema says).
        sig = inspect.signature(target_type)
        for name, param in sig.parameters.items():
            field_type = field_hints.get(name, param.annotation)
            # InitVar[X] is dataclasses' own wrapper, not a real type --
            # typing.get_type_hints returns it UNwrapped (confirmed
            # empirically), so unwrap it the same way _from_dict would
            # never need to (it never sees InitVar at all; __init__ already
            # consumed it by the time a value reaches a stored field).
            if isinstance(field_type, dataclasses.InitVar):
                field_type = field_type.type
            properties[name] = _schema_from_type(field_type, nested_visiting)
            # Owner decision, 2026-10-01 (see module docstring, point 3): a
            # missing required field at a NESTED level is now enforced here
            # too, not only at the top level -- "no default" in __init__'s
            # own signature is the exact test `target_type(**kwargs)`
            # applies, which is why this reads the signature rather than
            # re-deriving the rule from dataclasses.Field attributes.
            if param.default is inspect.Parameter.empty:
                required.append(name)
        schema = {"type": "object", "properties": properties, "additionalProperties": True}
        if required:
            schema["required"] = required
        return _nullable(schema)

    # ---- Plain scalars _from_dict passes through unchanged but whose JSON
    # wire shape is still fully known ----
    if target_type is bool:  # must precede int: bool is an int subclass
        return _nullable({"type": "boolean"})
    if target_type is int:
        return _nullable({"type": "integer"})
    if target_type is float:
        return _nullable({"type": "number"})
    if target_type is str:
        return _nullable({"type": "string"})

    # ---- Fallthrough: _from_dict returns the raw value unchanged for
    # anything else (Any, a bare tuple/set, a Callable, a TypeVar, ...) --
    # the honest schema is unconstrained, not a guess at a shape _from_dict
    # itself does not enforce. ----
    return _any_schema()


def entry_point_param_schema(func: Callable) -> dict:
    """The "params" half of *func*'s schema pair, mirroring
    ``export_wrapper`` exactly (see module docstring for the governing
    rule and its two call-out divergences from the Go emitter).
    """
    hints = typing.get_type_hints(func)
    workflow_param_names, required_param_names = _classify_entry_params(func, hints)

    if not workflow_param_names:
        # input_data is parsed but never read by anything that affects
        # binding outcome (no required check can fail, no kwarg is built) --
        # mirrors Go's own len(fields)==0 case.
        return _any_schema()

    properties = {}
    for pname in workflow_param_names:
        properties[pname] = _schema_from_type(hints.get(pname))

    schema: dict = {
        "type": "object",
        "properties": properties,
        "additionalProperties": True,
    }
    if required_param_names:
        schema["required"] = required_param_names
    return schema


def entry_point_result_schema(func: Callable) -> dict:
    """Always anySchema -- see module docstring, point 4."""
    del func
    return _any_schema()


def _load_module(filepath: str):
    """Imports *filepath* as a real module (not an AST-only read): schema
    derivation needs ``typing.get_type_hints``, which needs the actual type
    objects a live import resolves, the same way ``@cleat_entry`` itself
    needs them at decoration time -- an import this module performs is one
    ``@cleat_entry`` was always going to need to perform too, at build time,
    via ``componentize-py``.
    """
    module_name = f"_cleat_jsonschema_target_{abs(hash(filepath))}"
    spec = importlib.util.spec_from_file_location(module_name, filepath)
    if spec is None or spec.loader is None:
        raise ImportError(f"could not load {filepath} as a Python module")
    module = importlib.util.module_from_spec(spec)
    sys.modules[module_name] = module
    spec.loader.exec_module(module)
    return module


def _find_entry(module, func_name: str) -> tuple[str, Callable]:
    """Returns (workflow_name, undecorated_func) for the ``@cleat_entry``
    function named *func_name* in *module*.

    Reads ``module._cleat_entry_wrappers`` (populated by
    ``entry._inject_witworld`` as a side effect of decoration, which has
    already happened by the time ``_load_module`` returns) rather than
    ``getattr(module, func_name)``, because the two names can legitimately
    differ: ``@cleat_entry("PlaceOrder")`` on ``def place_order(...)``
    registers the wrapper under "PlaceOrder", not "place_order". Matching on
    ``wrapper.__name__`` (preserved by ``functools.wraps`` from the
    UNDECORATED function) rather than the registry key is what lets this
    find the right entry even though *func_name* is the Python identifier
    cleat_sdk.vet's AST-based ``--detect-entry`` reports, not necessarily
    the registered workflow name.
    """
    wrappers = getattr(module, "_cleat_entry_wrappers", {})
    for workflow_name, wrapper in wrappers.items():
        if wrapper.__name__ == func_name:
            return workflow_name, getattr(wrapper, "__wrapped__", wrapper)
    raise LookupError(
        f"no @cleat_entry function named {func_name!r} found "
        f"(registered: {sorted(wrappers)})"
    )


def main() -> int:
    if len(sys.argv) != 3:
        print(
            "Usage: python -m cleat_sdk.jsonschema_emitter <file.py> <func_name>",
            file=sys.stderr,
        )
        return 2

    filepath, func_name = sys.argv[1], sys.argv[2]

    try:
        module = _load_module(filepath)
    # Deliberate: importing arbitrary user code can raise anything --
    # a SyntaxError, a missing dependency's ImportError, a module-level
    # assertion. All of them mean "cannot emit a schema", reported the same
    # way cleat_sdk.vet's own entry detection reports a file it cannot read.
    except Exception as exc:  # noqa: BLE001
        print(f"importing {filepath}: {exc}", file=sys.stderr)
        return 1

    try:
        workflow_name, func = _find_entry(module, func_name)
    except LookupError as exc:
        print(str(exc), file=sys.stderr)
        return 1

    try:
        params = entry_point_param_schema(func)
    # Deliberate: get_type_hints evaluates every annotation in the function's
    # defining module's namespace, so an unresolvable forward reference
    # anywhere in the signature raises here rather than degrading --
    # reported as a build failure rather than a silently empty schema, since
    # the vet module's own --detect-entry already requires the file to
    # import cleanly to reach this point at all.
    except Exception as exc:  # noqa: BLE001
        print(f"computing schema for {func_name!r} in {filepath}: {exc}", file=sys.stderr)
        return 1

    result = entry_point_result_schema(func)

    print(json.dumps({workflow_name: {"params": params, "result": result}}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
