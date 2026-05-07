"""Thorough unit tests for the memory.py bit-packing functions.

Tests all encode/decode round-trips, edge cases, constants, and string I/O.
"""

import json
import pytest

from cleat_sdk import memory


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


@pytest.fixture(autouse=True)
def setup_memory():
    """Set up a sufficiently large bytearray to represent WASM linear memory
    before each test, and restore the original afterward."""
    old = memory._memory
    memory._memory = bytearray(memory.OUTPUT_OFFSET + memory.OUT_BUF_SIZE)
    yield
    memory._memory = old


# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------


class TestConstants:
    """Verify all public constants match the ABI specification."""

    def test_constants(self):
        assert memory.OUT_BUF_SIZE == 65536
        assert memory.SCRATCH_BASE == 10 * 1024 * 1024  # 0xA00000
        assert memory.OUTPUT_OFFSET == 10 * 1024 * 1024 + 65536  # 0xA10000
        assert memory.SUSPEND_SENTINEL == 1 << 62  # 0x4000000000000000
        assert memory.SLEEP_STATUS_COMPLETED == 0
        assert memory.SLEEP_STATUS_SUSPEND == 1
        assert memory.POLL_SIGNAL_FOUND == 0x0100


# ---------------------------------------------------------------------------
# Export result encoding / decoding
# ---------------------------------------------------------------------------


class TestExportResult:
    """Tests for encode_export_result / decode_export_result."""

    @pytest.mark.parametrize(
        "err_code, actual_len",
        [
            (0, 0),
            (0, 42),
            (1, 12),
            (0, 65535),
            (255, 0x7FFF_FFFF),
            (0, 0xFFFF_FFFF),
            (0xFFFF_FFFF, 0),
            (0xDEAD, 0xBEEF),
            (0, 1),
            (1 << 31, 1 << 31),
        ],
    )
    def test_encode_decode_export_result_roundtrip(self, err_code, actual_len):
        """Round-trip encode then decode for various error codes and lengths."""
        encoded = memory.encode_export_result(err_code, actual_len)
        decoded = memory.decode_export_result(encoded)
        assert decoded == (err_code & 0xFFFF_FFFF, actual_len & 0xFFFF_FFFF), (
            f"round-trip failed for err_code={err_code}, actual_len={actual_len}, "
            f"encoded=0x{encoded:016x}"
        )

    def test_decode_export_result_known_pattern(self):
        """Low 32 bits = errCode (1), high 32 bits = actualLen (42)."""
        result = (42 << 32) | 1  # 0x0000_002A_0000_0001
        assert memory.decode_export_result(result) == (1, 42)

    def test_decode_export_result_zero(self):
        """All bits zero."""
        assert memory.decode_export_result(0) == (0, 0)

    def test_decode_export_result_max(self):
        """All bits set."""
        result = 0xFFFF_FFFF_FFFF_FFFF
        assert memory.decode_export_result(result) == (0xFFFF_FFFF, 0xFFFF_FFFF)

    def test_encode_decode_negative_i64(self):
        """Ensure signed i64 values (Python int) round-trip correctly through
        encode/decode."""
        encoded = memory.encode_export_result(0, 0x8000_0000)
        decoded = memory.decode_export_result(encoded)
        assert decoded == (0, 0x8000_0000)

    def test_encode_export_result_large(self):
        """encoded value must be non-negative (Python int can be arbitrarily large,
        but the packed result fits in 64 bits)."""
        encoded = memory.encode_export_result(255, 0x7FFF_FFFF)
        assert encoded >= 0
        assert memory.decode_export_result(encoded) == (255, 0x7FFF_FFFF)


# ---------------------------------------------------------------------------
# durable_call result decoding
# ---------------------------------------------------------------------------


