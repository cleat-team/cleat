"""
Cleat SDK type definitions for workflow primitives.
"""

from dataclasses import dataclass
from typing import Any, Generic, Optional, TypeVar

T = TypeVar("T")


@dataclass
class SignalResult(Generic[T]):
    """Result of awaiting or polling a signal.

    Attributes:
        signal_name: The name of the signal that was received.
        value: The payload delivered with the signal, if any.
        timed_out: Whether the wait expired without receiving the signal.
    """

    signal_name: str
    value: Optional[T] = None
    timed_out: bool = False


@dataclass
class ChildResult(Generic[T]):
    """Result of a child workflow execution.

    Attributes:
        run_id: The unique identifier for the child workflow run.
        result: The return value of the child workflow, if completed.
        failed: Whether the child workflow failed.
        error_message: Error message if the child workflow failed.
    """

    run_id: str
    result: Optional[T] = None
    failed: bool = False
    error_message: Optional[str] = None


@dataclass
class PromiseResult(Generic[T]):
    """Result of awaiting a promise/awakeable.

    Attributes:
        value: The resolved value of the promise.
        resolved: Whether the promise was resolved before timeout.
        timed_out: Whether the wait expired without resolution.
    """

    value: Optional[T] = None
    resolved: bool = False
    timed_out: bool = False
