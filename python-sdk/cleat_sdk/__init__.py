"""
cleat-sdk: Python SDK for the Cleat durable execution framework.

Provides the HostCalls class for making durable API calls from WASM workflows,
the @durable_entry decorator for marking workflow entry points, and memory
helpers for the Cleat ABI.

Quick start:
    from cleat_sdk import HostCalls, durable_entry

    @durable_entry
    def my_workflow(h: HostCalls, name: str) -> str:
        h.durable_log(f"Hello, {name}!")
        resp = h.durable_call("my-service", "DoThing", {"name": name})
        return resp
"""

from .host_calls import HostCalls, SuspendSentinel, RetryPolicy
from .host_calls import SignalResult, ChildResult, PromiseResult
from .entry import durable_entry, virtual_object
from .types import ChildWorkflow, Saga, SagaStep, TerminalError, DurableDefer
from .client import CleatClient
from .plugins import (
    Plugins,
    BlobPutResult,
    BlobGetResult,
    AwaitEventResult,
    EvaluateFlagResult,
    ProduceResult,
    SendWebhookResult,
    TriggerIncidentResult,
    ResolveIncidentResult,
    SendMessageResult,
    AwaitWebhookResult,
)

__all__ = [
    "HostCalls",
    "SuspendSentinel",
    "RetryPolicy",
    "SignalResult",
    "ChildResult",
    "PromiseResult",
    "durable_entry",
    "virtual_object",
    "ChildWorkflow",
    "Saga",
    "SagaStep",
    "TerminalError",
    "DurableDefer",
    "CleatClient",
    "Plugins",
    "BlobPutResult",
    "BlobGetResult",
    "AwaitEventResult",
    "EvaluateFlagResult",
    "ProduceResult",
    "SendWebhookResult",
    "TriggerIncidentResult",
    "ResolveIncidentResult",
    "SendMessageResult",
    "AwaitWebhookResult",
]