class TestDurableCallResult:
    """Tests for decode_durable_call_result.

    Bit layout:
        bits  0-7  = errCode
        bits  8-39 = callErrorCode
        bits 40-63 = responseLen
    """

    def test_decode_durable_call_result_success(self):
        """errCode=0, callErrorCode=0, responseLen=100."""
        result = (100 << 40) | (0 << 8) | 0
        response_len, call_error_code, err_code = memory.decode_durable_call_result(result)
        assert err_code == 0
        assert call_error_code == 0
        assert response_len == 100

    def test_decode_durable_call_result_error(self):
        """errCode=1, callErrorCode=5, responseLen=50."""
        result = (50 << 40) | (5 << 8) | 1
        response_len, call_error_code, err_code = memory.decode_durable_call_result(result)
        assert err_code == 1
        assert call_error_code == 5
        assert response_len == 50

    def test_decode_durable_call_result_max_values(self):
        """errCode=0xFF, callErrorCode=0xFFFFFFFF, responseLen=0xFFFFFF."""
        result = (0xFF_FFFF << 40) | (0xFFFF_FFFF << 8) | 0xFF
        response_len, call_error_code, err_code = memory.decode_durable_call_result(result)
        assert err_code == 0xFF
        assert call_error_code == 0xFFFF_FFFF
        assert response_len == 0xFF_FFFF

    def test_decode_durable_call_result_zero(self):
        assert memory.decode_durable_call_result(0) == (0, 0, 0)

    def test_decode_durable_call_result_negative_input(self):
        """Negative i64 values should be handled via _to_u64."""
        # High bit (sign) in responseLen field
        result = (1 << 63) | (1 << 40)
        response_len, call_error_code, err_code = memory.decode_durable_call_result(result)
        # responseLen is 24 bits so the sign bit gets masked
        assert response_len == 0x80_0000  # bit 63 >> 40 = bit 23
        assert err_code == 0

    def test_decode_durable_call_result_suspect_negative(self):
        """When the i64 value is negative (high bit set in practice),
        _to_u64 must convert correctly."""
        # Simulate a result where the i64 representation would be negative.
        # Set bit 63 so Python sees it as a negative value when cast.
        result = 0x8000_0000_0000_0000  # bit 63 set
        response_len, call_error_code, err_code = memory.decode_durable_call_result(result)
        # bit 63 >> 40 = bit 23. response_len is 24-bit mask: 0x800000.
        assert response_len == 0x80_0000
        assert call_error_code == 0  # bits 8-39 are zero
        assert err_code == 0

    @pytest.mark.parametrize(
        "packed, expected",
        [
            ((0 << 40) | (0 << 8) | 0, (0, 0, 0)),
            ((42 << 40) | (0 << 8) | 0, (42, 0, 0)),
            ((0 << 40) | (99 << 8) | 0, (0, 99, 0)),
            ((0 << 40) | (0 << 8) | 7, (0, 0, 7)),
            ((255 << 40) | (0 << 8) | 0, (255, 0, 0)),
            ((0 << 40) | (0xFFFF_FFFF << 8) | 0xFF, (0, 0xFFFF_FFFF, 0xFF)),
        ],
    )
    def test_decode_durable_call_result_parametrized(self, packed, expected):
        assert memory.decode_durable_call_result(packed) == expected


# ---------------------------------------------------------------------------
# Simple result decoding (defer, continue_as_new, child_workflow, etc.)
# ---------------------------------------------------------------------------


class TestSimpleResult:
    """Tests for decode_simple_result.

    Bit layout:
        bits  0-7  = errCode
        bits 32-63 = extra
    """

    def test_decode_simple_result_success(self):
        """errCode=0, extra=42."""
        result = (42 << 32) | 0
        extra, err_code = memory.decode_simple_result(result)
        assert err_code == 0
        assert extra == 42

    def test_decode_simple_result_error(self):
        """errCode=7, extra=12345."""
        result = (12345 << 32) | 7
        extra, err_code = memory.decode_simple_result(result)
        assert err_code == 7
        assert extra == 12345

    def test_decode_simple_result_max_extra(self):
        """extra all 32 bits set."""
        result = (0xFFFF_FFFF << 32) | 0xFF
        extra, err_code = memory.decode_simple_result(result)
        assert err_code == 0xFF
        assert extra == 0xFFFF_FFFF

    def test_decode_simple_result_zero(self):
        assert memory.decode_simple_result(0) == (0, 0)

    @pytest.mark.parametrize(
        "packed, expected",
        [
            ((0 << 32) | 0, (0, 0)),
            ((1 << 32) | 1, (1, 1)),
            ((0xFFFF_FFFF << 32) | 0, (0xFFFF_FFFF, 0)),
            ((0 << 32) | 0xFF, (0, 0xFF)),
            ((42 << 32) | 3, (42, 3)),
        ],
    )
    def test_decode_simple_result_parametrized(self, packed, expected):
        assert memory.decode_simple_result(packed) == expected


