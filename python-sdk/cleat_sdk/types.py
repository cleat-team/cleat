"""
Type sugar wrappers for the Cleat durable execution engine.

Provides typed convenience wrappers --- parallel to the Go SDK's typed
helpers --- for child workflows, sagas, and deferred cleanup.
"""

from __future__ import annotations

import json
import sys
from dataclasses import dataclass, field
from typing import Any, Callable, Generic, Optional, TypeVar

from .host_calls import HostCalls

T = TypeVar("T")

# ---------------------------------------------------------------------------


class TerminalError(Exception):
    """Exception indicating a non-retryable (terminal) error.

    When raised from a saga step, compensation is triggered immediately.
    Transient errors (plain ``Exception`` subclasses) do NOT trigger
    compensation, allowing the caller to retry the entire saga.
    """

    pass


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
    """Child workflow definition name (matches an ``@cleat_entry``)."""

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
        self.run_id = h.child_workflow(self.name, input_json)
        return self.run_id

    def run(self, h: HostCalls) -> T:
        """Convenience method that starts the child workflow and immediately
        awaits the result.  Combines :meth:`start` and :meth:`await_result`
        in a single call.

        Parameters
        ----------
        h:
            HostCalls instance for the current execution context.

        Returns
        -------
        T
            Deserialised result of the child workflow.
        """
        self.start(h)
        return self.await_result(h)

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
            raise RuntimeError("ChildWorkflow.await_result() called before start()")
        result_json = h.await_child(self.run_id)
        result: T = json.loads(result_json)  # type: ignore[assignment]
        return result


# ---------------------------------------------------------------------------
# Saga
# ---------------------------------------------------------------------------


SagaResultT = TypeVar("SagaResultT")


class SagaStep(Generic[T]):
    """A single step in a saga, paired with its compensating action.

    Generic parameter ``T`` is the return type of the step's action.

    Subclass this and override :meth:`action` and :meth:`compensate` for
    a non-closure-based alternative to the lambda API.  The methods
    receive a :class:`HostCalls` instance, avoiding closure capture
    across WASM suspend/resume boundaries.

    Attributes
    ----------
    name:
        Human-readable label for the step (used in error messages).
    """

    def __init__(self, name: str) -> None:
        self.name = name

    def action(self, h: HostCalls) -> T:
        """Perform the forward work for this step.

        Override in subclasses.

        Parameters
        ----------
        h : HostCalls
            HostCalls instance for the current execution context.

        Returns
        -------
        T
            The result of the forward action. The type parameter of the
            :class:`SagaStep` determines the expected return type.
        """
        raise NotImplementedError("SagaStep subclasses must implement action()")

    def compensate(self, h: HostCalls) -> None:
        """Undo the forward work for this step.

        Override in subclasses.

        Parameters
        ----------
        h : HostCalls
            HostCalls instance for the current execution context.
        """
        raise NotImplementedError("SagaStep subclasses must implement compensate()")


@dataclass
class SagaStepResult(Generic[T]):
    """Result of a single saga step execution.

    Attributes
    ----------
    step_name:
        Human-readable label for the step.
    success:
        ``True`` if the step completed without error.
    result:
        The return value of the step's action (only meaningful when
        *success* is ``True``).
    error:
        Error message if the step failed, or ``None`` on success.
    """

    step_name: str
    success: bool
    result: Optional[T] = None
    error: Optional[str] = None


