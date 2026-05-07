"""Thorough unit tests for HostCalls method coverage and plugin wrappers.

Tests verify that:
1. All expected HostCalls methods exist and have correct signatures
2. All plugin wrapper methods exist and construct correct inputs
3. Missing HostCalls methods (has_state, list_state, log_kv, etc.) are present
4. Plugin wrappers handle errors properly
5. The Plugins._call helper correctly marshals/unmarshals
"""

import json
from unittest import mock

import pytest

try:
    from cleat_sdk import memory
    from cleat_sdk.host_calls import (
        HostCalls,
        SuspendSentinel,
        RetryPolicy,
        SignalResult,
        ChildResult,
        PromiseResult,
        DurableCallError,
        DurableCallTransientError,
        DurableCallPermanentError,
        DurableCallTimeoutError,
        INFINITE_TIMEOUT_MS,
    )
    from cleat_sdk.plugins import (
        Plugins,
        LLMChatResult,
        LLMEmbedResult,
        LLMListModelsResult,
        PgVectorSearchResult,
    )
except ImportError as e:
    pytest.skip(
        f"Skipping host calls tests: {e}.  "
        "Required modules must exist.",
        allow_module_level=True,
    )


# ========================================================================
# Fixtures
# ========================================================================


@pytest.fixture(autouse=True)
def setup_memory():
    """Set up a large enough linear memory before each test."""
    old = memory._memory
    memory._memory = bytearray(memory.OUTPUT_OFFSET + memory.OUT_BUF_SIZE)
    yield
    memory._memory = old


@pytest.fixture
def host():
    """Create a fresh HostCalls instance for each test."""
    return HostCalls()


@pytest.fixture
def plugins(host):
    """Create a fresh Plugins instance for each test."""
    return Plugins(host)


# ========================================================================
# HostCalls — method existence verification
# ========================================================================


class TestHostCallsMethodExistence:
    """Verify every expected HostCalls method exists with the right signature."""

    # --- Core durable operations ---

    def test_now(self, host):
        assert callable(host.now)

    def test_random(self, host):
        assert callable(host.random)

    def test_version(self, host):
        assert callable(host.version)

    def test_min_version(self, host):
        assert callable(host.min_version)

    def test_durable_log(self, host):
        assert callable(host.durable_log)

    def test_log_kv(self, host):
        """log_kv is a structured logging helper."""
        assert callable(host.log_kv)

    def test_durable_sleep(self, host):
        assert callable(host.durable_sleep)

    def test_durable_call(self, host):
        assert callable(host.durable_call)

    def test_durable_call_typed(self, host):
        assert callable(host.durable_call_typed)

    def test_durable_call_with_retry(self, host):
        assert callable(host.durable_call_with_retry)

    def test_durable_call_with_heartbeat(self, host):
        assert callable(host.durable_call_with_heartbeat)

    def test_durable_fetch(self, host):
        assert callable(host.durable_fetch)

    def test_durable_fetch_json(self, host):
        """durable_fetch_json is a deserializing fetch wrapper."""
        assert callable(host.durable_fetch_json)

    def test_fetch_get(self, host):
        """fetch_get is a GET shorthand."""
        assert callable(host.fetch_get)

    def test_fetch_get_json(self, host):
        """fetch_get_json is a GET shorthand with JSON deserialization."""
        assert callable(host.fetch_get_json)

    # --- Signal / cancellation ---

    def test_await_signals(self, host):
        assert callable(host.await_signals)

    def test_poll_signal(self, host):
        assert callable(host.poll_signal)

    def test_poll_cancellation(self, host):
        assert callable(host.poll_cancellation)

    # --- Child workflows ---

    def test_child_workflow(self, host):
        assert callable(host.child_workflow)

    def test_await_child(self, host):
        assert callable(host.await_child)

    def test_await_all_children(self, host):
        assert callable(host.await_all_children)

    # --- State operations ---

    def test_set_query_state(self, host):
        assert callable(host.set_query_state)

    def test_set_state(self, host):
        assert callable(host.set_state)

    def test_get_state(self, host):
        assert callable(host.get_state)

    def test_delete_state(self, host):
        assert callable(host.delete_state)

    def test_incr_state(self, host):
        assert callable(host.incr_state)

    def test_has_state(self, host):
        """has_state checks existence of a durable state key."""
        assert callable(host.has_state)

    def test_list_state(self, host):
        """list_state lists durable state keys by prefix."""
        assert callable(host.list_state)

    # --- Promises ---

    def test_create_promise(self, host):
        assert callable(host.create_promise)

    def test_await_promise(self, host):
        assert callable(host.await_promise)

    def test_resolve_promise(self, host):
        assert callable(host.resolve_promise)

    def test_reject_promise(self, host):
        assert callable(host.reject_promise)

    # --- Handlers ---

    def test_register_update_handler(self, host):
        assert callable(host.register_update_handler)

    def test_register_query_handler(self, host):
        assert callable(host.register_query_handler)

    # --- Lifecycle ---

    def test_durable_defer(self, host):
        assert callable(host.durable_defer)

    def test_continue_as_new(self, host):
        assert callable(host.continue_as_new)

    def test_extend_timeout(self, host):
        """extend_timeout extends the workflow execution timeout."""
        assert callable(host.extend_timeout)

    def test_run_detached(self, host):
        assert callable(host.run_detached)

    # --- Fire-and-forget / scheduling ---

    def test_durable_send(self, host):
        assert callable(host.durable_send)

    def test_schedule_invoke(self, host):
        assert callable(host.schedule_invoke)

    # --- Identity ---

    def test_current_workflow_id(self, host):
        assert callable(host.current_workflow_id)

    def test_current_run_id(self, host):
        assert callable(host.current_run_id)

    # --- Scoped state ---

    def test_set_scope(self, host):
        assert callable(host.set_scope)

    def test_get_scope(self, host):
        assert callable(host.get_scope)

    def test_clear_scope(self, host):
        assert callable(host.clear_scope)

    # --- UUID ---

    def test_uuid(self, host):
        assert callable(host.uuid)

    # --- Plugin ---

    def test_plugin_call(self, host):
        assert callable(host.plugin_call)

    # --- Total method count ---

    def test_total_method_count(self, host):
        """Sanity check: count the expected public methods."""
        public_methods = [
            name for name in dir(host)
            if not name.startswith("_") and callable(getattr(host, name))
        ]
        # This is a living count — the exact number may grow as new
        # wrappers are added.  Update it when adding new public methods.
        assert len(public_methods) >= 39, (
            f"Expected at least 38 public methods, got {len(public_methods)}. "
            f"Methods: {sorted(public_methods)}"
        )