# ---------------------------------------------------------------------------
# Sleep result decoding
# ---------------------------------------------------------------------------


class TestSleepResult:
    """Tests for decode_sleep_result.

    Bit layout:
        bits  0-55 = durationMs
        bits 56-63 = status (0 = completed, 1 = suspend)
    """

    def test_decode_sleep_completed(self):
        """status=0 (completed), duration=5000."""
        result = 5000
        status, duration = memory.decode_sleep_result(result)
        assert status == memory.SLEEP_STATUS_COMPLETED
        assert duration == 5000

    def test_decode_sleep_suspend(self):
        """status=1 (suspend), duration=10000."""
        result = (1 << 56) | 10000
        status, duration = memory.decode_sleep_result(result)
        assert status == memory.SLEEP_STATUS_SUSPEND
        assert duration == 10000

    def test_decode_sleep_large_duration(self):
        """Maximum 56-bit duration with status=0."""
        result = 0x00FF_FFFF_FFFF_FFFF
        status, duration = memory.decode_sleep_result(result)
        assert status == 0  # bits 56-63 are clear
        assert duration == 0x00FF_FFFF_FFFF_FFFF

    def test_decode_sleep_max_duration_with_status(self):
        """Max duration and status=1."""
        result = (1 << 56) | 0x00FF_FFFF_FFFF_FFFF
        status, duration = memory.decode_sleep_result(result)
        assert status == 1
        assert duration == 0x00FF_FFFF_FFFF_FFFF

    def test_decode_sleep_zero(self):
        assert memory.decode_sleep_result(0) == (0, 0)

    def test_decode_sleep_negative_input(self):
        """When the i64 value is negative (bit 63 set in practice),
        the duration must be masked to 56 bits."""
        result = 0x8000_0000_0000_0000  # bit 63 set, bit 56-63 would give status
        status, duration = memory.decode_sleep_result(result)
        # bit 63 lands in status field (bits 56-63)
        assert status == 0x80
        # duration = bits 0-55 = 0
        assert duration == 0

    @pytest.mark.parametrize(
        "packed, expected_status, expected_duration",
        [
            (0, 0, 0),
            (5000, 0, 5000),
            ((1 << 56), memory.SLEEP_STATUS_SUSPEND, 0),
            ((1 << 56) | 999, memory.SLEEP_STATUS_SUSPEND, 999),
            ((0xFF << 56) | 0x00FF_FFFF_FFFF_FFFF, 0xFF, 0x00FF_FFFF_FFFF_FFFF),
        ],
    )
    def test_decode_sleep_result_parametrized(self, packed, expected_status, expected_duration):
        status, duration = memory.decode_sleep_result(packed)
        assert status == expected_status
        assert duration == expected_duration


# ---------------------------------------------------------------------------
# await_signals result decoding
# ---------------------------------------------------------------------------


