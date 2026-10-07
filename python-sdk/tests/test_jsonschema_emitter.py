"""Tests for cleat_sdk.jsonschema_emitter (cleat#2914).

See that module's own docstring for the governing rule and the five
documented divergences from internal/jsonschema's Go emitter. These tests
pin the SHAPE of the schema each divergence produces, not just that a
schema is produced at all -- a schema emitter that silently regressed to
Go's rules (a lone string gets anySchema, only pointer/slice/map are
nullable, a nested dataclass gains "required") would still "pass" a test
that only checked for SOME non-empty schema.
"""

from dataclasses import InitVar, dataclass, field

import pytest

try:
    from cleat_sdk.entry import _from_dict, cleat_entry
    from cleat_sdk.host_calls import HostCalls
    from cleat_sdk.jsonschema_emitter import (
        entry_point_param_schema,
        entry_point_result_schema,
    )
except ImportError as e:
    pytest.skip(
        f"Skipping jsonschema_emitter tests: {e}. cleat_sdk must be importable "
        f"(requires Python >= 3.10).",
        allow_module_level=True,
    )


def _func(wrapper):
    """The undecorated function cleat_entry wrapped, same lookup
    cleat_sdk.jsonschema_emitter._find_entry uses for a real build."""
    return wrapper.__wrapped__


def test_no_workflow_params_is_any_schema():
    @cleat_entry
    def wf(h: HostCalls) -> str:
        return "{}"

    assert entry_point_param_schema(_func(wf)) == {}


def test_a_lone_string_parameter_is_still_an_object():
    """Divergence 2 from the Go emitter: Python has no "whole raw input"
    fast path. A lone string param is sent like every other -- as a keyed
    object property -- so the schema must say "object", not anySchema the
    way Go's EntryPointParamSchema would for the identical signature."""

    @cleat_entry
    def wf(h: HostCalls, note: str) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))
    assert schema["type"] == "object"
    assert schema["properties"]["note"]["type"] == ["string", "null"]
    assert schema["required"] == ["note"]


def test_a_defaulted_parameter_is_not_required():
    @cleat_entry
    def wf(h: HostCalls, user_id: str, priority: int = 1) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))
    assert schema["required"] == ["user_id"]
    assert "priority" not in schema["required"]
    assert schema["additionalProperties"] is True


def test_every_scalar_kind_is_nullable():
    """Divergence 1: _from_dict's null check is unconditional, for every
    type, not scoped to pointer/slice/map the way Go's nullable() is."""

    @cleat_entry
    def wf(h: HostCalls, s: str, i: int, f: float, b: bool) -> str:
        return "{}"

    props = entry_point_param_schema(_func(wf))["properties"]
    assert props["s"]["type"] == ["string", "null"]
    assert props["i"]["type"] == ["integer", "null"]
    assert props["f"]["type"] == ["number", "null"]
    assert props["b"]["type"] == ["boolean", "null"]


def test_list_schema_describes_items_and_is_itself_nullable():
    @cleat_entry
    def wf(h: HostCalls, cart: list[int]) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))["properties"]["cart"]
    assert schema["type"] == ["array", "null"]
    assert schema["items"]["type"] == ["integer", "null"]


def test_dict_schema_describes_values_via_additional_properties():
    @cleat_entry
    def wf(h: HostCalls, meta: dict[str, int]) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))["properties"]["meta"]
    assert schema["type"] == ["object", "null"]
    assert schema["additionalProperties"]["type"] == ["integer", "null"]


def test_optional_is_indistinguishable_from_plain_because_everything_is_nullable():
    """str | None and a plain str get the IDENTICAL schema: _from_dict's
    null handling does not depend on whether the annotation says Optional,
    so there is nothing for Optional to add."""

    @cleat_entry
    def wf(h: HostCalls, a: str, b: str | None = None) -> str:
        return "{}"

    props = entry_point_param_schema(_func(wf))["properties"]
    assert props["a"] == props["b"]


def test_multi_arm_union_is_any_schema():
    @cleat_entry
    def wf(h: HostCalls, v: int | str) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))["properties"]["v"]
    assert schema == {}


