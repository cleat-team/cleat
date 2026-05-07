"""
Cleat Saga orchestration for compensating transactions.

The Saga pattern coordinates distributed transactions with compensating
actions that undo partial work when a step fails. This is essential for
workflows that span multiple services.
"""

import asyncio
from dataclasses import dataclass, field
from typing import Any, Awaitable, Callable, List, Optional


@dataclass
class SagaStep:
    """A single step in a saga with its compensating action."""

    action: Callable[[], Awaitable[Any]]
    compensate: Callable[[], Awaitable[Any]]
    name: Optional[str] = None


class Saga:
    """Orchestrates a sequence of actions with compensating undo logic.

    Usage:
        saga = Saga()
        saga.add_step(
            action=lambda: charge_card(amount),
            compensate=lambda: refund_card(amount),
            name="charge_card",
        )
        saga.add_step(
            action=lambda: book_flight(flight_id),
            compensate=lambda: cancel_flight(flight_id),
            name="book_flight",
        )
        result = await saga.execute()
        # If any step fails, all previously completed steps are
        # compensated in reverse order.
    """

    def __init__(self):
        self._steps: List[SagaStep] = []
        self._completed: List[int] = []  # indices of completed steps

    def add_step(
        self,
        action: Callable[[], Awaitable[Any]],
        compensate: Callable[[], Awaitable[Any]],
        name: Optional[str] = None,
    ) -> "Saga":
        """Register a step and its compensation.

        Args:
            action: An async callable that performs the forward action.
            compensate: An async callable that undoes the forward action.
            name: Optional human-readable name for logging/debugging.

        Returns:
            Self for chaining.
        """
        self._steps.append(SagaStep(action=action, compensate=compensate, name=name))
        return self

    async def execute(self) -> Any:
        """Execute all steps in order, compensating on failure.

        Returns:
            The result of the last successful step.

        Raises:
            The original exception on failure, after compensating
            all previously completed steps.
        """
        last_result = None
        for i, step in enumerate(self._steps):
            try:
                last_result = await step.action()
                self._completed.append(i)
            except Exception:
                # Compensate in reverse order
                await self._compensate_all()
                raise

        return last_result

    async def _compensate_all(self) -> None:
        """Execute all registered compensations in reverse order."""
        for i in reversed(self._completed):
            step = self._steps[i]
            try:
                await step.compensate()
            except Exception as e:
                # Log but don't suppress - compensation failures are
                # logged so the overall saga failure is still raised
                import logging

                logging.getLogger(__name__).error(
                    "Saga compensation failed for step %s (%s): %s",
                    i,
                    step.name or "unnamed",
                    e,
                )