class TestAwaitSignalsResult:
    """Tests for decode_await_signals_result.

    Bit layout:
        bits  0-15 = errCode
        bits 16-31 = timedOut (non-zero = timed out)
        bits 32-47 = payloadLen
        bits 48-63 = sigNameLen
    """

    def test_decode_await_signals_success(self):
        """errCode=0, timedOut=False, payloadLen=20, sigNameLen=10."""
        result = (10 << 48) | (20 << 32) | (0 << 16) | 0
        sig_name_len, payload_len, timed_out, err_code = memory.decode_await_signals_result(result)
        assert err_code == 0
        assert not timed_out
        assert payload_len == 20
        assert sig_name_len == 10

    def test_decode_await_signals_timed_out(self):
        """errCode=0, timedOut=True, payloadLen=0, sigNameLen=0."""
        result = (1 << 16)
        sig_name_len, payload_len, timed_out, err_code = memory.decode_await_signals_result(result)
        assert err_code == 0
        assert timed_out
        assert payload_len == 0
        assert sig_name_len == 0

    def test_decode_await_signals_all_fields(self):
        """All four fields non-zero."""
        result = (0xABCD << 48) | (0x1234 << 32) | (1 << 16) | 0x00FF
        sig_name_len, payload_len, timed_out, err_code = memory.decode_await_signals_result(result)
        assert err_code == 0x00FF
        assert timed_out
        assert payload_len == 0x1234
        assert sig_name_len == 0xABCD

    def test_decode_await_signals_max_values(self):
        """All fields at maximum."""
        result = (0xFFFF << 48) | (0xFFFF << 32) | (0xFFFF << 16) | 0xFFFF
        sig_name_len, payload_len, timed_out, err_code = memory.decode_await_signals_result(result)
        assert err_code == 0xFFFF
        assert timed_out  # non-zero
        assert payload_len == 0xFFFF
        assert sig_name_len == 0xFFFF

    def test_decode_await_signals_zero(self):
        assert memory.decode_await_signals_result(0) == (0, 0, False, 0)

    def test_decode_await_signals_timed_out_any_bit(self):
        """Any bit in the timedOut field (bits 16-31) should produce True."""
        result = (1 << 17)  # bit 17, not bit 16
        _, _, timed_out, _ = memory.decode_await_signals_result(result)
        assert timed_out

    @pytest.mark.parametrize(
        "packed, expected",
        [
            (0, (0, 0, False, 0)),
            ((1 << 48), (1, 0, False, 0)),
            ((1 << 32), (0, 1, False, 0)),
            ((1 << 16), (0, 0, True, 0)),
            (1, (0, 0, False, 1)),
            ((0xFFFF << 48) | (0xFFFF << 32) | (0xFFFF << 16) | 0xFFFF,
             (0xFFFF, 0xFFFF, True, 0xFFFF)),
        ],
    )
    def test_decode_await_signals_result_parametrized(self, packed, expected):
        assert memory.decode_await_signals_result(packed) == expected


# ---------------------------------------------------------------------------
# await_promise result decoding
# ---------------------------------------------------------------------------


class TestAwaitPromiseResult:
    """Tests for decode_await_promise_result.

    Bit layout:
        bits  0-15 = errCode
        bits 16-23 = timedOut (non-zero = timed out)
        bits 32-63 = resultLen
    """

    def test_decode_await_promise_success(self):
        """errCode=0, timedOut=False, resultLen=100."""
        result = (100 << 32) | (0 << 16) | 0
        result_len, timed_out, err_code = memory.decode_await_promise_result(result)
        assert err_code == 0
        assert not timed_out
        assert result_len == 100

    def test_decode_await_promise_timed_out(self):
        """errCode=0, timedOut=True, resultLen=0."""
        result = (1 << 16)
        result_len, timed_out, err_code = memory.decode_await_promise_result(result)
        assert err_code == 0
        assert timed_out
        assert result_len == 0

    def test_decode_await_promise_error(self):
        """errCode=5, timedOut=False, resultLen=200."""
        result = (200 << 32) | (0 << 16) | 5
        result_len, timed_out, err_code = memory.decode_await_promise_result(result)
        assert err_code == 5
        assert not timed_out
        assert result_len == 200

    def test_decode_await_promise_max_values(self):
        """All fields at maximum."""
        result = (0xFFFF_FFFF << 32) | (0xFF << 16) | 0xFFFF
        result_len, timed_out, err_code = memory.decode_await_promise_result(result)
        assert err_code == 0xFFFF
        assert timed_out
        assert result_len == 0xFFFF_FFFF

    def test_decode_await_promise_zero(self):
        assert memory.decode_await_promise_result(0) == (0, False, 0)

    def test_decode_await_promise_timed_out_any_bit(self):
        """Any bit in the timedOut field (bits 16-23) should produce True."""
        result = (1 << 17)
        _, timed_out, _ = memory.decode_await_promise_result(result)
        assert timed_out

    @pytest.mark.parametrize(
        "packed, expected",
        [
            (0, (0, False, 0)),
            ((42 << 32), (42, False, 0)),
            ((1 << 16), (0, True, 0)),
            (7, (0, False, 7)),
            ((0xFFFF_FFFF << 32) | (0xFF << 16) | 0xFFFF,
             (0xFFFF_FFFF, True, 0xFFFF)),
        ],
    )
    def test_decode_await_promise_result_parametrized(self, packed, expected):
        assert memory.decode_await_promise_result(packed) == expected


