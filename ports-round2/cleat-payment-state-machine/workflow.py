"""
Payment state machine workflow ported from Restate's PaymentProcessor Virtual Object.

This implements the advanced payment lifecycle with:
- State machine transitions: NEW -> PROCESSING -> COMPLETED/CANCELLED
- Concurrent cancellation support (cancellation overtaking payment request)
- Compensation: cancelling a completed payment refunds the account
- Idempotent processing: payments are processed at most once
- Expiry: automatic state cleanup after a timeout

Original Restate patterns used:
- VirtualObject("PaymentProcessor") for singleton-per-key execution
- ctx.key() for the payment ID (virtual object key)
- ctx.get()/ctx.set() for key-scoped state
- ctx.object_call() to call the account Virtual Object
- ctx.object_send() with send_delay for delayed fire-and-forget
- ctx.clear_all() for state cleanup

Cleat equivalents:
- @cleat_entry for workflow entry points
- ctx.key() returning workflow_id (aliased for compatibility)
- ctx.get_state()/ctx.set_state() for state
- ctx.cleat_call() for service invocation
- ctx.cleat_sleep() + ctx.cleat_call() for delayed operations
- ctx.clear_all_state() for state cleanup
"""

from dataclasses import dataclass, asdict
from datetime import timedelta
from typing import Any, Dict, Optional

from accounts import (
    deposit,
    withdraw,
    DepositRequest,
    WithdrawRequest,
    Result,
    dataclass_to_dict,
)
from cleat_sdk import HostCalls, TerminalError, Saga, cleat_entry

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

# State keys (prefixed by payment_id for virtual-object-like isolation)
# ISSUE #1: Cleat has no Virtual Object / key-scoped state.
_PREFIX = "payment"
_STATUS_KEY = f"{_PREFIX}:status"
_PAYMENT_KEY = f"{_PREFIX}:payment"

# Expiry timeout - in production this would be something reasonable.
# Restate used timedelta(days=1). For testing we use a shorter duration.
EXPIRY_TIMEOUT_MS = 24 * 60 * 60 * 1000  # 24 hours in milliseconds

# State machine values
STATE_NEW = "NEW"
STATE_PROCESSING = "PROCESSING"
STATE_COMPLETED = "COMPLETED_SUCCESSFULLY"
STATE_CANCELLED = "CANCELED"

# ---------------------------------------------------------------------------
# Data models
# ---------------------------------------------------------------------------


@dataclass
class Payment:
    """A payment request to process."""
    account_id: str
    amount_cents: int


@dataclass
class PaymentRequest:
    """Incoming payment request."""
    payment_id: str
    account_id: str
    amount_cents: int


@dataclass
class CancelRequest:
    """Incoming cancellation request."""
    payment_id: str


@dataclass
class PaymentStatus:
    """The current status of a payment."""
    payment_id: str
    status: str
    payment: Optional[Payment] = None
    message: Optional[str] = None


# ---------------------------------------------------------------------------
# State helpers (manual key-scoping workaround)
# ---------------------------------------------------------------------------


def _payment_state_key(payment_id: str, field: str) -> str:
    """Build a namespaced state key for a payment.

    Restate Virtual Objects auto-scope state to the object key.
    Cleat does not, so we must prefix manually.
    """
    return f"payment:{payment_id}:{field}"


def _get_status(ctx: HostCalls, payment_id: str) -> str:
    """Get the current payment status, defaulting to NEW."""
    return ctx.get_state(_payment_state_key(payment_id, "status")) or STATE_NEW


def _set_status(ctx: HostCalls, payment_id: str, status: str) -> None:
    """Set the payment status."""
    ctx.set_state(_payment_state_key(payment_id, "status"), status)


def _get_payment(ctx: HostCalls, payment_id: str) -> Optional[Payment]:
    """Get the stored payment details."""
    raw = ctx.get_state(_payment_state_key(payment_id, "payment"))
    if isinstance(raw, dict):
        return Payment(**raw)
    return raw


