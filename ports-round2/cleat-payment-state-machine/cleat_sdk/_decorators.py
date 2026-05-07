"""
Cleat durable_entry decorator for workflow entry points.

The @durable_entry decorator marks a function as a durable workflow entry point.
When decorated, the function can be triggered by the Cleat runtime and will
receive a HostCalls context as its first argument.
"""

import functools
import inspect
from typing import Any, Callable, Dict, Optional, TypeVar

F = TypeVar("F", bound=Callable[..., Any])


# Registry of all durable_entry functions, keyed by name
_DURABLE_ENTRY_REGISTRY: Dict[str, Callable[..., Any]] = {}


def get_registry() -> Dict[str, Callable[..., Any]]:
    """Return all registered durable entry points."""
    return dict(_DURABLE_ENTRY_REGISTRY)


def durable_entry(name: Optional[str] = None) -> Callable[[F], F]:
    """Decorator that marks a function as a durable workflow entry point.

    The decorated function will receive a HostCalls context object as its
    first argument, followed by any user-provided arguments.

    Args:
        name: Optional explicit name for this entry point. If not provided,
              the function name is used.

    Usage:
        @durable_entry(name="PaymentWorkflow")
        async def payment_workflow(ctx: HostCalls, request: dict) -> dict:
            ...
    """

    def decorator(func: F) -> F:
        entry_name = name or func.__name__

        @functools.wraps(func)
        async def wrapper(*args: Any, **kwargs: Any) -> Any:
            return await func(*args, **kwargs)

        wrapper._cleat_entry_name = entry_name  # type: ignore[attr-defined]
        wrapper._cleat_is_entry = True  # type: ignore[attr-defined]

        _DURABLE_ENTRY_REGISTRY[entry_name] = wrapper

        return wrapper  # type: ignore[return-value]

    return decorator