# ---------------------------------------------------------------------------
# poll_signal result decoding
# ---------------------------------------------------------------------------


class TestPollSignalResult:
    """Tests for decode_poll_signal_result.

    Bit layout:
        bits  0-7  = errCode
        bits  8-15 = found flag (0x0100 = signal found)
        bits 32-63 = payloadLen
    """

    def test_decode_poll_signal_found(self):
        """errCode=0, found=True, payloadLen=30."""
        result = (30 << 32) | (0x0100 << 8) | 0
        payload_len, found, err_code = memory.decode_poll_signal_result(result)
        assert err_code == 0
        assert found
        assert payload_len == 30

    def test_decode_poll_signal_not_found(self):
        """errCode=0, found=False, payloadLen=0."""
        result = 0
        payload_len, found, err_code = memory.decode_poll_signal_result(result)
        assert err_code == 0
        assert not found
        assert payload_len == 0

    def test_decode_poll_signal_found_with_error(self):
        """errCode=3, found=True, payloadLen=10."""
        result = (10 << 32) | (0x0100 << 8) | 3
        payload_len, found, err_code = memory.decode_poll_signal_result(result)
        assert err_code == 3
        assert found
        assert payload_len == 10

    def test_decode_poll_signal_max_values(self):
        """All fields at maximum."""
        result = (0xFFFF_FFFF << 32) | (0xFF << 8) | 0xFF
        payload_len, found, err_code = memory.decode_poll_signal_result(result)
        assert err_code == 0xFF
        assert found  # any non-zero in bits 8-15 -> found
        assert payload_len == 0xFFFF_FFFF

    def test_decode_poll_signal_zero(self):
        assert memory.decode_poll_signal_result(0) == (0, False, 0)

    def test_decode_poll_signal_any_bit_in_flag_field(self):
        """Any bit set in the flag field (bits 8-15) should be found=True,
        even if it's not 0x0100."""
        result = (1 << 9)  # bit 9, not bit 8
        _, found, _ = memory.decode_poll_signal_result(result)
        assert found

    @pytest.mark.parametrize(
        "packed, expected",
        [
            (0, (0, False, 0)),
            ((1 << 32), (1, False, 0)),
            ((0x0100 << 8), (0, True, 0)),
            (1, (0, False, 1)),
            ((42 << 32) | (0x0100 << 8) | 7, (42, True, 7)),
            ((0xFFFF_FFFF << 32) | (0xFF << 8) | 0xFF, (0xFFFF_FFFF, True, 0xFF)),
        ],
    )
    def test_decode_poll_signal_result_parametrized(self, packed, expected):
        assert memory.decode_poll_signal_result(packed) == expected


# ---------------------------------------------------------------------------
# poll_cancellation result decoding
# ---------------------------------------------------------------------------


