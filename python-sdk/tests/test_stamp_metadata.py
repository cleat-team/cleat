"""Tests for scripts/stamp_metadata.py's WASM header handling (cleat#2936).

Before this fix, find_custom_section rejected every WASM Component Model
binary outright ("unsupported WASM version (only v1 supported)"), because it
checked bytes 4-8 against the core module's exact u32 LE version encoding.
componentize-py -- the only toolchain this SDK's `cleat build --target
python` actually produces output with -- emits a Component Model binary, not
a core module, so this script never worked on real Python build output; only
on a synthetic core-module-shaped fixture no real deployment produces.

See scripts/stamp_metadata.py's COMPONENT_LAYER comment for the header
format this is about.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest


def _sdk_root() -> Path:
    return Path(__file__).resolve().parent.parent


def _scripts_dir() -> Path:
    return _sdk_root() / "scripts"


def _import_stamp_metadata():
    scripts_dir = str(_scripts_dir())
    sys.path.insert(0, scripts_dir)
    try:
        import stamp_metadata

        return stamp_metadata
    finally:
        sys.path.remove(scripts_dir)


# The real, checked-in componentize-py output used throughout cleat#2936's
# investigation: 19,300,914 bytes, header 00 61 73 6d 0d 00 01 00 (version
# 13, layer 1), 642 top-level sections. See wasm/component_header_test.go
# (the Go-side sibling of this test) for the section-walk that confirmed a
# component's custom sections (id 0) frame identically to a core module's.
_COMPONENT_ARTIFACT = (
    _sdk_root().parent / "tests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm"
)


def _component_bytes() -> bytes:
    if not _COMPONENT_ARTIFACT.exists():
        pytest.skip(f"fixture {_COMPONENT_ARTIFACT} is missing; it is checked in")
    return _COMPONENT_ARTIFACT.read_bytes()


def test_component_layer_constant_matches_the_real_artifacts_header():
    sm = _import_stamp_metadata()
    data = _component_bytes()
    assert data[:4] == sm.WASM_MAGIC
    assert data[6:8] == sm.COMPONENT_LAYER
    assert data[4:8] != sm.WASM_VERSION


def test_find_custom_section_rejects_bad_magic():
    sm = _import_stamp_metadata()
    data = bytearray(_component_bytes())
    data[0] = 0xFF
    with pytest.raises(ValueError, match="bad magic number"):
        sm.find_custom_section(bytes(data), "cleat.metadata")


def test_find_custom_section_rejects_an_unrecognized_layer():
    """Negative control: accepting COMPONENT_LAYER must not make the header
    check accept everything. A layer value that is neither a core module's
    (0) nor a component's (1) must still be rejected."""
    sm = _import_stamp_metadata()
    data = bytearray(_component_bytes())
    data[6] = 0x02
    with pytest.raises(ValueError, match="unsupported WASM header"):
        sm.find_custom_section(bytes(data), "cleat.metadata")


def test_find_custom_section_still_accepts_a_core_module():
    """The widening must not regress the format this script worked on
    before cleat#2936: a plain core-module header (version 1, no
    Component Model framing at all)."""
    sm = _import_stamp_metadata()
    core_module = bytes.fromhex("0061736d01000000")
    payload, start, end = sm.find_custom_section(core_module, "cleat.metadata")
    assert payload is None  # no such section, but no header error either
    assert (start, end) == (0, 0)


def test_inject_and_read_metadata_round_trips_on_a_real_component_binary():
    """The regression test for cleat#2936 itself: inject_metadata/
    read_metadata against the actual componentize-py output, not a
    hand-built fixture."""
    sm = _import_stamp_metadata()
    original = _component_bytes()

    meta = {
        "workflow_name": "CallAllPlugins",
        "workflow_version": 1,
        "min_compatible_version": 1,
        "abi_version": 1,
        "plugin_deps": {},
        "child_binding_policy": "",
        "sdk_language": "python",
        "sdk_version": "0.4.0",
        "created_at": "2026-10-01T00:00:00Z",
        "language": "python",
        "entry_points": ["call_all_plugins"],
    }
    modified = sm.inject_metadata(original, meta)
    assert len(modified) > len(original)

    round_tripped = sm.read_metadata(modified)
    assert round_tripped == meta


def test_build_metadata_entry_points_reach_a_real_component_binary(monkeypatch):
    """cleat#2914's own follow-through: build_metadata's entry_points field,
    once stamped by inject_metadata, is readable back off the real artifact
    -- not just a dict in memory."""
    sm = _import_stamp_metadata()
    original = _component_bytes()

    monkeypatch.setenv("CLEAT_WORKFLOW_NAME", "CallAllPlugins")
    monkeypatch.setenv("CLEAT_WORKFLOW_VERSION", "1")
    monkeypatch.setenv("CLEAT_ENTRY_POINTS", "call_all_plugins")

    import argparse

    args = argparse.Namespace(
        name=None,
        version=None,
        min_version=None,
        abi_version=None,
        plugin_deps=None,
        child_binding_policy=None,
        language=None,
        entry_points=None,
    )
    meta = sm.build_metadata(args)
    modified = sm.inject_metadata(original, meta)
    round_tripped = sm.read_metadata(modified)
    assert round_tripped["entry_points"] == ["call_all_plugins"]
