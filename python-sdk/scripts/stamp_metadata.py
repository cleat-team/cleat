#!/usr/bin/env python3
"""Post-compile metadata stamping for cleat Python WASM workflows.

Reads a compiled WASM binary and injects (or replaces) the "cleat.metadata"
custom section with workflow identity information. The host can read this
metadata without instantiating the module, enabling tools like "cleat deploy"
to validate and insert into workflow_defs.

Usage:
    python stamp_metadata.py <wasm_file> \\
        --name PlaceOrder \\
        --version 3 \\
        --min-version 1 \\
        --abi-version 1 \\
        --plugin-deps '{"llm": ">=1.2.0"}' \\
        [--output <output.wasm>]

Environment variable alternative:
    CLEAT_WORKFLOW_NAME=PlaceOrder \\
    CLEAT_WORKFLOW_VERSION=3 \\
    CLEAT_MIN_COMPATIBLE_VERSION=1 \\
    CLEAT_ABI_VERSION=1 \\
    CLEAT_PLUGIN_DEPS='{"llm": ">=1.2.0"}' \\
    python stamp_metadata.py <wasm_file>

This script does NOT require componentize-py. It is run AFTER the WASM
compilation step as a post-processing stage.
"""

import argparse
import json
import os
import sys
from datetime import datetime, timezone

# WASM constants
SECTION_ID_CUSTOM = 0x00
SECTION_NAME = "cleat.metadata"
WASM_MAGIC = b"\x00asm"
WASM_VERSION = b"\x01\x00\x00\x00"


def encode_uleb128(value: int) -> bytes:
    """Encode an integer as unsigned LEB128 varint."""
    result = bytearray()
    while True:
        b = value & 0x7F
        value >>= 7
        if value != 0:
            b |= 0x80
        result.append(b)
        if value == 0:
            break
    return bytes(result)


def decode_uleb128(data: bytes, offset: int) -> tuple[int, int]:
    """Decode an unsigned LEB128 varint starting at offset.
    Returns (value, bytes_consumed).
    """
    result = 0
    shift = 0
    consumed = 0

    # SIM113 suggests enumerate() for `consumed`. Suppressed on the increment
    # below: `consumed` is not a loop index, it is a byte count this function
    # returns, and the loop exits early via `return result, consumed`.
    # enumerate(..., start=1) yields the same numbers while making the returned
    # value look incidental to the iteration rather than the point of it.
    for i in range(offset, len(data)):
        b = data[i]
        consumed += 1  # noqa: SIM113
        result |= (b & 0x7F) << shift
        if b & 0x80 == 0:
            return result, consumed
        shift += 7
        if shift > 63:
            raise ValueError(
                f"invalid WASM binary at offset {offset}: corrupted section length encoding"
            )

    raise ValueError(f"invalid WASM binary at offset {offset}: truncated section length encoding")


def build_custom_section(name: str, payload: bytes) -> bytes:
    """Build a complete WASM custom section (section ID 0x00)."""
    name_bytes = name.encode("utf-8")
    name_len = encode_uleb128(len(name_bytes))
    full_payload = name_len + name_bytes + payload
    payload_len = encode_uleb128(len(full_payload))
    return bytes([SECTION_ID_CUSTOM]) + payload_len + full_payload


def find_custom_section(wasm_bytes: bytes, name: str) -> tuple[bytes | None, int, int]:
    """Find a custom section in a WASM binary.

    Returns (payload, section_start, section_end) or (None, 0, 0) if not found.
    section_start and section_end are the byte offsets of the entire section
    (including section ID and length).
    """
    if len(wasm_bytes) < 8:
        return None, 0, 0

    if wasm_bytes[:4] != WASM_MAGIC:
        raise ValueError("not a valid WASM file (bad magic number)")

    if wasm_bytes[4:8] != WASM_VERSION:
        raise ValueError("unsupported WASM version (only v1 supported)")

    offset = 8
    name_bytes = name.encode("utf-8")

    while offset < len(wasm_bytes):
        section_id = wasm_bytes[offset]
        section_start = offset
        offset += 1

        if offset >= len(wasm_bytes):
            break

        payload_len, consumed = decode_uleb128(wasm_bytes, offset)
        if consumed == 0:
            raise ValueError("Invalid LEB128 section length")
        offset += consumed

        section_end = offset + payload_len

        if offset + payload_len > len(wasm_bytes):
            raise ValueError("Section extends beyond end of WASM binary")

        if section_id == SECTION_ID_CUSTOM:
            # Try to read the section name
            name_len, nc = decode_uleb128(wasm_bytes, offset)
            if nc > 0:
                name_start = offset + nc
                name_end = name_start + name_len
                if name_end <= offset + payload_len:
                    section_name = wasm_bytes[name_start:name_end]
                    if section_name == name_bytes:
                        return wasm_bytes[name_end:section_end], section_start, section_end

        offset += payload_len

    return None, 0, 0


def inject_metadata(wasm_bytes: bytes, meta: dict) -> bytes:
    """Inject (or replace) a cleat.metadata custom section in a WASM binary."""
    json_payload = json.dumps(meta, ensure_ascii=False, separators=(",", ":")).encode("utf-8")

    # For simplicity: if section exists, reconstruct without it.
    existing_payload, s_start, s_end = find_custom_section(wasm_bytes, SECTION_NAME)
    if existing_payload is not None:
        wasm_bytes = wasm_bytes[:s_start] + wasm_bytes[s_end:]

    new_section = build_custom_section(SECTION_NAME, json_payload)
    return wasm_bytes + new_section