class TestPollCancellationResult:
    """Tests for decode_poll_cancellation_result.

    Bit layout:
        bits  0-31 = cancelled (non-zero = cancelled)
        bits 32-63 = reasonLen
    """

    def test_decode_poll_cancel_not_cancelled(self):
        assert memory.decode_poll_cancellation_result(0) == (0, False)

    def test_decode_poll_cancel_cancelled(self):
        """cancelled=1, reasonLen=15."""
        result = (15 << 32) | 1
        reason_len, cancelled = memory.decode_poll_cancellation_result(result)
        assert cancelled
        assert reason_len == 15

    def test_decode_poll_cancel_cancelled_no_reason(self):
        """cancelled=1, reasonLen=0."""
        result = 1
        reason_len, cancelled = memory.decode_poll_cancellation_result(result)
        assert cancelled
        assert reason_len == 0

    def test_decode_poll_cancel_max_reason(self):
        """Maximum 32-bit reasonLen, cancelled flag set."""
        result = (0xFFFF_FFFF << 32) | 0xFFFF_FFFF
        reason_len, cancelled = memory.decode_poll_cancellation_result(result)
        assert cancelled
        assert reason_len == 0xFFFF_FFFF

    def test_decode_poll_cancel_all_bits(self):
        """All bits set (both fields saturated)."""
        result = 0xFFFF_FFFF_FFFF_FFFF
        reason_len, cancelled = memory.decode_poll_cancellation_result(result)
        assert cancelled
        assert reason_len == 0xFFFF_FFFF

    @pytest.mark.parametrize(
        "packed, expected",
        [
            (0, (0, False)),
            (1, (0, True)),
            (0xFFFF_FFFF, (0, True)),  # only low 32 bits set -> cancelled, reason=0
            ((5 << 32), (5, False)),   # reasonLen=5, cancelled=False
            ((5 << 32) | 1, (5, True)),
            ((0xFFFF_FFFF << 32) | 0, (0xFFFF_FFFF, False)),
        ],
    )
    def test_decode_poll_cancellation_parametrized(self, packed, expected):
        assert memory.decode_poll_cancellation_result(packed) == expected


# ---------------------------------------------------------------------------
# String I/O
# ---------------------------------------------------------------------------


