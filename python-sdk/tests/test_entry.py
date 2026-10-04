"""Tests for the ``@cleat_entry`` decorator and its helpers.

These tests verify that the decorator correctly wraps workflow entry-point
functions, injects ``HostCalls``, handles JSON input/output serialization,
and properly reports errors.

WASM linear memory is mocked by setting ``memory._memory`` to a large
bytearray before each test.
"""

import json
from dataclasses import InitVar, dataclass, field

import pytest

try:
    from cleat_sdk import memory
    from cleat_sdk.entry import _from_dict, _unwrap_result, cleat_entry
    from cleat_sdk.host_calls import HostCalls
except ImportError as e:
    pytest.skip(
        f"Skipping entry tests: {e}.  entry.py and host_calls.py must exist.",
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
    """Call *fn* (the ``export_wrapper`` returned by ``@cleat_entry``)
    with the input dict serialized as JSON.

    Returns the result string from the wrapper (``str`` in the new
    string-returning ABI).
    """
    input_json = json.dumps(input_dict)
    return fn(input_json)


def _decode_output(result):
    """Decode the export wrapper's string result.

    With the new ``string -> string`` ABI there is no separate error code
    in the return value.  We simply parse the result as JSON and return
    ``(0, parsed)``.  Callers that expect an error check the ``"error"``
    key in the returned dict directly.
    """
    if not result:
        return 0, {}
    output = json.loads(result)
    return 0, output


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
            for attr in (
                "call",
                "sleep",
                "log",
                "now",
                "random",
                "defer",
                "poll_cancellation",
                "poll_signal",
            ):
                assert hasattr(h, attr), f"HostCalls missing {attr}"
            return {"ok": True}

        result = _call_export(my_func, {"data": "test"})
        parsed = json.loads(result)
        assert isinstance(parsed, dict) and "error" not in parsed

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
        """A missing required parameter produces an ``{"error": ...}`` result
        with a message listing the missing keys."""

        @cleat_entry
        def my_func(h: HostCalls, name: str, count: int):
            return {"ok": True}

        packed = _call_export(my_func, {"name": "Alice"})  # missing "count"
        _err_code, output = _decode_output(packed)
        assert "error" in output
        assert "count" in output.get("error", "")

    def test_cleat_entry_extra_params(self):
        """Extra keys in the JSON input are silently ignored."""

        @cleat_entry
        def my_func(h: HostCalls, name: str):
            return {"name": name}

        packed = _call_export(my_func, {"name": "Bob", "extra": "ignored", "another": 123})
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
        """Returning ``None`` produces the JSON literal ``null``."""

        @cleat_entry
        def my_func(h: HostCalls):
            return None

        result = _call_export(my_func, {})
        assert result == "null"

    def test_cleat_entry_string_result(self):
        """Returning a plain string produces a JSON quoted string."""

        @cleat_entry
        def my_func(h: HostCalls):
            return "plain string"

        result = _call_export(my_func, {})
        assert json.loads(result) == "plain string"

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
        returns ``{"error": "..."}`` JSON."""

        @cleat_entry
        def my_func(h: HostCalls):
            raise RuntimeError("something went wrong")

        packed = _call_export(my_func, {})
        _err_code, output = _decode_output(packed)
        assert isinstance(output, dict)
        assert "something went wrong" in output.get("error", "")

    def test_cleat_entry_type_error_handling(self):
        """A ``TypeError`` inside the wrapped function also yields
        an ``{"error": ...}`` result."""

        @cleat_entry
        def my_func(h: HostCalls, name: str):
            return name + 1  # type error if name is a string

        result = _call_export(my_func, {"name": "Alice"})
        error_obj = json.loads(result)
        assert "error" in error_obj

    def test_cleat_entry_large_output(self):
        """Output up to ``OUT_BUF_SIZE`` bytes is supported."""

        large_value = "x" * 40000

        @cleat_entry
        def my_func(h: HostCalls):
            return {"data": large_value}

        packed = _call_export(my_func, {})
        output_str = packed  # result is now a string directly
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


# ---------------------------------------------------------------------------
# The registry key, which is what the build reads
# ---------------------------------------------------------------------------


class TestTheRegistryKeyIsTheWorkflowName:
    """cleat#2976: every decorator form must register under a NAME.

    ``jsonschema_emitter._find_entry`` locates the entry by
    ``wrapper.__name__`` and RETURNS the registry key as the workflow name;
    ``json.dumps({workflow_name: ...})`` is what turns that key into the
    document. So the key is the address the build uses, and it has to be a name
    in every form the SDK documents -- a function object there is a TypeError at
    the emitter's ``main()``, not a lookup miss.

    THE TESTS ABOVE DO NOT COVER THIS AND COULD NOT HAVE. ``test_cleat_entry_basic``
    decorates with a bare ``@cleat_entry`` and asserts the wrapper runs
    end-to-end -- which it did, because the defect was never in what the wrapper
    does. It was in what the registry is keyed by, and only reading the registry
    can see that; the build's side of it is asserted in
    ``cmd/cleat/python_json_schema_emitter_test.go``.
    """

    @staticmethod
    def _registry():
        import sys

        return getattr(sys.modules[__name__], "_cleat_entry_wrappers", {})

    def test_a_bare_decorator_registers_under_the_functions_own_name(self):
        from cleat_sdk.entry import cleat_entry

        @cleat_entry
        def bare_named_workflow(h: HostCalls, x: str) -> str:
            return "{}"

        assert "bare_named_workflow" in self._registry(), (
            "the bare form did not register under the function's name; keys were "
            f"{list(self._registry())}"
        )

    def test_no_form_registers_under_a_function_object(self):
        """The regression itself, stated as a property rather than a case.

        Asserting one form's key would have missed the other two: the same
        branch was copy-pasted into ``cleat_entry``, ``virtual_object`` and
        ``query_handler``, so all three keyed the registry by a function object
        when used bare.
        """
        from cleat_sdk.entry import cleat_entry, query_handler, virtual_object

        @cleat_entry
        def bare_regression_entry(h: HostCalls, x: str) -> str:
            return "{}"

        @virtual_object
        def bare_regression_object(h: HostCalls, x: str) -> str:
            return "{}"

        @query_handler
        def bare_regression_query(h: HostCalls, x: str) -> str:
            return "{}"

        non_strings = [k for k in self._registry() if not isinstance(k, str)]
        assert non_strings == [], (
            "the registry is keyed by something that is not a name, so the schema "
            f"emitter -- which matches on the key -- finds no entry for it: {non_strings}"
        )

    def test_the_parenthesised_form_still_registers_under_its_argument(self):
        """The control: the fix must not have moved the explicit form's key."""
        from cleat_sdk.entry import cleat_entry

        @cleat_entry("ExplicitDifferentFromIdentifier")
        def paren_control(h: HostCalls, x: str) -> str:
            return "{}"

        assert "ExplicitDifferentFromIdentifier" in self._registry()
        assert "paren_control" not in self._registry(), (
            "the explicit name must win over the identifier, exactly as "
            "TestComputePythonEntryPointSchemaUsesTheDecoratorsOwnName asserts on the "
            "build side"
        )


class TestFromDictDataclassConversion:
    """``_from_dict``'s dataclass branch, focused on the source of the kwargs.

    The loop reads ``inspect.signature`` and not ``dataclasses.fields``,
    because fields() omits InitVar pseudo-fields and lists ``init=False``
    fields that ``__init__`` will not accept. The version that read fields()
    was wrong in both directions (cleat#2940):

    * an InitVar was invisible, so a dataclass with a REQUIRED one could not
      be built through this binding whatever the payload carried;
    * an ``init=False`` field was listed, so a payload value for one was
      passed as a keyword and crashed construction.
    """

    def test_a_required_initvar_is_supplied_and_reaches_the_initialiser(self):
        @dataclass
        class Order:
            sku: str
            seed: InitVar[int]

            def __post_init__(self, seed):
                self.seed_seen = seed

        built = _from_dict({"sku": "a", "seed": 3}, Order)
        assert (built.sku, built.seed_seen) == ("a", 3)

    def test_an_initvar_with_a_default_still_takes_it_when_the_payload_omits_it(self):
        @dataclass
        class Order:
            sku: str
            seed: InitVar[int] = 7

            def __post_init__(self, seed):
                self.seed_seen = seed

        assert _from_dict({"sku": "a"}, Order).seed_seen == 7

    def test_omitting_a_required_initvar_still_raises_and_names_it(self):
        """The case that keeps the fix honest: the payload really is
        incomplete, and no reflection can invent the value."""

        @dataclass
        class Order:
            sku: str
            seed: InitVar[int]

            def __post_init__(self, seed):
                pass

        with pytest.raises(
            TypeError, match="missing 1 required positional argument: 'seed'"
        ):
            _from_dict({"sku": "a"}, Order)

    def test_an_initvar_annotation_is_unwrapped_before_recursing(self):
        """``InitVar[X]`` is a WRAPPER, not a type -- recursing with it
        would convert against a non-type, so the inner type is what the
        payload has to satisfy."""

        @dataclass
        class Inner:
            n: int

        @dataclass
        class Order:
            sku: str
            seed: InitVar[Inner]

            def __post_init__(self, seed):
                self.seed_seen = seed

        built = _from_dict({"sku": "a", "seed": {"n": 3}}, Order)
        assert isinstance(built.seed_seen, Inner)
        assert built.seed_seen.n == 3

    def test_an_init_false_field_is_ignored_rather_than_passed_as_a_keyword(self):
        @dataclass
        class Order:
            sku: str
            total: int = field(init=False)

            def __post_init__(self):
                self.total = 0

        built = _from_dict({"sku": "a", "total": 99}, Order)
        assert built.sku == "a"
        # 0 from __post_init__, not 99 from the payload: the field is not a
        # constructor parameter, so the payload's value for it is ignored.
        assert built.total == 0
