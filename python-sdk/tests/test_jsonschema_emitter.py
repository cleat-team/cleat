"""Tests for cleat_sdk.jsonschema_emitter (cleat#2914).

See that module's own docstring for the governing rule and the five
documented divergences from internal/jsonschema's Go emitter. These tests
pin the SHAPE of the schema each divergence produces, not just that a
schema is produced at all -- a schema emitter that silently regressed to
Go's rules (a lone string gets anySchema, only pointer/slice/map are
nullable, a nested dataclass gains "required") would still "pass" a test
that only checked for SOME non-empty schema.
"""

from dataclasses import dataclass
from typing import Optional

import pytest

try:
    from cleat_sdk.entry import cleat_entry
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
    """Optional[str] and a plain str get the IDENTICAL schema: _from_dict's
    null handling does not depend on whether the annotation says Optional,
    so there is nothing for Optional to add."""

    @cleat_entry
    def wf(h: HostCalls, a: str, b: Optional[str] = None) -> str:
        return "{}"

    props = entry_point_param_schema(_func(wf))["properties"]
    assert props["a"] == props["b"]


def test_multi_arm_union_is_any_schema():
    @cleat_entry
    def wf(h: HostCalls, v: int | str) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))["properties"]["v"]
    assert schema == {}


def test_nested_dataclass_has_no_required_array():
    """Divergence 3: a nested dataclass's missing-field TypeError is raised
    inside export_wrapper's own try/except, becoming a completed run's
    {"error": ...} today -- not a refusal -- so this module does not newly
    turn that into a pre-start 400 by emitting "required" at a nested
    level."""

    @dataclass
    class Address:
        street: str
        city: str

    @cleat_entry
    def wf(h: HostCalls, address: Address) -> str:
        return "{}"

    schema = entry_point_param_schema(_func(wf))["properties"]["address"]
    assert schema["type"] == ["object", "null"]
    assert set(schema["properties"]) == {"street", "city"}
    assert "required" not in schema
    assert schema["additionalProperties"] is True


def test_self_referential_dataclass_does_not_recurse_forever():
    @dataclass
    class Node:
        value: int
        next: Optional["Node"] = None

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