def _set_payment(ctx: HostCalls, payment_id: str, payment: Payment) -> None:
    """Store the payment details."""
    ctx.set_state(_payment_state_key(payment_id, "payment"), asdict(payment))


# ---------------------------------------------------------------------------
# Durable entry points
# ---------------------------------------------------------------------------


@cleat_entry(name="PaymentProcessor.makePayment")
async def make_payment(ctx: HostCalls, request: PaymentRequest) -> str:
    """Process a payment through the state machine.

    Ported from:
        @payment_processor.handler("makePayment")
        async def make_payment(ctx: restate.ObjectContext, payment: Payment) -> str:

    State machine transitions:
        NEW -> PROCESSING -> COMPLETED_SUCCESSFULLY  (on success)
        CANCELED -> returns "already cancelled"
        COMPLETED_SUCCESSFULLY -> returns "already completed"

    Args:
        ctx: The cleat HostCalls context.
        request: Contains payment_id, account_id, amount_cents.

    Returns:
        A human-readable status message.
    """
    payment_id = request.payment_id
    status = _get_status(ctx, payment_id)

    # Idempotency check - if already done, return early
    if status == STATE_CANCELLED:
        return "Payment already cancelled"

    if status == STATE_COMPLETED:
        return "Payment already completed in prior call"

    # Transition to processing
    _set_status(ctx, payment_id, STATE_PROCESSING)

    # Build the payment details
    payment = Payment(
        account_id=request.account_id,
        amount_cents=request.amount_cents,
    )

    # --- Saga for compensating the withdrawal on failure ---
    # The Saga ensures that if something goes wrong after the withdrawal,
    # the funds are returned to the account.
    saga = Saga()

    saga.add_step(
        action=lambda: ctx.cleat_call(
            "account",
            "withdraw",
            dataclass_to_dict(
                WithdrawRequest(
                    account_id=payment.account_id,
                    amount_cents=payment.amount_cents,
                )
            ),
        ),
        compensate=lambda: ctx.cleat_call(
            "account",
            "deposit",
            dataclass_to_dict(
                DepositRequest(
                    account_id=payment.account_id,
                    amount_cents=payment.amount_cents,
                )
            ),
        ),
        name="withdraw_from_account",
    )

    try:
        result = await saga.execute()
    except TerminalError:
        # Terminal failure (e.g., negative amount) - saga compensated.
        _set_status(ctx, payment_id, STATE_CANCELLED)
        raise
    except Exception:
        # Non-terminal failure (e.g., network timeout) - saga compensated.
        _set_status(ctx, payment_id, STATE_CANCELLED)
        raise

    # Check withdrawal result
    # NOTE: In production, cleat_call returns the deserialized response.
    # Here result might be a dict or Result object depending on runtime.
    if isinstance(result, dict):
        withdrawal_ok = result.get("success", False)
        message = result.get("message", "")
    elif isinstance(result, Result):
        withdrawal_ok = result.success
        message = result.message
    else:
        withdrawal_ok = getattr(result, "success", False)
        message = getattr(result, "message", "Unknown result")

    if withdrawal_ok:
        # Record successful payment
        _set_status(ctx, payment_id, STATE_COMPLETED)
        _set_payment(ctx, payment_id, payment)

        # Schedule expiry (fire-and-forget with delay)
        # ISSUE #3: Cleat has no delayed fire-and-forget (object_send).
        _schedule_expiry(ctx, payment_id)
    else:
        # Withdrawal returned a business failure (e.g., insufficient funds).
        # Reset to NEW so the caller can retry with the same payment_id.
        # In the original Restate code, the status stays as whatever was last
        # set, which is effectively NEW since the original has no PROCESSING
        # state. We reset to NEW for correctness.
        _set_status(ctx, payment_id, STATE_NEW)

    return message


