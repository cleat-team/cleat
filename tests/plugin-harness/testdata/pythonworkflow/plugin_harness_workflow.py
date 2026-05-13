"""Plugin harness workflow — calls every plugin host function.

This workflow exercises every host function exposed by every cleat plugin
from a Python workflow.  It is the Python equivalent of the Go, Rust, and
AssemblyScript workflows in the same testdata directory.

Usage (when the Python WASM FFI is ready):
    cleat build --target python --entry plugin_harness_workflow.py:call_all_plugins
    cleat run call_all_plugins '{}'
"""

import json

from cleat_sdk import HostCalls, cleat_entry


def _call(results: dict, key: str, h: HostCalls, plugin: str, func: str, input_obj) -> None:
    """Call a single plugin function and record the result."""
    try:
        raw = h.plugin_call(plugin, func, input_obj)
        results[key] = json.loads(raw)
    except Exception as exc:
        results[key] = {"error": str(exc)}


@cleat_entry("CallAllPlugins")
def call_all_plugins(h: HostCalls, _input: str = "{}") -> str:
    """Call every plugin host function and return a JSON summary."""

    results: dict = {}

    # -- blobstore -----------------------------------------------------------
    _call(results, "blobstore.put", h, "blobstore", "put",
          {"key": "test-key", "data": "aGVsbG8="})

    _call(results, "blobstore.get", h, "blobstore", "get",
          {"key": "test-key"})

    # -- event-triggers ------------------------------------------------------
    _call(results, "event-triggers.await_event", h, "event-triggers", "await_event",
          {"event_type": "test.event", "timeout_ms": 100})

    # -- feature-flags -------------------------------------------------------
    _call(results, "feature-flags.evaluate_flag", h, "feature-flags", "evaluate_flag",
          {"key": "test-flag", "context": {"user_id": "test-user"}})

    # -- kafka-connect -------------------------------------------------------
    _call(results, "kafka-connect.produce", h, "kafka-connect", "produce",
          {"config_id": "00000000-0000-0000-0000-000000000001",
           "value": "test-message",
           "key": "test-key"})

    # -- notifications -------------------------------------------------------
    _call(results, "notifications.send_webhook", h, "notifications", "send_webhook",
          {"webhook_id": "00000000-0000-0000-0000-000000000002",
           "event_type": "test.event",
           "payload": {"message": "hello"}})

    # -- pagerduty-alert -----------------------------------------------------
    _call(results, "pagerduty-alert.trigger_incident", h, "pagerduty-alert", "trigger_incident",
          {"config_id": "00000000-0000-0000-0000-000000000003",
           "summary": "Test incident",
           "severity": "critical",
           "source": "test-harness"})

    _call(results, "pagerduty-alert.resolve_incident", h, "pagerduty-alert", "resolve_incident",
          {"config_id": "00000000-0000-0000-0000-000000000003",
           "incident_key": "mock-key"})

    # -- pgvector ------------------------------------------------------------
    _call(results, "pgvector.upsert", h, "pgvector", "upsert",
          {"collection": "test-collection",
           "external_id": "test-1",
           "content": "test content",
           "embedding": [0.1, 0.2, 0.3, 0.4]})

    _call(results, "pgvector.search", h, "pgvector", "search",
          {"collection": "test-collection",
           "query_vector": [0.1, 0.2, 0.3, 0.4],
           "top_k": 5})

    _call(results, "pgvector.delete", h, "pgvector", "delete",
          {"collection": "test-collection",
           "external_id": "test-1"})

    # -- slack-notify --------------------------------------------------------
    _call(results, "slack-notify.send_message", h, "slack-notify", "send_message",
          {"config_id": "00000000-0000-0000-0000-000000000004",
           "text": "Test message from plugin harness",
           "channel": "#test"})

    # -- webhook-ingest ------------------------------------------------------
    _call(results, "webhook-ingest.await_webhook", h, "webhook-ingest", "await_webhook",
          {"source_id": "test-source"})

    # -- llm -----------------------------------------------------------------
    _call(results, "llm.chat", h, "llm", "chat",
          {"provider": "openai",
           "model": "mock-model",
           "messages": [{"role": "user", "content": "hello"}]})

    _call(results, "llm.embed", h, "llm", "embed",
          {"provider": "openai",
           "model": "mock-model",
           "input": ["test text"]})

    _call(results, "llm.list_models", h, "llm", "list_models",
          {"provider": "openai"})

    # -- llm chat_stream (streaming) -----------------------------------------
    try:
        events = list(h.plugin_call_streaming(
            "llm", "chat_stream",
            {"provider": "openai",
             "model": "mock-model",
             "messages": [{"role": "user", "content": "hello"}]},
        ))
        results["llm.chat_stream"] = events
    except Exception as exc:
        results["llm.chat_stream"] = {"error": str(exc)}

    return json.dumps(results)
