"""The shared entry-point binding table, run against the Python SDK.

cleat#1065. The five SDKs do not share an entry-point shape, and the divergence
survived because nothing compared them -- cleat#1046 turned an absent int into a
hard bind error, passed cleat's entire suite, and was caught downstream by
cleat-ports.

``tests/conformance/entry_point_binding_cases.json`` is that comparison. This
file is Python's reader; Go's is ``engine/entry_point_binding_conformance_test.go``.

It characterises what the SDK does TODAY, deliberately. The owner's decision on
cleat#1065 is "an absent declared parameter is an error unless declared
optional", and the flip is a versioned breaking change that has not happened.
Recording today's behaviour is what makes the flip visible when it lands.
"""

import json
from dataclasses import dataclass
from pathlib import Path

import pytest

from cleat_sdk.entry import cleat_entry
from cleat_sdk.host_calls import HostCalls

TABLE = Path(__file__).resolve().parents[2] / "tests" / "conformance" / "entry_point_binding_cases.json"


@dataclass
class Item:
    sku: str
    qty: int


def _cases():
    doc = json.loads(TABLE.read_text())
    return [c for c in doc["cases"] if "python" in c["expect"]]


def test_the_table_is_readable_and_not_empty():
    """The third outcome.

    A reader whose table went missing, moved, or lost its python rows would
    otherwise report zero cases and PASS -- a green that compared nothing, which
    is the exact failure this table exists to prevent.
    """
    assert TABLE.exists(), f"conformance table not found at {TABLE}"
    cases = _cases()
    assert len(cases) >= 4, f"only {len(cases)} python cases; the table has lost rows"


def _build(declared):
    """Build an entry point with the declared parameters, and return its wrapper."""
    spec = declared[0]
    kind = spec["kind"]
    optional = "optional_default" in spec

    if kind == "string" and optional:
        @cleat_entry
        def wf(h: HostCalls, note: str = spec["optional_default"]) -> str:
            return json.dumps({"bound": note})
    elif kind == "lone_string":
        # The single-string fast path Go and AssemblyScript have, and Python
        # does NOT: they hand a lone string parameter the whole payload, Python
        # binds by name always. Declared identically here so the difference is
        # the SDK's rather than the fixture's.
        @cleat_entry
        def wf(h: HostCalls, note: str) -> str:
            return json.dumps({"bound": note})
    elif kind == "string":
        @cleat_entry
        def wf(h: HostCalls, note: str) -> str:
            return json.dumps({"bound": note})
    elif kind == "int":
        @cleat_entry
        def wf(h: HostCalls, count: int) -> str:
            return json.dumps({"bound": count})
    elif kind == "composite":
        @cleat_entry
        def wf(h: HostCalls, item: Item) -> str:
            return json.dumps({"bound": item.sku})
    else:
        raise AssertionError(f"unhandled kind {kind!r}")
    return getattr(wf, "export_wrapper", wf), spec


def _classify(out, spec, payload):
    """Map the SDK's answer onto the table's semantic outcome tags.

    A tag, never a message: the SDKs word their errors differently and always
    will, which is why the table carries kinds rather than strings.
    """
    parsed = json.loads(out)
    if isinstance(parsed, dict) and "error" in parsed:
        return "refused"
    bound = json.loads(parsed)["bound"] if isinstance(parsed, str) else parsed["bound"]
    name = spec["name"]
    if name in payload:
        return "bound_value"
    if "optional_default" in spec and bound == spec["optional_default"]:
        return "bound_default"
    if spec.get("kind") == "lone_string" and bound == json.dumps(payload):
        return "bound_whole_payload"
    if bound in ("", 0, None):
        return "bound_zero"
    return f"bound_other({bound!r})"


@pytest.mark.parametrize("case", _cases(), ids=lambda c: c["name"])
def test_entry_point_binding_matches_the_shared_table(case):
    wrapper, spec = _build(case["declared"])
    payload = case["payload"]
    got = _classify(wrapper(json.dumps(payload)), spec, payload)
    want = case["expect"]["python"]
    assert got == want, (
        f"{case['name']}: Python bound {got!r}, the table says {want!r}.\n\n"
        f"why this case is in the table: {case['why']}\n\n"
        "If this changed deliberately, update tests/conformance/entry_point_binding_cases.json "
        "AND every other SDK's row -- the point of the table is that the SDKs are compared, so "
        "editing one reader to agree with itself is the one repair that is always wrong."
    )
