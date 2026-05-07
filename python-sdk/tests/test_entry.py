"""Tests for the ``@cleat_entry`` decorator and its helpers.

These tests verify that the decorator correctly wraps workflow entry-point
functions, injects ``HostCalls``, handles JSON input/output serialization,
and properly reports errors.

WASM linear memory is mocked by setting ``memory._memory`` to a large
bytearray before each test.
"""

import json

import pytest

try:
    from cleat_sdk import memory
    from cleat_sdk.entry import cleat_entry, _unwrap_result
    from cleat_sdk.host_calls import HostCalls
except ImportError as e:
    pytest.skip(
        f"Skipping entry tests: {e}.  "
        "entry.py and host_calls.py must exist.",
        allow_module_level=True,
    )

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


@pytest.fixture(autouse=True)
def setup_memory():
    """Set up a large enough linear memory before each test."""
    old = memory._memory
    memory._memory = bytearray(memory.OUTPUT_OFFSET + memory.OUT_BUF_SIZE)
    yield
    memory._memory = old


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _call_export(fn, input_dict):
    """Write *input_dict* as JSON to the scratch region and invoke *fn*
    (which is the ``export_wrapper`` returned by ``@cleat_entry``).

    Returns the packed ``i64`` result from the wrapper.
    """
    input_bytes = json.dumps(input_dict).encode("utf-8")
    ptr = memory.SCRATCH_BASE
    memory._memory[ptr : ptr + len(input_bytes)] = input_bytes
    return fn(ptr, len(input_bytes), memory.OUTPUT_OFFSET, memory.OUT_BUF_SIZE)


def _decode_output(packed):
    """Decode a packed export result and return ``(err_code, output_dict)``."""
    err_code, actual_len = memory.decode_export_result(packed)
    raw = memory.read_string(memory.OUTPUT_OFFSET, actual_len)
    output = json.loads(raw) if raw.strip() else {}
    return err_code, output


# ---------------------------------------------------------------------------
# @cleat_entry decorator
# ---------------------------------------------------------------------------


class TestCleatEntry:
    """Behavioural tests for the ``@cleat_entry`` decorator."""

    def test_cleat_entry_basic(self):
        """The decorator sets ``_is_cleat_entry`` and the returned wrapper
        follows the WASM export ABI signature."""

        @cleat_entry
        def my_func(h: HostCalls, name: str):
            return {"greeting": f"Hello, {name}"}

        # The decorated function IS the export wrapper
        assert my_func._is_cleat_entry is True

        # Verify the wrapper runs end-to-end
        packed = _call_export(my_func, {"name": "World"})
        err_code, output = _decode_output(packed)
        assert err_code == 0
        assert output == {"greeting": "Hello, World"}

    def test_cleat_entry_preserves_wrapped_name(self):
        """The original function name is preserved via @functools.wraps."""

        @cleat_entry
        def place_order(h: HostCalls, user_id: str):
            return {"user_id": user_id}

        assert place_order.__name__ == "place_order"

    def test_cleat_entry_explicit_name(self):
        """The ``@cleat_entry("ExplicitName")`` syntax works."""

        @cleat_entry("MyWorkflow")
        def my_func(h: HostCalls, x: int):
            return {"x": x}

        assert my_func._is_cleat_entry is True
        packed = _call_export(my_func, {"x": 42})
        err_code, output = _decode_output(packed)
        assert err_code == 0
        assert output == {"x": 42}

    def test_cleat_entry_with_host_calls(self):
        """A ``HostCalls`` instance is injected as the first argument."""

        @cleat_entry
        def my_func(h: HostCalls, data: str):
            # The injected object should have the key host-call methods
            # Note: ``now`` and ``random`` are not prefixed with ``cleat_``
            for attr in ("cleat_call", "cleat_sleep", "cleat_log",
                         "now", "random", "cleat_defer",
                         "poll_cancellation", "poll_signal"):
                assert hasattr(h, attr), f"HostCalls missing {attr}"
            return {"ok": True}

        packed = _call_export(my_func, {"data": "test"})
        err_code, _actual_len = memory.decode_export_result(packed)
        assert err_code == 0

    def test_cleat_entry_input_deserialization(self):
        """JSON input is correctly deserialized and passed as kwargs."""

        @cleat_entry
        def my_func(h: HostCalls, name: str, count: int):
            return {"name": name, "count": count}

        packed = _call_export(my_func, {"name": "Alice", "count": 42})
        err_code, output = _decode_output(packed)
        assert err_code == 0
        assert output == {"name": "Alice", "count": 42}

    def test_cleat_entry_no_params(self):
        """A function with only the ``h: HostCalls`` parameter works with
        empty or simple JSON input."""

        @cleat_entry
        def my_func(h: HostCalls):
            return {"status": "ok"}

        packed = _call_export(my_func, {})
        err_code, output = _decode_output(packed)
        assert err_code == 0
        assert output == {"status": "ok"}

    def test_cleat_entry_missing_param(self):
        """A missing required parameter produces ``err_code=1`` with an
        error message listing the missing keys."""

        @cleat_entry
        def my_func(h: HostCalls, name: str, count: int):
            return {"ok": True}

        packed = _call_export(my_func, {"name": "Alice"})  # missing "count"
        err_code, output = _decode_output(packed)
        assert err_code == 1
        assert "count" in output.get("error", "")

    def test_cleat_entry_extra_params(self):
        """Extra keys in the JSON input are silently ignored."""

        @cleat_entry
        def my_func(h: HostCalls, name: str):
            return {"name": name}

        packed = _call_export(my_func, {"name": "Bob", "extra": "ignored",
                                         "another": 123})
        err_code, output = _decode_output(packed)
        assert err_code == 0
        assert output == {"name": "Bob"}

    def test_cleat_entry_result_serialization(self):
        """The return value is JSON-serialized and written to the output
        buffer, including nested structures."""

        @cleat_entry
        def my_func(h: HostCalls):
            return {"result": "data", "nested": {"a": [1, 2, 3]}}

        packed = _call_export(my_func, {})
        err_code, output = _decode_output(packed)
        assert err_code == 0
        assert output == {"result": "data", "nested": {"a": [1, 2, 3]}}

    def test_cleat_entry_none_result(self):
        """Returning ``None`` produces the JSON literal ``null`` in the
        output buffer and err_code=0."""

        @cleat_entry
        def my_func(h: HostCalls):
            return None

        packed = _call_export(my_func, {})
        err_code, actual_len = memory.decode_export_result(packed)
        assert err_code == 0
        raw = memory.read_string(memory.OUTPUT_OFFSET, actual_len)
        assert raw == "null"

    def test_cleat_entry_string_result(self):
        """Returning a plain string produces a JSON quoted string."""

        @cleat_entry
        def my_func(h: HostCalls):
            return "plain string"

        packed = _call_export(my_func, {})
        err_code, actual_len = memory.decode_export_result(packed)
        assert err_code == 0
        raw = memory.read_string(memory.OUTPUT_OFFSET, actual_len)
        assert json.loads(raw) == "plain string"

    def test_cleat_entry_list_result(self):
        """Returning a list produces a JSON array."""

        @cleat_entry
        def my_func(h: HostCalls):
            return [1, 2, 3]

        packed = _call_export(my_func, {})
        err_code, output = _decode_output(packed)
        assert err_code == 0
        assert output == [1, 2, 3]

    def test_cleat_entry_error_handling(self):
        """When the wrapped function raises an exception the decorator
        returns ``err_code=1`` and a ``{"error": "..."}`` JSON in the
        output buffer."""

        @cleat_entry
        def my_func(h: HostCalls):
            raise RuntimeError("something went wrong")

        packed = _call_export(my_func, {})
        err_code, output = _decode_output(packed)
        assert err_code == 1
        assert "something went wrong" in output.get("error", "")

    def test_cleat_entry_type_error_handling(self):
        """A ``TypeError`` inside the wrapped function also yields
        ``err_code=1``."""

        @cleat_entry
        def my_func(h: HostCalls, name: str):
            return name + 1  # type error if name is a string

        packed = _call_export(my_func, {"name": "Alice"})
        err_code, actual_len = memory.decode_export_result(packed)
        assert err_code == 1
        raw = memory.read_string(memory.OUTPUT_OFFSET, actual_len)
        error_obj = json.loads(raw)
        assert "error" in error_obj

    def test_cleat_entry_large_output(self):
        """Output up to ``OUT_BUF_SIZE`` bytes is supported."""

        large_value = "x" * 40000

        @cleat_entry
        def my_func(h: HostCalls):
            return {"data": large_value}

        packed = _call_export(my_func, {})
        err_code, actual_len = memory.decode_export_result(packed)
        assert err_code == 0
        output_str = memory.read_string(memory.OUTPUT_OFFSET, actual_len)
        parsed = json.loads(output_str)
        assert parsed["data"] == large_value


