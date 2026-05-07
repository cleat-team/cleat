"""
Type sugar wrappers for the Cleat durable execution engine.

Provides typed convenience wrappers --- parallel to the Go SDK's typed
helpers --- for child workflows, sagas, and deferred cleanup.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, Callable, Generic, Optional, TypeVar

from .host_calls import HostCalls

T = TypeVar("T")


# ---------------------------------------------------------------------------
# Child workflow
# ---------------------------------------------------------------------------

@dataclass
class ChildWorkflow(Generic[T]):
    """A typed handle for starting and awaiting a child workflow.

    Usage::

        child = ChildWorkflow[str](
            name="notify_user",
            input={"user_id": "u-42", "message": "Order shipped"},
        )
        run_id = child.start(h)
        # ... do other work ...
        result = child.await_result(h)
    """

    name: str
    """Child workflow definition name (matches an ``@durable_entry``)."""

    input: Any
    """Input payload --- will be JSON-serialised before sending to the host."""

    run_id: Optional[str] = None
    """Run ID returned by the host after ``start()``."""

    def start(self, h: HostCalls) -> str:
        """Start the child workflow on the host.

        Parameters
        ----------
        h:
            HostCalls instance for the current execution context.

        Returns
        -------
        str
            The run ID assigned by the host.
        """
        input_json = json.dumps(self.input, default=str)
        self.run_id = h.durable_child_workflow(self.name, input_json)
        return self.run_id

    def await_result(self, h: HostCalls) -> T:
        """Block (suspend if needed) until the child workflow completes.

        Parameters
        ----------
        h:
            HostCalls instance for the current execution context.

        Returns
        -------
        T
            Deserialised result of the child workflow.
        """
        if self.run_id is None:
            raise RuntimeError(
                "ChildWorkflow.await_result() called before start()"
            )
        result_json = h.durable_await_child(self.run_id)
        result: T = json.loads(result_json)  # type: ignore[assignment]
        return result


# ---------------------------------------------------------------------------
# Saga
# ---------------------------------------------------------------------------

@dataclass
class SagaStep:
    """A single step in a saga, paired with its compensating action.

    Attributes
    ----------
    name:
        Human-readable label for the step (used in error messages).
    action:
        Callable that performs the forward work.
    compensate:
        Callable that undoes the forward work.
    """

    name: str
    action: Callable[..., Any]
    compensate: Callable[..., Any]


class Saga:
    """Orchestrates a sequence of steps with automatic compensation on failure.

    If any step raises an exception, all previously completed steps are
    compensated **in reverse order**.  Compensations are best-effort ---
    if a compensation itself fails, its exception is swallowed and logged
    so that earlier compensations still have a chance to run.

    Usage::

        saga = Saga(h)
        saga.add_step(
            name="reserve_inventory",
            action=lambda: h.durable_call("inventory", "Reserve", reserve_json),
            compensate=lambda: h.durable_call("inventory", "Release", release_json),
        )
        saga.add_step(
            name="process_payment",
            action=lambda: h.durable_call("payments", "Charge", charge_json),
            compensate=lambda: h.durable_call("payments", "Refund", refund_json),
        )
        results = saga.execute()
    """

    def __init__(self, h: HostCalls) -> None:
        self._h = h
        self._steps: list[SagaStep] = []

    def add_step(
        self,
        name: str,
        action: Callable[..., Any],
        compensate: Callable[..., Any],
    ) -> None:
        """Register a saga step.

        Parameters
        ----------
        name:
            Human-readable label (e.g. ``"reserve_inventory"``).
        action:
            Forward action callable.
        compensate:
            Compensating action callable that undoes *action*.
        """
        self._steps.append(SagaStep(name=name, action=action, compensate=compensate))

    def execute(self) -> list[Any]:
        """Execute all steps in order, compensating on failure.

        Returns
        -------
        list[Any]
            Results of each step, in order.  The type of each element
            depends on the return type of the corresponding ``action``.

        Raises
        ------
        Exception
            Re-raises the exception from the failing step after
            compensations have been attempted.
        """
        results: list[Any] = []
        completed: list[SagaStep] = []

        for step in self._steps:
            try:
                result = step.action()
                results.append(result)
                completed.append(step)
            except Exception:
                # Compensate completed steps in reverse order.
                _compensate_all(self._h, completed)
                raise

        return results


def _compensate_all(h: HostCalls, completed: list[SagaStep]) -> None:
    """Run compensations for *completed* steps in reverse order.

    Failures during compensation are logged via ``h.durable_log`` but do
    not prevent later compensations from running (best-effort).
    """
    for step in reversed(completed):
        try:
            step.compensate()
        except Exception as exc:
            try:
                h.durable_log(
                    f"Saga compensation failed for step '{step.name}': {exc}"
                )
            except Exception:
                pass  # Nothing we can do if even logging fails.


# ---------------------------------------------------------------------------
# Durable defer
# ---------------------------------------------------------------------------

@dataclass
class DurableDefer:
    """Context manager for registering deferred cleanup with the host.

    The ``durable_defer`` host call registers a cleanup description that
    the host will execute when the workflow exits (normally or abnormally).
    This context manager provides a convenient ``with``-statement wrapper.

    Usage::

        with DurableDefer("release inventory reservation", h):
            # ... workflow logic ...
            pass   # ``durable_defer`` registered on entry
    """

    description: str
    """Human-readable description of the cleanup action."""

    _h: Optional[HostCalls] = field(default=None, repr=False)
    """HostCalls instance (set via the constructor or ``__enter__``)."""

    _defer_id: Optional[str] = field(default=None, repr=False)
    """Defer ID returned by the host, if applicable."""

    def __enter__(self) -> DurableDefer:
        """Register the defer with the host on context entry."""
        if self._h is not None:
            self._defer_id = self._h.durable_defer(self.description)
        return self

    def __exit__(
        self,
        exc_type: Optional[type[BaseException]],
        exc_val: Optional[BaseException],
        exc_tb: Optional[object],
    ) -> Optional[bool]:
        """No synchronous cleanup needed; host manages the defer lifecycle."""
        return None