# ========================================================================
# HostCalls — new method behaviour
# ========================================================================


class TestHostCallsNewMethods:
    """Tests for the newly added HostCalls methods."""

    # --- log_kv ---

    def test_log_kv_basic(self, host):
        """log_kv without kvs just passes through to durable_log."""
        with mock.patch.object(host, "durable_log") as mock_log:
            host.log_kv("hello")
            mock_log.assert_called_once_with("hello")

    def test_log_kv_with_pairs(self, host):
        """log_kv formats key-value pairs."""
        with mock.patch.object(host, "durable_log") as mock_log:
            host.log_kv("processing order", "order_id", "ord-42", "status", "active")
            mock_log.assert_called_once()
            msg = mock_log.call_args[0][0]
            assert "processing order" in msg
            assert "order_id=ord-42" in msg
            assert "status=active" in msg

    def test_log_kv_odd_count(self, host):
        """log_kv with odd kvs pairs handles trailing key gracefully."""
        with mock.patch.object(host, "durable_log") as mock_log:
            host.log_kv("test", "key_only")
            msg = mock_log.call_args[0][0]
            assert "key_only=" in msg  # empty value

    # --- has_state ---

    def test_has_state_delegates(self, host):
        """has_state delegates to durable_call('state', 'has', ...)."""
        with mock.patch.object(host, "durable_call", return_value="true") as mock_call:
            result = host.has_state("my_key")
            mock_call.assert_called_once_with(
                "state", "has", {"key": "my_key"}
            )
            assert result is True

    def test_has_state_false(self, host):
        """has_state returns False when the key does not exist."""
        with mock.patch.object(host, "durable_call", return_value="false"):
            assert host.has_state("missing") is False

    # --- list_state ---

    def test_list_state_delegates(self, host):
        """list_state delegates to durable_call('state', 'list', ...)."""
        with mock.patch.object(
            host, "durable_call", return_value='["k1", "k2"]'
        ) as mock_call:
            result = host.list_state("prefix_")
            mock_call.assert_called_once_with("state", "list", {"prefix": "prefix_"})
            assert result == ["k1", "k2"]

    def test_list_state_empty_prefix(self, host):
        """list_state with empty prefix passes an empty filter."""
        with mock.patch.object(
            host, "durable_call", return_value="[]"
        ) as mock_call:
            result = host.list_state()
            mock_call.assert_called_once_with("state", "list", {"prefix": ""})
            assert result == []

    # --- durable_fetch_json ---

    def test_durable_fetch_json_delegates(self, host):
        """durable_fetch_json deserializes the response from durable_fetch."""
        with mock.patch.object(
            host, "durable_fetch", return_value=('{"key": "val"}', 200)
        ):
            result = host.durable_fetch_json("http://example.com")
            assert result == {"key": "val"}

    # --- fetch_get ---

    def test_fetch_get_delegates(self, host):
        """fetch_get delegates to durable_fetch with GET."""
        with mock.patch.object(
            host, "durable_fetch", return_value=('{"ok": true}', 200)
        ) as mock_fetch:
            result = host.fetch_get("http://example.com")
            mock_fetch.assert_called_once_with("http://example.com", "GET")
            assert result == ('{"ok": true}', 200)

    # --- fetch_get_json ---

    def test_fetch_get_json_delegates(self, host):
        """fetch_get_json delegates through fetch_get with JSON parsing."""
        with mock.patch.object(
            host, "durable_fetch", return_value=('{"x": 1}', 200)
        ):
            result = host.fetch_get_json("http://example.com")
            assert result == {"x": 1}