class Saga(Generic[SagaResultT]):
    """Orchestrates a sequence of steps with automatic compensation on failure.

    Generic parameter ``SagaResultT`` is the return type of each step's action.
    ``execute()`` returns ``list[SagaResultT]``.

    If any step raises a :class:`TerminalError` (or any exception type in
    ``terminal_exceptions``), all previously completed steps are compensated
    **in reverse order**.  Plain exceptions (transient errors) do NOT
    trigger compensation, allowing the caller to retry.

    Compensations are best-effort --- if a compensation itself fails, its
    exception is swallowed and logged so that earlier compensations still
    have a chance to run.

    Usage with lambda closures (convenient but captures ``h`` in scope)::

        saga = Saga[str](h)
        saga.add_step(
            name="reserve_inventory",
            action=lambda: h.call("inventory", "Reserve", reserve_json),
            compensate=lambda: h.call("inventory", "Release", release_json),
        )

    Usage with explicit HostCalls (no closure capture)::

        saga = Saga[str](h)
        saga.add_step_fn(
            name="reserve_inventory",
            action=lambda h: h.call("inventory", "Reserve", reserve_json),
            compensate=lambda h: h.call("inventory", "Release", release_json),
        )

    Usage with subclassed steps (cleanest for WASM)::

        class ReserveStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                return h.call("inventory", "Reserve", reserve_json)
            def compensate(self, h: HostCalls) -> None:
                h.call("inventory", "Release", release_json)

        saga = Saga[str](h)
        saga.add_step(ReserveStep("reserve_inventory"))
        results: list[str] = saga.execute()
    """

    def __init__(
        self,
        h: HostCalls,
        terminal_exceptions: Optional[tuple[type[BaseException], ...]] = None,
    ) -> None:
        self._h = h
        self._steps: list[SagaStep[Any]] = []
        self._terminal_exceptions: tuple[type[BaseException], ...] = terminal_exceptions or ()

    def add_step(
        self,
        step_or_name: SagaStep[SagaResultT] | str,
        action: Optional[Callable[..., SagaResultT]] = None,
        compensate: Optional[Callable[..., Any]] = None,
    ) -> None:
        """Register a saga step.

        This method can be called in three ways:

        1. With a :class:`SagaStep` instance (preferred for WASM)::

            saga.add_step(ReserveStep("reserve_inventory"))

        2. With ``name``, ``action``, and ``compensate`` callables that do
           NOT receive ``h`` (closure-based, convenient)::

            saga.add_step(
                name="reserve_inventory",
                action=lambda: call_service(),
                compensate=lambda: undo_service(),
            )

        Parameters
        ----------
        step_or_name :
            A :class:`SagaStep` instance, or a step name string.
        action :
            Forward action callable (only when *step_or_name* is a string).
        compensate :
            Compensating action callable (only when *step_or_name* is a string).
        """
        if isinstance(step_or_name, SagaStep):
            self._steps.append(step_or_name)
        else:
            self._steps.append(
                _LambdaSagaStep(
                    name=step_or_name,
                    action=action,
                    compensate=compensate,
                )
            )

    def add_step_fn(
        self,
        name: str,
        action: Callable[[HostCalls], SagaResultT],
        compensate: Optional[Callable[[HostCalls], None]] = None,
    ) -> None:
        """Register a saga step using callables that receive ``HostCalls``.

        Unlike :meth:`add_step`, the callables are invoked with the
        current :class:`HostCalls` instance, avoiding closure capture
        of ``h`` across WASM suspend/resume boundaries.

        Parameters
        ----------
        name :
            Human-readable label (e.g. ``"reserve_inventory"``).
        action :
            Forward action callable that receives ``HostCalls``.
        compensate :
            Optional compensating action callable that receives
            ``HostCalls``.  If ``None``, no compensation is performed
            for this step.
        """
        self._steps.append(_FnSagaStep(name=name, action=action, compensate=compensate))

    def execute(
        self,
        terminal_exceptions: Optional[tuple[type[BaseException], ...]] = None,
    ) -> list[SagaStepResult]:
        """Execute all steps in order, compensating on terminal failure.

        Parameters
        ----------
        terminal_exceptions :
            Additional exception types that should trigger compensation.
            Merged with exceptions set in the constructor.
            :class:`TerminalError` is always treated as terminal.

        Returns
        -------
        list[SagaStepResult]
            Results of each step, in order, each containing the step name,
            success flag, result value, and optional error message.

        Raises
        ------
        Exception
            Re-raises the exception from the failing step.  If the step
            failed with a terminal exception, compensations are run first.
            Only terminal exceptions trigger compensation --- transient
            errors are re-raised without compensation.
        """
        if terminal_exceptions is None:
            terminal_exceptions = ()
        all_terminal = self._terminal_exceptions + terminal_exceptions

        results: list[SagaStepResult] = []
        completed: list[SagaStep[Any]] = []

        for step in self._steps:
            try:
                result = step.action(self._h)
                results.append(
                    SagaStepResult(
                        step_name=step.name,
                        success=True,
                        result=result,
                        error=None,
                    )
                )
                completed.append(step)
            except all_terminal + (TerminalError,) as exc:
                # Terminal error: compensate and re-raise.
                results.append(
                    SagaStepResult(
                        step_name=step.name,
                        success=False,
                        result=None,
                        error=str(exc),
                    )
                )
                _compensate_all(self._h, completed)
                raise
            except Exception as exc:
                # Transient error: add result and re-raise without
                # compensation so the caller can retry the saga.
                results.append(
                    SagaStepResult(
                        step_name=step.name,
                        success=False,
                        result=None,
                        error=str(exc),
                    )
                )
                raise

        return results