def read_metadata(wasm_bytes: bytes) -> dict | None:
    """Read cleat.metadata from a WASM binary, returning the parsed JSON dict."""
    payload, _, _ = find_custom_section(wasm_bytes, SECTION_NAME)
    if payload is None:
        return None
    return json.loads(payload.decode("utf-8"))


def build_metadata(args: argparse.Namespace) -> dict:
    """Build the metadata dict from command-line args or environment variables."""

    # Environment variable fallbacks.
    def env_or_arg(env_var: str, arg_value):
        return arg_value if arg_value is not None else os.environ.get(env_var)

    name = env_or_arg("CLEAT_WORKFLOW_NAME", args.name) or "unknown"
    version = env_or_arg("CLEAT_WORKFLOW_VERSION", args.version)
    min_version = env_or_arg("CLEAT_MIN_COMPATIBLE_VERSION", args.min_version)
    abi_version = env_or_arg("CLEAT_ABI_VERSION", args.abi_version)
    plugin_deps_str = env_or_arg("CLEAT_PLUGIN_DEPS", args.plugin_deps)
    child_binding_policy = env_or_arg("CLEAT_CHILD_BINDING_POLICY", args.child_binding_policy) or ""
    language = env_or_arg("CLEAT_LANGUAGE", args.language)

    # Parse numeric values from env (they come as strings).
    version = int(version) if version is not None else 0
    min_version = int(min_version) if min_version is not None else 1
    abi_version = int(abi_version) if abi_version is not None else 1

    plugin_deps = {}
    if plugin_deps_str:
        try:
            plugin_deps = json.loads(plugin_deps_str)
            if not isinstance(plugin_deps, dict):
                plugin_deps = {}
        except (json.JSONDecodeError, TypeError):
            plugin_deps = {}

    return {
        "workflow_name": name,
        "workflow_version": version,
        "min_compatible_version": min_version,
        "abi_version": abi_version,
        "plugin_deps": plugin_deps,
        "child_binding_policy": child_binding_policy,
        "sdk_language": "python",
        "sdk_version": "0.2.0",
        "created_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "language": language or "python",
    }


def main():
    parser = argparse.ArgumentParser(
        description="Stamp cleat metadata into a compiled WASM workflow binary"
    )
    parser.add_argument("wasm_file", help="Path to compiled WASM file")
    parser.add_argument("--name", help="Workflow name (or CLEAT_WORKFLOW_NAME env var)")
    parser.add_argument(
        "--version", type=int, help="Workflow version (or CLEAT_WORKFLOW_VERSION env var)"
    )
    parser.add_argument(
        "--min-version",
        type=int,
        help="Min compatible version (or CLEAT_MIN_COMPATIBLE_VERSION env var)",
    )
    parser.add_argument(
        "--abi-version", type=int, help="ABI version (or CLEAT_ABI_VERSION env var)"
    )
    parser.add_argument(
        "--plugin-deps", help="Plugin dependencies JSON (or CLEAT_PLUGIN_DEPS env var)"
    )
    parser.add_argument(
        "--child-binding-policy",
        default=None,
        dest="child_binding_policy",
        help="Child binding policy (or CLEAT_CHILD_BINDING_POLICY env var)",
    )
    parser.add_argument("--output", "-o", help="Output WASM path (default: overwrite input)")
    parser.add_argument(
        "--language",
        help="Source language for the WASM binary (or CLEAT_LANGUAGE env var)",
    )
    parser.add_argument(
        "--read", action="store_true", help="Read and display metadata instead of writing"
    )
    parser.add_argument("--verbose", "-v", action="store_true", help="Verbose output")

    args = parser.parse_args()

    with open(args.wasm_file, "rb") as f:
        wasm_bytes = f.read()

    # Read-only mode.
    if args.read:
        try:
            meta = read_metadata(wasm_bytes)
        except ValueError as e:
            print(f"Error reading {args.wasm_file}: {e}", file=sys.stderr)
            sys.exit(1)
        if meta is None:
            print(f"No '{SECTION_NAME}' section found in {args.wasm_file}")
            sys.exit(1)
        print(json.dumps(meta, indent=2))
        return

    # Build and inject metadata.
    meta = build_metadata(args)
    try:
        modified = inject_metadata(wasm_bytes, meta)
    except ValueError as e:
        print(f"Error processing {args.wasm_file}: {e}", file=sys.stderr)
        sys.exit(1)

    output = args.output or args.wasm_file

    with open(output, "wb") as f:
        f.write(modified)

    if args.verbose:
        print(f"Stamped metadata into {output}")
        print(f"  workflow_name:        {meta['workflow_name']}")
        print(f"  workflow_version:     {meta['workflow_version']}")
        print(f"  min_compatible_version: {meta['min_compatible_version']}")
        print(f"  abi_version:          {meta['abi_version']}")
        if meta["child_binding_policy"]:
            print(f"  child_binding_policy: {meta['child_binding_policy']}")
        if meta["plugin_deps"]:
            print(f"  plugin_deps:          {meta['plugin_deps']}")
        original_size = len(wasm_bytes)
        new_size = len(modified)
        print(f"  Size: {original_size} bytes -> {new_size} bytes (+{new_size - original_size})")
    else:
        print(f"Stamped cleat metadata into {output}")


if __name__ == "__main__":
    main()