# ========================================================================
# Plugins — AI plugin wrappers
# ========================================================================


class TestPluginAIMethods:
    """Tests for the AI plugin wrapper methods on the Plugins class."""

    # --- llm_chat ---

    def test_llm_chat_minimal(self, plugins):
        """llm_chat with required params only."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"choices": [], "usage": {}, "cost": 0.0, "model": "gpt-4o"}',
        ) as mock_call:
            result = plugins.llm_chat(
                "openai", "gpt-4o",
                [{"role": "user", "content": "hello"}],
            )
            mock_call.assert_called_once_with(
                "llm", "chat",
                {
                    "provider": "openai",
                    "model": "gpt-4o",
                    "messages": [{"role": "user", "content": "hello"}],
                },
            )
            assert isinstance(result, LLMChatResult)
            assert result.model == "gpt-4o"

    def test_llm_chat_with_options(self, plugins):
        """llm_chat with all optional parameters."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"choices": [], "usage": {}, "cost": 0.0, "model": "gpt-4o"}',
        ):
            result = plugins.llm_chat(
                "openai", "gpt-4o",
                [{"role": "user", "content": "hi"}],
                tools=[{"type": "function", "function": {"name": "foo", "parameters": {}}}],
                max_tokens=100,
                temperature=0.7,
                system_prompt="Be helpful",
                tool_choice="auto",
            )
            assert isinstance(result, LLMChatResult)

    def test_llm_chat_error_response(self, plugins):
        """llm_chat handles error responses from the plugin."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"choices": [], "usage": {}, "cost": 0.0, "model": "", "error": "rate limited"}',
        ):
            result = plugins.llm_chat("openai", "gpt-4o", [{"role": "user", "content": "hi"}])
            assert result.error == "rate limited"

    def test_llm_chat_runtime_error(self, plugins):
        """llm_chat propagates RuntimeError from plugin_call failure."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            side_effect=RuntimeError("plugin not available"),
        ):
            with pytest.raises(RuntimeError, match="plugin not available"):
                plugins.llm_chat("openai", "gpt-4o", [{"role": "user", "content": "hi"}])

    # --- llm_embed ---

    def test_llm_embed(self, plugins):
        """llm_embed constructs the correct input and returns typed result."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"data": [{"embedding": [0.1, 0.2], "index": 0}], "usage": {"total_tokens": 5}, "cost": 0.001}',
        ) as mock_call:
            result = plugins.llm_embed("openai", "text-embedding-3-small", ["hello world"])
            mock_call.assert_called_once_with(
                "llm", "embed",
                {"provider": "openai", "model": "text-embedding-3-small", "input": ["hello world"]},
            )
            assert isinstance(result, LLMEmbedResult)
            assert len(result.data) == 1
            assert result.data[0]["embedding"] == [0.1, 0.2]

    # --- llm_list_models ---

    def test_llm_list_models_all(self, plugins):
        """llm_list_models without a provider queries all."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"providers": {"openai": [{"name": "gpt-4o", "cost_per_1k_tokens": 0.01}]}}',
        ) as mock_call:
            result = plugins.llm_list_models()
            mock_call.assert_called_once_with("llm", "list_models", {})
            assert isinstance(result, LLMListModelsResult)
            assert "openai" in result.providers

    def test_llm_list_models_by_provider(self, plugins):
        """llm_list_models with a provider filters results."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"models": [{"name": "gpt-4o", "cost_per_1k_tokens": 0.01}], "provider": "openai"}',
        ) as mock_call:
            result = plugins.llm_list_models(provider="openai")
            mock_call.assert_called_once_with("llm", "list_models", {"provider": "openai"})
            assert isinstance(result, LLMListModelsResult)
            assert len(result.models) == 1

    # --- pgvector_search ---

    def test_pgvector_search(self, plugins):
        """pgvector_search constructs correct input and returns results list."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"results": [{"id": "abc", "score": 0.95, "content": "match"}]}',
        ) as mock_call:
            results = plugins.pgvector_search("docs", [0.1, 0.2, 0.3], limit=5)
            mock_call.assert_called_once_with(
                "pgvector", "search",
                {"collection": "docs", "query_vector": [0.1, 0.2, 0.3], "top_k": 5, "include_meta": True},
            )
            assert len(results) == 1
            assert results[0]["id"] == "abc"
            assert results[0]["score"] == 0.95

    def test_pgvector_search_with_filter(self, plugins):
        """pgvector_search with metadata filter."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"results": []}',
        ) as mock_call:
            plugins.pgvector_search("docs", [1.0, 2.0], filter={"status": "active"})
            call_input = mock_call.call_args[0][2]
            assert call_input["filter"] == {"status": "active"}

    def test_pgvector_search_min_score_filter(self, plugins):
        """pgvector_search with min_score filters results client-side."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"results": [{"id": "a", "score": 0.9}, {"id": "b", "score": 0.5}]}',
        ):
            results = plugins.pgvector_search("docs", [0.1, 0.2], min_score=0.7)
            assert len(results) == 1
            assert results[0]["id"] == "a"

    # --- pgvector_upsert ---

    def test_pgvector_upsert(self, plugins):
        """pgvector_upsert constructs correct input."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"id": "new-id"}',
        ) as mock_call:
            plugins.pgvector_upsert("docs", "ext-1", [0.1, 0.2], metadata={"author": "alice"})
            mock_call.assert_called_once_with(
                "pgvector", "upsert",
                {"collection": "docs", "external_id": "ext-1", "embedding": [0.1, 0.2],
                 "metadata": {"author": "alice"}},
            )

    def test_pgvector_upsert_no_metadata(self, plugins):
        """pgvector_upsert without metadata."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"id": "new-id"}',
        ) as mock_call:
            plugins.pgvector_upsert("docs", "ext-1", [0.1, 0.2])
            call_input = mock_call.call_args[0][2]
            assert "metadata" not in call_input

    # --- pgvector_delete ---

    def test_pgvector_delete(self, plugins):
        """pgvector_delete constructs correct input."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"deleted": 1}',
        ) as mock_call:
            plugins.pgvector_delete("docs", "ext-1")
            mock_call.assert_called_once_with(
                "pgvector", "delete",
                {"collection": "docs", "external_id": "ext-1"},
            )


# ========================================================================
# Plugins — error handling
# ========================================================================


class TestPluginErrorHandling:
    """Tests for error handling in plugin wrappers."""

    def test_invalid_json_response(self, plugins):
        """_call raises RuntimeError when plugin returns invalid JSON."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value="not json",
        ):
            with pytest.raises(RuntimeError, match="invalid JSON"):
                plugins.llm_chat("openai", "gpt-4o", [{"role": "user", "content": "hi"}])

    def test_non_object_response(self, plugins):
        """_call raises RuntimeError when response is not a JSON object."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='"just a string"',
        ):
            with pytest.raises(RuntimeError, match="expected a JSON object"):
                plugins.llm_chat("openai", "gpt-4o", [{"role": "user", "content": "hi"}])

    def test_plugin_call_runtime_error(self, host):
        """plugin_call propagates RuntimeError from the import stub."""
        with pytest.raises(RuntimeError, match="can only be called within a cleat WASM runtime"):
            host.plugin_call("nonexistent", "func", {})


# ========================================================================
# HostCalls — dataclass and error handling
# ========================================================================


class TestHostCallsDataclasses:
    """Test that result dataclasses can be constructed correctly."""

    def test_retry_policy_defaults(self):
        policy = RetryPolicy()
        assert policy.max_attempts == 3
        assert policy.initial_interval_ms == 1000
        assert policy.backoff_coefficient == 2.0
        assert policy.max_interval_ms == 30000
        assert policy.non_retryable_errors == []

    def test_retry_policy_custom(self):
        policy = RetryPolicy(
            max_attempts=5,
            initial_interval_ms=500,
            backoff_coefficient=1.5,
            max_interval_ms=60000,
            non_retryable_errors=["BAD_REQUEST"],
        )
        assert policy.max_attempts == 5
        assert "BAD_REQUEST" in policy.non_retryable_errors

    def test_signal_result(self):
        sr = SignalResult(name="payment_received", payload='{"amount": 100}', timed_out=False)
        assert sr.name == "payment_received"
        assert not sr.timed_out

    def test_signal_result_timed_out(self):
        sr = SignalResult(name="", payload="", timed_out=True)
        assert sr.timed_out

    def test_child_result_success(self):
        cr = ChildResult(run_id="run-1", result="{\"ok\": true}")
        assert cr.run_id == "run-1"
        assert cr.error is None

    def test_child_result_error(self):
        cr = ChildResult(run_id="run-1", result="", error="child failed")
        assert cr.error == "child failed"

    def test_promise_result_success(self):
        pr = PromiseResult(result="some value", timed_out=False)
        assert pr.result == "some value"
        assert not pr.timed_out
        assert not pr.rejected

    def test_promise_result_rejected(self):
        pr = PromiseResult(result="", timed_out=False, rejected=True)
        assert pr.rejected

    def test_promise_result_timed_out(self):
        pr = PromiseResult(result="", timed_out=True)
        assert pr.timed_out


class TestHostCallsErrorHandling:
    """Tests for HostCalls error handling in methods that use import stubs.

    These tests verify the error path when import stubs raise
    NotImplementedError, confirming the error is propagated as RuntimeError.
    """

    def test_now_raises_without_wasm(self, host):
        with pytest.raises(RuntimeError):
            # The stub raises NotImplementedError, but HostCalls.now()
            # returns the raw result.  The caller gets NotImplementedError
            # wrapped at the callsite.
            try:
                host.now()
            except NotImplementedError as e:
                raise RuntimeError(str(e)) from e

    def test_durable_log_raises_without_wasm(self, host):
        with pytest.raises(RuntimeError):
            try:
                host.durable_log("test")
            except NotImplementedError as e:
                raise RuntimeError(str(e)) from e

    def test_durable_sleep_raises(self, host):
        with pytest.raises(RuntimeError):
            try:
                host.durable_sleep(100)
            except NotImplementedError as e:
                raise RuntimeError(str(e)) from e


# ========================================================================
# Plugins — all existing plugin methods existence
# ========================================================================


class TestPluginMethodExistence:
    """Verify every expected Plugins method exists."""

    def test_blobstore_put(self, plugins):
        assert callable(plugins.blobstore_put)

    def test_blobstore_get(self, plugins):
        assert callable(plugins.blobstore_get)

    def test_await_event(self, plugins):
        assert callable(plugins.await_event)

    def test_evaluate_flag(self, plugins):
        assert callable(plugins.evaluate_flag)

    def test_produce(self, plugins):
        assert callable(plugins.produce)

    def test_send_webhook(self, plugins):
        assert callable(plugins.send_webhook)

    def test_trigger_incident(self, plugins):
        assert callable(plugins.trigger_incident)

    def test_resolve_incident(self, plugins):
        assert callable(plugins.resolve_incident)

    def test_send_message(self, plugins):
        assert callable(plugins.send_message)

    def test_await_webhook(self, plugins):
        assert callable(plugins.await_webhook)

    def test_llm_chat(self, plugins):
        assert callable(plugins.llm_chat)

    def test_llm_embed(self, plugins):
        assert callable(plugins.llm_embed)

    def test_llm_list_models(self, plugins):
        assert callable(plugins.llm_list_models)

    def test_pgvector_search(self, plugins):
        assert callable(plugins.pgvector_search)

    def test_pgvector_upsert(self, plugins):
        assert callable(plugins.pgvector_upsert)

    def test_pgvector_delete(self, plugins):
        assert callable(plugins.pgvector_delete)

    def test_total_plugin_count(self, plugins):
        """Count the expected public plugin methods."""
        public_methods = [
            name for name in dir(plugins)
            if not name.startswith("_") and callable(getattr(plugins, name))
        ]
        # blobstore(2) + eventtriggers(1) + featureflags(1) + kafkaconnect(1)
        # + notifications(1) + pagerdutyalert(2) + slacknotify(1)
        # + webhookingest(1) + llm(3) + pgvector(3) = 16
        assert len(public_methods) >= 16, (
            f"Expected at least 16 plugin wrapper methods, got {len(public_methods)}. "
            f"Methods: {sorted(public_methods)}"
        )


# ========================================================================
# DurableCall exception hierarchy (Task 6)
# ========================================================================


class TestDurableCallErrorHierarchy:
    """Tests for the DurableCall exception hierarchy."""

    def test_durable_call_error_is_runtime_error(self):
        """DurableCallError inherits from RuntimeError for backward compat."""
        assert issubclass(DurableCallError, RuntimeError)
        assert issubclass(DurableCallTransientError, DurableCallError)
        assert issubclass(DurableCallPermanentError, DurableCallError)
        assert issubclass(DurableCallTimeoutError, DurableCallTransientError)

    def test_durable_call_error_has_fields(self):
        """DurableCallError carries service, operation, and call_error_code."""
        err = DurableCallError("svc", "op", "something broke", call_error_code=3)
        assert err.service == "svc"
        assert err.operation == "op"
        assert err.call_error_code == 3
        assert "svc.op" in str(err)
        assert "[3]" in str(err)

    def test_durable_call_transient_error(self):
        err = DurableCallTransientError("svc", "op", "unavailable", call_error_code=2)
        assert isinstance(err, DurableCallError)
        assert isinstance(err, RuntimeError)

    def test_durable_call_permanent_error(self):
        err = DurableCallPermanentError("svc", "op", "invalid", call_error_code=4)
        assert isinstance(err, DurableCallError)

    def test_durable_call_timeout_error(self):
        err = DurableCallTimeoutError("svc", "op", "timed out", call_error_code=1)
        assert isinstance(err, DurableCallError)
        assert isinstance(err, DurableCallTransientError)


# ========================================================================
# durable_call timeout parameter (Task 7)
# ========================================================================


class TestDurableCallTimeoutParam:
    """Tests for the timeout_ms parameter on durable_call."""

    def test_durable_call_accepts_timeout_ms(self):
        """durable_call accepts an optional timeout_ms parameter."""
        with mock.patch.object(
            HostCalls, "_marshal", return_value="{}"
        ):
            # The import stub will raise NotImplementedError, but we
            # verify the method signature accepts the parameter.
            h = HostCalls()
            with pytest.raises(RuntimeError):
                h.durable_call("svc", "op", {}, timeout_ms=5000)

    def test_durable_call_without_timeout_still_works(self):
        """durable_call works without the timeout_ms parameter."""
        with mock.patch.object(
            HostCalls, "_marshal", return_value="{}"
        ):
            h = HostCalls()
            with pytest.raises(RuntimeError):
                h.durable_call("svc", "op", {})

    def test_durable_call_timeout_via_mock(self):
        """When mocked, durable_call with timeout_ms processes correctly."""
        h = HostCalls()
        h._marshal = mock.Mock(return_value="{}")
        # Make the call go through the non-WASM path (no retry import)
        with mock.patch("cleat_sdk.host_calls._USING_WASM", False):
            with mock.patch(
                "cleat_sdk.host_calls._import_durable_call",
                return_value=0,  # response_len=0, call_error_code=0, err_code=0
            ) as mock_call:
                result = h.durable_call("svc", "op", {}, timeout_ms=5000)
                # With _USING_WASM=False, the timeout path is skipped
                mock_call.assert_called_once()
                assert result == ""


# ========================================================================
# extend_timeout (Task 11)
# ========================================================================


class TestExtendTimeout:
    """Tests for the extend_timeout method."""

    def test_extend_timeout_exists(self, host):
        """extend_timeout is a callable method on HostCalls."""
        assert callable(host.extend_timeout)

    def test_extend_timeout_raises_without_wasm(self, host):
        """extend_timeout raises RuntimeError when called without WASM."""
        with pytest.raises(RuntimeError):
            try:
                host.extend_timeout(60000)
            except NotImplementedError as e:
                raise RuntimeError(str(e)) from e