def test_nested_dataclass_has_a_required_array():
    """Divergence 3, owner decision 2026-10-01: a nested dataclass's
    missing-field TypeError is enforced by the schema too, not only at the
    top level -- a documented breaking change (see CHANGELOG.md)."""

    @dataclass
    class Address:
        street: str
        city: str
        unit: str = ""

    @cleat_entry
    def wf(h: HostCalls, address: Address) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))["properties"]["address"]
    assert schema["type"] == ["object", "null"]
    assert set(schema["properties"]) == {"street", "city", "unit"}
    assert schema["required"] == ["street", "city"]
    assert "unit" not in schema["required"]
    assert schema["additionalProperties"] is True


def test_nested_dataclass_field_with_a_default_factory_is_not_required():
    @dataclass
    class Cart:
        items: list[str] = field(default_factory=list)

    @cleat_entry
    def wf(h: HostCalls, cart: Cart) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))["properties"]["cart"]
    assert "required" not in schema


def test_nested_dataclass_field_with_init_false_is_absent_from_the_schema():
    """cleat-review G6. A field(init=False) is never a valid __init__
    keyword -- target_type(**kwargs) raises "unexpected keyword argument"
    if a caller's value for it is ever passed through, and omitting it is
    exactly what works. dataclasses.fields() still lists it, so deriving
    "required"/properties from fields() (the first version of this
    function) got this backwards: it demanded the one payload that
    crashes construction and rejected the one that works. Reading
    inspect.signature(target_type) instead -- __init__'s own parameter
    list -- excludes it from both properties and required, matching what
    the binding actually accepts."""

    @dataclass
    class Order:
        sku: str
        total: int = field(init=False)

        def __post_init__(self):
            self.total = 0

    @cleat_entry
    def wf(h: HostCalls, order: Order) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))["properties"]["order"]
    assert "total" not in schema["properties"]
    assert schema["required"] == ["sku"]


def test_nested_dataclass_initvar_is_a_required_property_not_a_field():
    """The inverse of the init=False case: an InitVar IS a valid __init__
    keyword (target_type's own __post_init__ consumes it) but is NOT a
    stored field, so dataclasses.fields() never lists it at all -- the
    opposite blind spot inspect.signature(target_type) also closes."""

    @dataclass
    class Order:
        sku: str
        secret: InitVar[str] = None

        def __post_init__(self, secret):
            pass

    @cleat_entry
    def wf(h: HostCalls, order: Order) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))["properties"]["order"]
    assert schema["properties"]["secret"]["type"] == ["string", "null"]
    assert "secret" not in schema["required"]


def test_a_required_initvar_is_reported_and_now_instantiable_by_the_payload_that_carries_it():
    """cleat-review A2 on #2933, corrected by cleat#2940. A required
    (no-default) InitVar is a real __init__ parameter, so this module
    correctly reports it as present and required. Until #2940 that was
    only half a repair: _from_dict's own kwargs-building loop iterated
    dataclasses.fields(), which excludes InitVar, so NO payload the
    binding built could supply one -- the schema described a signature
    nothing could satisfy. Both now read inspect.signature, so the
    reported field IS passable.

    The asymmetry that remains is asserted here rather than assumed: a
    payload that OMITS the required InitVar still raises, exactly as
    omitting a required ordinary field does. That case is the one that
    keeps the fix honest -- a binding that invented a value for it would
    construct a dataclass from a payload the caller never completed."""

    @dataclass
    class Order:
        sku: str
        seed: InitVar[int]

        def __post_init__(self, seed):
            # Recorded so the assertion below proves the value reached
            # __init__'s own initialiser, not merely that the keyword was
            # accepted without complaint.
            self.seed_seen = seed

    @cleat_entry
    def wf(h: HostCalls, order: Order) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))["properties"]["order"]
    assert schema["required"] == ["sku", "seed"]

    # Carrying it -- the case cleat#2940 fixed.
    built = _from_dict({"sku": "a", "seed": 3}, Order)
    assert (built.sku, built.seed_seen) == ("a", 3)

    # Omitting it -- still a TypeError naming the missing parameter, the
    # same failure that omitting a required ordinary field produces.
    with pytest.raises(TypeError, match="missing 1 required positional argument: 'seed'"):
        _from_dict({"sku": "a"}, Order)


