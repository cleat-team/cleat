"""
Cleat Python SDK

Provides durable execution primitives for building reliable workflows.
This is a reference SDK for the cleat platform.
"""

from cleat_sdk._host_calls import HostCalls, register_local_handler, clear_local_handlers
from cleat_sdk._decorators import durable_entry
from cleat_sdk._saga import Saga
from cleat_sdk._exceptions import TerminalError
from cleat_sdk._types import SignalResult, ChildResult, PromiseResult

__all__ = [
    "HostCalls",
    "durable_entry",
    "Saga",
    "TerminalError",
    "SignalResult",
    "ChildResult",
    "PromiseResult",
    "register_local_handler",
    "clear_local_handlers",
]