# ---------------------------------------------------------------------------
# Internal step wrappers
# ---------------------------------------------------------------------------


class _LambdaSagaStep(SagaStep):
    """Internal wrapper for the closure-based ``add_step(name, action, compensate)`` API."""

    def __init__(
        self,
        name: str,
        action: Optional[Callable[..., Any]],
        compensate: Optional[Callable[..., Any]],
    ) -> None:
        super().__init__(name)
        self._action = action
        self._compensate = compensate

    def action(self, h: HostCalls) -> Any:
        if self._action is None:
            return None
        return self._action()

    def compensate(self, h: HostCalls) -> None:
        if self._compensate is not None:
            self._compensate()


class _FnSagaStep(SagaStep):
    """Internal wrapper for the ``add_step_fn`` API (callables receive ``HostCalls``)."""

    def __init__(
        self,
        name: str,
        action: Callable[[HostCalls], Any],
        compensate: Optional[Callable[[HostCalls], None]],
    ) -> None:
        super().__init__(name)
        self._action = action
        self._compensate = compensate

    def action(self, h: HostCalls) -> Any:
        return self._action(h)

    def compensate(self, h: HostCalls) -> None:
        if self._compensate is not None:
            self._compensate(h)


def _compensate_all(h: HostCalls, completed: list[SagaStep[Any]]) -> None:
    """Run compensations for *completed* steps in reverse order.

    Failures during compensation are logged via ``h.log`` but do
    not prevent later compensations from running (best-effort).

    If ``log`` is not available (e.g. the workflow context has been
    torn down), falls back to writing errors to ``sys.stderr``.
    """
    for step in reversed(completed):
        try:
            step.compensate(h)
        except Exception as exc:
            try:
                h.log(f"Saga compensation failed for step '{step.name}': {exc}")
            except Exception:
                # Fall back to stderr when log is unavailable
                # (e.g., workflow context already torn down).
                msg = f"Saga compensation failed for step '{step.name}': {exc}\n"
                sys.stderr.write(msg)


# ---------------------------------------------------------------------------
# Cleat defer
# ---------------------------------------------------------------------------


@dataclass
class CleatDefer:
    """Context manager for registering deferred cleanup with the host.

    The ``defer`` host call registers a cleanup description that
    the host will execute when the workflow exits (normally or abnormally).
    This context manager provides a convenient ``with``-statement wrapper.

    Usage::

        with CleatDefer("release inventory reservation", h):
            # ... workflow logic ...
            pass   # ``defer`` registered on entry
    """

    description: str
    """Human-readable description of the cleanup action."""

    _h: Optional[HostCalls] = field(default=None, repr=False)
    """HostCalls instance (set via the constructor or ``__enter__``)."""

    _defer_id: Optional[str] = field(default=None, repr=False)
    """Defer ID returned by the host, if applicable."""

    def __enter__(self) -> CleatDefer:
        """Register the defer with the host on context entry."""
        if self._h is not None:
            self._defer_id = self._h.defer(self.description)
        return self

    def __exit__(
        self,
        exc_type: Optional[type[BaseException]],
        exc_val: Optional[BaseException],
        exc_tb: Optional[object],
    ) -> Optional[bool]:
        """No synchronous cleanup needed; host manages the defer lifecycle."""
        return None
