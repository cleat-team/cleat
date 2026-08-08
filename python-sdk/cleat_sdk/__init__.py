"""
cleat-sdk: Python SDK for the Cleat cleat execution framework.

Provides the HostCalls class for making cleat API calls from WASM workflows,
the @cleat_entry decorator for marking workflow entry points, and memory
helpers for the Cleat ABI.

Quick start:
    from cleat_sdk import HostCalls, cleat_entry

    @cleat_entry
    def my_workflow(h: HostCalls, name: str) -> str:
        h.log(f"Hello, {name}!")
        resp = h.call("my-service", "DoThing", {"name": name})
        return resp
"""

from .client import CleatClient
from .entry import cleat_entry, virtual_object
from .host_calls import (
    INFINITE_TIMEOUT_MS,
    ChildResult,
    ChildWorkflowOptions,
    CleatCallError,
    CleatCallPermanentError,
    CleatCallTimeoutError,
    CleatCallTransientError,
    HostCalls,
    PromiseResult,
    RetryPolicy,
    SignalResult,
    SuspendSentinel,
)
from .langchain.callbacks import CleatCallbackHandler
from .langgraph.checkpoint import CleatCheckpointer
from .local_host import LocalHostCalls
from .plugins import (
    AwaitEventResult,
    AwaitWebhookResult,
    BlobGetResult,
    BlobPutResult,
    EvaluateFlagResult,
    LLMChatResult,
    LLMEmbedResult,
    LLMListModelsResult,
    PgVectorDeleteResult,
    PgVectorSearchResult,
    PgVectorUpsertResult,
    Plugins,
    ProduceResult,
    ResolveIncidentResult,
    SendMessageResult,
    SendWebhookResult,
    StreamEvent,
    TriggerIncidentResult,
)
from .test_harness import CallRecord, CleatTestHarness
from .types import ChildWorkflow, CleatDefer, Saga, SagaStep, SagaStepResult, TerminalError

__all__ = [
    "INFINITE_TIMEOUT_MS",
    "AwaitEventResult",
    "AwaitWebhookResult",
    "BlobGetResult",
    "BlobPutResult",
    "CallRecord",
    "ChildResult",
    "ChildWorkflow",
    "ChildWorkflowOptions",
    "CleatCallError",
    "CleatCallPermanentError",
    "CleatCallTimeoutError",
    "CleatCallTransientError",
    "CleatCallbackHandler",
    "CleatCheckpointer",
    "CleatClient",
    "CleatDefer",
    "CleatTestHarness",
    "EvaluateFlagResult",
    "HostCalls",
    "LLMChatResult",
    "LLMEmbedResult",
    "LLMListModelsResult",
    "LocalHostCalls",
    "PgVectorDeleteResult",
    "PgVectorSearchResult",
    "PgVectorUpsertResult",
    "Plugins",
    "ProduceResult",
    "PromiseResult",
    "ResolveIncidentResult",
    "RetryPolicy",
    "Saga",
    "SagaStep",
    "SagaStepResult",
    "SendMessageResult",
    "SendWebhookResult",
    "SignalResult",
    "StreamEvent",
    "SuspendSentinel",
    "TerminalError",
    "TriggerIncidentResult",
    "cleat_entry",
    "virtual_object",
]