class TestStringIO:
    """Tests for read_string and write_string."""

    def test_read_write_string_roundtrip(self):
        """Write then read a basic ASCII string."""
        ptr = 1000
        text = "Hello, Cleat!"
        n = memory.write_string(ptr, text, 100)
        assert n == len(text)
        result = memory.read_string(ptr, n)
        assert result == text

    def test_read_write_empty_string(self):
        """Empty string round-trip."""
        ptr = 2000
        n = memory.write_string(ptr, "", 100)
        assert n == 0
        result = memory.read_string(ptr, n)
        assert result == ""

    def test_read_string_zero_length(self):
        """Reading with length 0 should return empty string regardless of memory contents."""
        result = memory.read_string(5000, 0)
        assert result == ""

    def test_write_string_zero_max_len(self):
        """Writing with max_len=0 should return 0 and not modify memory."""
        ptr = 3000
        n = memory.write_string(ptr, "hello", 0)
        assert n == 0

    def test_write_string_negative_max_len(self):
        """Writing with negative max_len should return 0."""
        ptr = 3000
        n = memory.write_string(ptr, "hello", -1)
        assert n == 0

    def test_write_string_truncated(self):
        """Strings longer than max_len get truncated."""
        ptr = 4000
        text = "This is a long string that will be truncated"
        max_len = 10
        n = memory.write_string(ptr, text, max_len)
        assert n == max_len
        result = memory.read_string(ptr, n)
        assert result == text[:max_len]

    def test_write_string_exact_fit(self):
        """String exactly max_len bytes should not be truncated."""
        ptr = 5000
        text = "12345"
        n = memory.write_string(ptr, text, 5)
        assert n == 5
        result = memory.read_string(ptr, n)
        assert result == text

    def test_write_string_truncation_unicode(self):
        """Unicode string truncation. Multi-byte characters may be truncated
        mid-character; decode should handle gracefully via errors='replace'."""
        ptr = 6000
        text = "Hello, 世界!"  # 世 is 3 bytes, 界 is 3 bytes
        max_len = 9  # "Hello, " (7 bytes) + partial 世
        n = memory.write_string(ptr, text, max_len)
        assert n == max_len
        result = memory.read_string(ptr, n)
        # The 世 character (3 bytes) would be truncated to only 2 bytes,
        # resulting in replacement character
        assert "Hello, " in result

    def test_read_string_unicode(self):
        """Unicode string round-trip."""
        ptr = 7000
        text = "Hello, 世界! 日本語"
        n = memory.write_string(ptr, text, 100)
        assert n == len(text.encode("utf-8"))
        result = memory.read_string(ptr, n)
        assert result == text

    def test_read_string_invalid_utf8(self):
        """Invalid UTF-8 bytes should be replaced with replacement character."""
        ptr = 8000
        memory._memory[ptr:ptr + 3] = b"\xff\xfe\x00"
        result = memory.read_string(ptr, 3)
        # \xff\xfe is not valid UTF-8, should be replaced
        assert "�" in result

    def test_write_string_large(self):
        """Write a large string (up to OUT_BUF_SIZE)."""
        ptr = 9000
        text = "a" * 50000
        n = memory.write_string(ptr, text, 50000)
        assert n == 50000
        result = memory.read_string(ptr, n)
        assert result == text

    def test_write_string_unicode_multi_byte_count(self):
        """Verify write_string returns byte count, not character count,
        for multi-byte characters."""
        ptr = 10000
        text = "世界"  # 2 characters, 6 bytes
        n = memory.write_string(ptr, text, 10)
        assert n == 6  # byte count
        result = memory.read_string(ptr, n)
        assert result == text

    def test_write_string_none_string(self):
        """Passing None or non-string as s argument should be handled
        gracefully by the encode call (will raise TypeError)."""
        ptr = 11000
        with pytest.raises((TypeError, AttributeError)):
            memory.write_string(ptr, None, 10)  # type: ignore

    def test_read_write_separate_pointers(self):
        """Reading from one region and writing to another should be
        independent."""
        ptr_a = 12000
        ptr_b = 13000
        text_a = "AAA"
        text_b = "BBB"
        memory.write_string(ptr_a, text_a, 10)
        memory.write_string(ptr_b, text_b, 10)
        assert memory.read_string(ptr_a, 3) == text_a
        assert memory.read_string(ptr_b, 3) == text_b

    def test_write_string_overwrite(self):
        """Writing a shorter string should leave the old data beyond the
        new length. We only verify the new content."""
        ptr = 14000
        memory.write_string(ptr, "Hello World", 20)
        memory.write_string(ptr, "Hi", 20)
        # The buffer still has "Hi" at the start, but we don't care
        # what comes after since we use the returned length
        n = memory.write_string(ptr, "Hi", 20)
        assert n == 2
        assert memory.read_string(ptr, n) == "Hi"


# ---------------------------------------------------------------------------
# _to_u64 helper
# ---------------------------------------------------------------------------


class TestToU64:
    """Tests for the internal _to_u64 helper."""

    def test_to_u64_positive(self):
        assert memory._to_u64(42) == 42

    def test_to_u64_zero(self):
        assert memory._to_u64(0) == 0

    def test_to_u64_negative(self):
        """-1 as i64 should become 0xFFFFFFFFFFFFFFFF."""
        assert memory._to_u64(-1) == 0xFFFF_FFFF_FFFF_FFFF

    def test_to_u64_max_positive(self):
        assert memory._to_u64(0x7FFF_FFFF_FFFF_FFFF) == 0x7FFF_FFFF_FFFF_FFFF

    def test_to_u64_overflow(self):
        """Values larger than 64 bits are masked."""
        assert memory._to_u64(1 << 64) == 0
        assert memory._to_u64((1 << 64) + 42) == 42