@cleat_entry(name="PaymentProcessor.cancelPayment")
async def cancel_payment(ctx: HostCalls, request: CancelRequest) -> None:
    """Cancel a payment.

    Ported from:
        @payment_processor.handler("cancelPayment")
        async def cancel_payment(ctx: restate.ObjectContext):

    Handles three scenarios:
    1. Payment is NEW -> mark as CANCELED (prevents future processing).
    2. Payment is already CANCELED -> no-op.
    3. Payment is COMPLETED_SUCCESSFULLY -> undo the charge and mark CANCELED.

    Args:
        ctx: The cleat HostCalls context.
        request: Contains payment_id.
    """
    payment_id = request.payment_id
    status = _get_status(ctx, payment_id)

    if status == STATE_NEW:
        # Cancellation arrived before the payment request.
        # Mark as cancelled so the payment won't go through.
        _set_status(ctx, payment_id, STATE_CANCELLED)
        # Schedule expiry for cleanup
        _schedule_expiry(ctx, payment_id)

    elif status == STATE_CANCELLED:
        pass  # Already cancelled

    elif status == STATE_COMPLETED:
        # Need to undo the completed payment
        payment = _get_payment(ctx, payment_id)
        if payment is None:
            raise TerminalError("No payment info found for cancellation")

        # Mark as cancelled first, then refund
        _set_status(ctx, payment_id, STATE_CANCELLED)

        # Refund the account by depositing the amount back
        # ISSUE #5: Cleat lacks Restate's object_send fire-and-forget.
        # We use cleat_call which waits for a response. For a refund
        # this is acceptable, but it changes the semantics from fire-and-forget.
        await ctx.cleat_call(
            "account",
            "deposit",
            dataclass_to_dict(
                DepositRequest(
                    account_id=payment.account_id,
                    amount_cents=payment.amount_cents,
                )
            ),
        )

    elif status == STATE_PROCESSING:
        # Payment is mid-flight. The original Restate example doesn't
        # handle this explicitly because Virtual Object serial execution
        # prevents concurrent access. Without Virtual Objects, concurrent
        # calls are possible, so we handle it.
        # ISSUE #6: No Virtual Object serial execution guarantee.
        _set_status(ctx, payment_id, STATE_CANCELLED)


@cleat_entry(name="PaymentProcessor.expire")
async def expire(ctx: HostCalls, request: Optional[dict] = None) -> None:
    """Clean up all state for a payment after expiry.

    Ported from:
        @payment_processor.handler()
        async def expire(ctx: restate.ObjectContext):

    Original used ctx.clear_all() which clears all state for the
    Virtual Object instance. In Cleat, we clear known keys.

    NOTE: In the original, this is called via object_send with send_delay.
    The payment_id is derived from the Virtual Object key.
    """
    payment_id = (request or {}).get("payment_id")
    if payment_id is None:
        return

    # ISSUE #7: Cleat has no clear_all(). We must clear keys individually.
    for field in ["status", "payment"]:
        ctx.clear_state(_payment_state_key(payment_id, field))


# ---------------------------------------------------------------------------
# Helper: Schedule expiry
# ---------------------------------------------------------------------------

# In-memory expiry schedule (development only)
# In production, the Cleat runtime would handle delayed execution.
_expiry_tasks: dict = {}


def _schedule_expiry(ctx: HostCalls, payment_id: str) -> None:
    """Schedule payment state cleanup after expiry timeout.

    ISSUE #3: Cleat lacks Restate's object_send with send_delay for
    fire-and-forget delayed execution. This is a workaround using
    asyncio.create_task to simulate the behavior in development.

    In production, the Cleat runtime should provide a durable timer
    mechanism that survives restarts.
    """
    import asyncio

    async def _delayed_expiry():
        try:
            await ctx.cleat_sleep(EXPIRY_TIMEOUT_MS)
            # After sleep, clean up state
            for field in ["status", "payment"]:
                ctx.clear_state(_payment_state_key(payment_id, field))
        except Exception:
            pass  # Expiry is best-effort cleanup

    task = asyncio.create_task(_delayed_expiry())
    _expiry_tasks[payment_id] = task
