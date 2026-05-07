"""WASM linear memory helpers and bit-packing decoders for the Cleat ABI.

This module provides constants, string I/O functions, and bit-packing
encode/decode functions that match the Cleat ABI specification.

For the MVP, a module-level ``_memory`` bytearray represents WASM linear
memory. This can be replaced with actual WASM memory access when running
in a real WASM runtime.

Bit-packing conventions match the Rust SDK at
``crates/durable-sdk/src/memory.rs`` and the ABI specification in
``ABI.md``.
"""

from typing import Tuple

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

OUT_BUF_SIZE: int = 65536
"""Output buffer size in bytes (64 KiB)."""

SCRATCH_BASE: int = 10 * 1024 * 1024  # 0xA00000
"""Scratch region base offset in linear memory (10 MiB = 0xA00000)."""

OUTPUT_OFFSET: int = SCRATCH_BASE + OUT_BUF_SIZE  # 0xA10000
"""Output buffer offset (10 MiB + 64 KiB = 0xA10000)."""

SUSPEND_SENTINEL: int = 1 << 62  # 0x4000000000000000
"""Suspend sentinel value returned by export functions when the workflow
needs to suspend (e.g., for a timer or signal)."""

SLEEP_STATUS_COMPLETED: int = 0
"""Sleep completed normally (replay path)."""

SLEEP_STATUS_SUSPEND: int = 1
"""Sleep should cause workflow suspension (fresh execution path)."""

POLL_SIGNAL_FOUND: int = 0x0100
"""PollSignal found flag value (bit 8 set)."""

# ---------------------------------------------------------------------------
# Module-level WASM linear memory (MVP)
# ---------------------------------------------------------------------------

_INITIAL_MEMORY_SIZE: int = OUTPUT_OFFSET + OUT_BUF_SIZE  # ~10 MiB + 128 KiB

_memory: bytearray = bytearray(_INITIAL_MEMORY_SIZE)
"""Module-level bytearray representing WASM linear memory for the MVP.

Initialized to the minimum WASM memory size required by the ABI (10 MiB + 128 KiB).
Replace with actual WASM memory access when running in a real WASM runtime.
"""


def _ensure_memory_size(min_size: int) -> None:
    """Grow the module-level memory to at least ``min_size`` bytes if needed."""
    if len(_memory) < min_size:
        _memory.extend(bytearray(min_size - len(_memory)))

# ---------------------------------------------------------------------------
# String I/O
# ---------------------------------------------------------------------------


def read_string(ptr: int, length: int) -> str:
    """Read a UTF-8 string from WASM linear memory at ``(ptr, length)``.

    Args:
        ptr: Pointer to the start of the UTF-8 data.
        length: Number of bytes to read.

    Returns:
        The decoded string, or empty string when ``length <= 0``.
    """
    if length <= 0:
        return ""
    return _memory[ptr : ptr + length].decode("utf-8", errors="replace")


def write_string(ptr: int, s: str, max_len: int) -> int:
    """Write a UTF-8 string to WASM linear memory at ``ptr``, truncating
    to at most ``max_len`` bytes.

    Args:
        ptr: Destination pointer in linear memory.
        s: String to encode.
        max_len: Maximum number of bytes to write (capacity of the buffer).

    Returns:
        The number of bytes actually written.
    """
    if max_len <= 0 or not s:
        return 0
    data = s.encode("utf-8")
    n = min(len(data), max_len)
    _memory[ptr : ptr + n] = data[:n]
    return n


# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------


def _to_u64(value: int) -> int:
    """Convert a signed i64 value to its unsigned 64-bit representation."""
    return value & 0xFFFFFFFFFFFFFFFF


# ---------------------------------------------------------------------------
# Export result encoding / decoding
# ---------------------------------------------------------------------------


def encode_export_result(err_code: int, actual_len: int) -> int:
    """Encode the export return value.

    Bit layout:
        bits  0-31 = errCode
        bits 32-63 = actualLen

    Args:
        err_code: Error code (0 = success).
        actual_len: Bytes written to the output buffer.

    Returns:
        Packed i64 for WASM return.
    """
    return ((actual_len & 0xFFFFFFFF) << 32) | (err_code & 0xFFFFFFFF)


def decode_export_result(result: int) -> Tuple[int, int]:
    """Decode the export return value.

    Bit layout:
        bits  0-31 = errCode
        bits 32-63 = actualLen

    Returns:
        Tuple of ``(err_code, actual_len)``.
    """
    r = _to_u64(result)
    err_code = r & 0xFFFFFFFF
    actual_len = (r >> 32) & 0xFFFFFFFF
    return (err_code, actual_len)


# ---------------------------------------------------------------------------
# durable_call result decoding
# ---------------------------------------------------------------------------


