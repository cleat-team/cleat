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

from .host_calls import HostCalls, SuspendSentinel, RetryPolicy, ChildWorkflowOptions
from .host_calls import SignalResult, ChildResult, PromiseResult
from .host_calls import (
    CleatCallError,
    CleatCallTransientError,
    CleatCallPermanentError,
    CleatCallTimeoutError,
    INFINITE_TIMEOUT_MS,
)
from .entry import cleat_entry, virtual_object
from .types import ChildWorkflow, Saga, SagaStep, SagaStepResult, TerminalError, CleatDefer
from .client import CleatClient
from .test_harness import CleatTestHarness, CallRecord
from .local_host import LocalHostCalls
from .plugins import (
    Plugins,
    BlobPutResult,
    BlobGetResult,
    AwaitEventResult,
    EvaluateFlagResult,
    ProduceResult,
    SendWebhookResult,
    StreamEvent,
    TriggerIncidentResult,
    ResolveIncidentResult,
    SendMessageResult,
    AwaitWebhookResult,
    LLMChatResult,
    LLMEmbedResult,
    LLMListModelsResult,
    PgVectorSearchResult,
    PgVectorUpsertResult,
    PgVectorDeleteResult,
)
from .langchain.callbacks import CleatCallbackHandler
from .langgraph.checkpoint import CleatCheckpointer

__all__ = [
    "HostCalls",
    "LocalHostCalls",
    "SuspendSentinel",
    "RetryPolicy",
    "SignalResult",
    "ChildResult",
    "ChildWorkflowOptions",
    "PromiseResult",
    "CleatCallError",
    "CleatCallTransientError",
    "CleatCallPermanentError",
    "CleatCallTimeoutError",
    "INFINITE_TIMEOUT_MS",
    "cleat_entry",
    "virtual_object",
    "ChildWorkflow",
    "Saga",
    "SagaStep",
    "SagaStepResult",
    "TerminalError",
    "CleatDefer",
    "CleatClient",
    "CleatTestHarness",
    "CallRecord",
    "Plugins",
    "BlobPutResult",
    "BlobGetResult",
    "AwaitEventResult",
    "EvaluateFlagResult",
    "ProduceResult",
    "SendWebhookResult",
    "StreamEvent",
    "TriggerIncidentResult",
    "ResolveIncidentResult",
    "SendMessageResult",
    "AwaitWebhookResult",
    "LLMChatResult",
    "LLMEmbedResult",
    "LLMListModelsResult",
    "PgVectorSearchResult",
    "PgVectorUpsertResult",
    "PgVectorDeleteResult",
    "CleatCallbackHandler",
    "CleatCheckpointer",
]