# ---------------------------------------------------------------------------
# _unwrap_result helper (Result-like type unwrapping)
# ---------------------------------------------------------------------------


class TestUnwrapResult:
    """Tests for the ``_unwrap_result`` helper that unwraps Result-like
    objects before serialization."""

    def test_unwrap_result_plain_value(self):
        """Non-Result values pass through unchanged."""
        assert _unwrap_result("hello") == "hello"
        assert _unwrap_result(42) == 42
        assert _unwrap_result(None) is None
        assert _unwrap_result([1, 2, 3]) == [1, 2, 3]
        assert _unwrap_result({"key": "val"}) == {"key": "val"}

    def test_unwrap_result_ok(self):
        """A Result-like object with ``error=None`` returns the ``value``."""

        class OkResult:
            value = "success data"
            error = None

        assert _unwrap_result(OkResult()) == "success data"

    def test_unwrap_result_error(self):
        """A Result-like object with a non-None ``error`` returns an
        ``{"error": str(error)}`` dict."""

        class ErrResult:
            value = None
            error = Exception("something failed")

        result = _unwrap_result(ErrResult())
        assert result == {"error": "something failed"}

    def test_unwrap_result_partial_object(self):
        """An object with only ``value`` (no ``error``) is returned
        unchanged (does not match the Result duck-type)."""

        class OnlyValue:
            value = 42

        obj = OnlyValue()
        assert _unwrap_result(obj) is obj

    def test_unwrap_result_integration_with_decorator(self):
        """The flow: decorate, call, and verify that the Result-like
        return value is properly unwrapped before serialization."""

        class MyResult:
            def __init__(self, value, error=None):
                self.value = value
                self.error = error

        @cleat_entry
        def ok_workflow(h: HostCalls):
            return MyResult("success", error=None)

        packed = _call_export(ok_workflow, {})
        err_code, output = _decode_output(packed)
        assert err_code == 0
        assert output == "success"

        @cleat_entry
        def err_workflow(h: HostCalls):
            return MyResult(None, error=ValueError("bad input"))

        packed2 = _call_export(err_workflow, {})
        err_code2, output2 = _decode_output(packed2)
        assert err_code2 == 0
        assert output2 == {"error": "bad input"}

    def test_unwrap_result_integration_none_error_attribute(self):
        """A Result with ``error`` returning a string instead of None
        should still appear as an error."""

        class ResultStr:
            value = "partial"
            error = "not none"

        result = _unwrap_result(ResultStr())
        assert result == {"error": "not none"}