def decode_durable_call_result(result: int) -> Tuple[int, int, int]:
    """Decode a ``durable_call`` result.

    Bit layout:
        bits  0-7  = errCode (8 bits)
        bits  8-39 = callErrorCode (32 bits)
        bits 40-63 = responseLen (24 bits)

    Returns:
        Tuple of ``(response_len, call_error_code, err_code)``.
    """
    r = _to_u64(result)
    response_len = (r >> 40) & 0xFFFFFF
    call_error_code = (r >> 8) & 0xFFFFFFFF
    err_code = r & 0xFF
    return (response_len, call_error_code, err_code)


# ---------------------------------------------------------------------------
# Simple result decoding (shared by many host calls)
# ---------------------------------------------------------------------------


def decode_simple_result(result: int) -> Tuple[int, int]:
    """Decode a simple result (used by defer, continue_as_new,
    child_workflow, await_child, and others).

    Bit layout:
        bits  0-7  = errCode (8 bits)
        bits 32-63 = extra (32 bits)

    Returns:
        Tuple of ``(extra, err_code)``.
    """
    r = _to_u64(result)
    extra = (r >> 32) & 0xFFFFFFFF
    err_code = r & 0xFF
    return (extra, err_code)


# ---------------------------------------------------------------------------
# Sleep result decoding
# ---------------------------------------------------------------------------


def decode_sleep_result(result: int) -> Tuple[int, int]:
    """Decode a ``durable_sleep`` result.

    Bit layout:
        bits  0-55 = durationMs (56 bits)
        bits 56-63 = status (8 bits): 0 = completed, 1 = suspend

    Returns:
        Tuple of ``(status, duration_ms)``.
    """
    r = _to_u64(result)
    status = (r >> 56) & 0xFF
    duration_ms = r & 0x00FFFFFFFFFFFFFF
    return (status, duration_ms)


# ---------------------------------------------------------------------------
# await_signals result decoding
# ---------------------------------------------------------------------------


def decode_await_signals_result(result: int) -> Tuple[int, int, bool, int]:
    """Decode a ``durable_await_signals`` result.

    Bit layout:
        bits  0-15 = errCode (16 bits)
        bits 16-31 = timedOut flag (non-zero = timed out)
        bits 32-47 = payloadLen (16 bits)
        bits 48-63 = sigNameLen (16 bits)

    Returns:
        Tuple of ``(sig_name_len, payload_len, timed_out, err_code)``.
    """
    r = _to_u64(result)
    sig_name_len = (r >> 48) & 0xFFFF
    payload_len = (r >> 32) & 0xFFFF
    timed_out = ((r >> 16) & 0xFFFF) != 0
    err_code = r & 0xFFFF
    return (sig_name_len, payload_len, timed_out, err_code)


# ---------------------------------------------------------------------------
# await_promise result decoding
# ---------------------------------------------------------------------------


def decode_await_promise_result(result: int) -> Tuple[int, bool, int]:
    """Decode a ``durable_await_promise`` result.

    Bit layout:
        bits  0-15 = errCode (16 bits)
        bits 16-23 = timedOut flag (non-zero = timed out)
        bits 32-63 = resultLen (32 bits)

    Returns:
        Tuple of ``(result_len, timed_out, err_code)``.
    """
    r = _to_u64(result)
    result_len = (r >> 32) & 0xFFFFFFFF
    timed_out = ((r >> 16) & 0xFF) != 0
    err_code = r & 0xFFFF
    return (result_len, timed_out, err_code)


# ---------------------------------------------------------------------------
# poll_signal result decoding
# ---------------------------------------------------------------------------


def decode_poll_signal_result(result: int) -> Tuple[int, bool, int]:
    """Decode a ``durable_poll_signal`` result.

    Bit layout:
        bits  0-7  = errCode (8 bits)
        bits  8-15 = found flag (0x0100 = signal found)
        bits 32-63 = payloadLen (32 bits)

    Returns:
        Tuple of ``(payload_len, found, err_code)``.
    """
    r = _to_u64(result)
    payload_len = (r >> 32) & 0xFFFFFFFF
    flags = r & 0xFFFFFFFF
    err_code = flags & 0xFF
    found = (flags >> 8) != 0
    return (payload_len, found, err_code)


# ---------------------------------------------------------------------------
# poll_cancellation result decoding
# ---------------------------------------------------------------------------


def decode_poll_cancellation_result(result: int) -> Tuple[int, bool]:
    """Decode a ``durable_poll_cancellation`` result.

    Bit layout:
        bits  0-31 = cancelled flag (non-zero = cancelled)
        bits 32-63 = reasonLen (32 bits)

    Returns:
        Tuple of ``(reason_len, cancelled)``.
    """
    r = _to_u64(result)
    reason_len = (r >> 32) & 0xFFFFFFFF
    cancelled = (r & 0xFFFFFFFF) != 0
    return (reason_len, cancelled)