def test_self_referential_dataclass_does_not_recurse_forever():
    @dataclass
    class Node:
        value: int
        next: "Node | None" = None

    @cleat_entry
    def wf(h: HostCalls, head: Node) -> str:
        return "{}"

    # Must simply return -- an infinite recursion would hang or blow the
    # stack rather than raise something pytest.raises could catch.
    schema = entry_point_param_schema(_func(wf))["properties"]["head"]
    assert schema["type"] == ["object", "null"]
    assert "next" in schema["properties"]


def test_result_schema_is_always_any_schema():
    @cleat_entry
    def wf(h: HostCalls) -> str:
        return "{}"

    assert entry_point_result_schema(_func(wf)) == {}


def test_extra_properties_are_allowed():
    @cleat_entry
    def wf(h: HostCalls, a: str) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))
    assert schema["additionalProperties"] is True


# ---------------------------------------------------------------------------
# cleat#1986: exposure rides this same sidecar document.
# ---------------------------------------------------------------------------


class TestFindEntryReturnsExposure:
    """``_find_entry`` is what the build actually calls (via ``main()``), so
    this exercises the real lookup path rather than reading ``_cleat_expose``
    off a decorated function directly, the way TestExposureDeclaration in
    test_entry.py does for the decorator's own behaviour.
    """

    def test_a_declared_class_round_trips_through_find_entry(self):
        import sys

        from cleat_sdk.jsonschema_emitter import _find_entry

        @cleat_entry("InventorySync", expose="internal")
        def inventory_sync(h: HostCalls, item_id: str) -> str:
            return "{}"

        module = sys.modules[__name__]
        workflow_name, func, expose = _find_entry(module, "inventory_sync")
        assert workflow_name == "InventorySync"
        assert func is _func(inventory_sync)
        assert expose == "internal"

    def test_no_declaration_returns_none_not_a_missing_key(self):
        import sys

        from cleat_sdk.jsonschema_emitter import _find_entry

        @cleat_entry
        def undeclared_for_find_entry(h: HostCalls) -> str:
            return "{}"

        module = sys.modules[__name__]
        _, _, expose = _find_entry(module, "undeclared_for_find_entry")
        assert expose is None


def test_main_writes_the_exposure_key_only_when_declared(tmp_path, capsys, monkeypatch):
    """The end-to-end path main() drives: a source file on disk, through
    _load_module and _find_entry, to the printed document cleat build writes
    straight to ``<wasm>.schema.json``.
    """
    import json
    import sys

    from cleat_sdk.jsonschema_emitter import main

    declared = tmp_path / "declared.py"
    declared.write_text(
        "from cleat_sdk.entry import cleat_entry\n"
        "from cleat_sdk.host_calls import HostCalls\n\n"
        '@cleat_entry("InventorySync", expose="internal")\n'
        "def inventory_sync(h: HostCalls, item_id: str) -> str:\n"
        '    return "{}"\n'
    )
    monkeypatch.setattr(sys, "argv", ["jsonschema_emitter", str(declared), "inventory_sync"])
    rc = main()
    assert rc == 0
    doc = json.loads(capsys.readouterr().out)
    assert doc["InventorySync"]["exposure"] == "internal"

    undeclared = tmp_path / "undeclared.py"
    undeclared.write_text(
        "from cleat_sdk.entry import cleat_entry\n"
        "from cleat_sdk.host_calls import HostCalls\n\n"
        "@cleat_entry\n"
        "def plain_one(h: HostCalls, item_id: str) -> str:\n"
        '    return "{}"\n'
    )
    monkeypatch.setattr(sys, "argv", ["jsonschema_emitter", str(undeclared), "plain_one"])
    rc = main()
    assert rc == 0
    doc = json.loads(capsys.readouterr().out)
    assert "exposure" not in doc["plain_one"], (
        'engine.EntryPointSchema.Exposure\'s `json:"exposure,omitempty"` -- writing a '
        'literal "auth" here would be indistinguishable on the Go side from a real '
        "declaration, so an undeclared entry point must OMIT the key"
    )
