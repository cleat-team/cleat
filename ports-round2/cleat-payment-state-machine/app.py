"""
Main application entry point for the Cleat Payment State Machine.

This is ported from the Restate version at:
    examples/python/patterns-use-cases/statemachinepayments/app.py

The original used:
    app = restate.app([payment_processor, account])
    hypercorn.asyncio.serve(app, conf)

In Cleat, there is no built-in HTTP server. The cleat runtime provides
the execution environment. This file serves as both documentation and
a development harness.

Services contract:
    The following cleat_entry points are registered and need to be
    exposed by the Cleat runtime:
      - account.deposit
      - account.withdraw
      - PaymentProcessor.makePayment
      - PaymentProcessor.cancelPayment
      - PaymentProcessor.expire
"""

import asyncio
import logging
import uuid
from typing import Optional

from cleat_sdk import (
    HostCalls,
    cleat_entry,
    register_local_handler,
    clear_local_handlers,
)
from cleat_sdk._decorators import get_registry

# Import the workflow modules to ensure their
# @cleat_entry decorators are executed
import accounts  # noqa: F401
import workflow  # noqa: F401

logging.basicConfig(
    level=logging.INFO,
    format="[%(asctime)s] [%(process)d] [%(levelname)s] - %(message)s",
)
logger = logging.getLogger(__name__)


def list_entry_points() -> list[str]:
    """List all registered durable entry points."""
    return list(get_registry().keys())


# ---------------------------------------------------------------------------
# Local handler registration for development/testing
# ---------------------------------------------------------------------------
# In production, cleat_call dispatches through the Cleat runtime.
# For development, we register local handlers so the example can run
# without a runtime connection.
# See ISSUES.md #4 for details on this workaround.


def _register_local_service_handlers() -> None:
    """Register local handlers so cleat_call works without a runtime.

    This wraps the @cleat_entry functions as handlers compatible
    with the HostCalls.cleat_call dispatch path.

    The account service handlers (deposit/withdraw) accept typed request
    objects (DepositRequest/WithdrawRequest) but cleat_call passes raw
    dicts. We deserialize them here.
    """
    from accounts import deposit, withdraw, DepositRequest, WithdrawRequest, Result
    from workflow import make_payment, cancel_payment, PaymentRequest, CancelRequest

    # Map (service, operation) -> async handler(ctx, request_dict)

    async def _deposit_handler(ctx: HostCalls, request: dict) -> None:
        req = DepositRequest(**request)
        await deposit(ctx, req)

    async def _withdraw_handler(ctx: HostCalls, request: dict) -> dict:
        req = WithdrawRequest(**request)
        result: Result = await withdraw(ctx, req)
        return {"success": result.success, "message": result.message}

    register_local_handler("account", "deposit", _deposit_handler)
    register_local_handler("account", "withdraw", _withdraw_handler)

    logger.info("Registered local service handlers for development mode")


# ---------------------------------------------------------------------------
# Development harness
# ---------------------------------------------------------------------------
# In the original Restate example, this module was served via hypercorn.
# In Cleat, the runtime manages execution. This is a simple harness to
# demonstrate workflow invocation for testing.


async def simulate_payment_workflow(
    payment_id: str,
    account_id: str,
    amount_cents: int,
    cancel: bool = False,
) -> None:
    """Simulate the payment state machine workflow for testing.

    This bypasses the Cleat runtime and directly calls the
    cleat_entry functions with a HostCalls context.

    In production, the Cleat runtime would:
    1. Receive an HTTP request
    2. Create a HostCalls context
    3. Invoke the registered cleat_entry function
    4. Manage state persistence and recovery
    """
    from workflow import make_payment, cancel_payment, PaymentRequest, CancelRequest

    # Create a context for the payment workflow
    ctx = HostCalls(workflow_id=payment_id)

    # Step 1: Make the payment
    result = await make_payment(
        ctx,
        PaymentRequest(
            payment_id=payment_id,
            account_id=account_id,
            amount_cents=amount_cents,
        ),
    )
    logger.info("Payment result: %s", result)

    # Step 2 (optional): Cancel the payment
    if cancel:
        cancel_ctx = HostCalls(workflow_id=payment_id)
        await cancel_payment(cancel_ctx, CancelRequest(payment_id=payment_id))
        logger.info("Payment cancelled: %s", payment_id)


async def main() -> None:
    """Development harness - demonstrates the workflow."""
    # Register local handlers so cleat_call works in dev mode
    _register_local_service_handlers()

    logger.info("Cleat Payment State Machine")
    logger.info("Registered entry points: %s", list_entry_points())

    # --- Scenario 1: Successful payment ---
    payment_id = f"payment-{uuid.uuid4().hex[:8]}"
    account_id = f"acct-{uuid.uuid4().hex[:8]}"

    logger.info("\n=== Scenario 1: Successful Payment ===")
    logger.info("  Payment ID: %s", payment_id)
    logger.info("  Account ID: %s", account_id)
    logger.info("  Amount: 5000 cents")

    await simulate_payment_workflow(
        payment_id=payment_id,
        account_id=account_id,
        amount_cents=5000,
        cancel=False,
    )

    # --- Scenario 2: Cancelled payment ---
    payment_id2 = f"payment-{uuid.uuid4().hex[:8]}"
    logger.info("\n=== Scenario 2: Cancelled Payment ===")
    logger.info("  Payment ID: %s", payment_id2)

    await simulate_payment_workflow(
        payment_id=payment_id2,
        account_id=account_id,
        amount_cents=3000,
        cancel=True,
    )

    # --- Scenario 3: Cancel non-existent payment ---
    payment_id3 = f"payment-{uuid.uuid4().hex[:8]}"
    logger.info("\n=== Scenario 3: Cancel New (Unstarted) Payment ===")
    logger.info("  Payment ID: %s", payment_id3)

    from workflow import cancel_payment, CancelRequest

    cancel_ctx = HostCalls(workflow_id=payment_id3)
    await cancel_payment(cancel_ctx, CancelRequest(payment_id=payment_id3))
    logger.info("Payment cancelled: %s (was NEW, now CANCELED)", payment_id3)

    # --- Scenario 4: Make payment for already-cancelled ID ---
    logger.info("\n=== Scenario 4: Attempt Payment on Cancelled ID ===")
    logger.info("  Payment ID: %s", payment_id3)

    from workflow import make_payment, PaymentRequest

    ctx3 = HostCalls(workflow_id=payment_id3)
    result3 = await make_payment(
        ctx3,
        PaymentRequest(
            payment_id=payment_id3,
            account_id=account_id,
            amount_cents=1000,
        ),
    )
    logger.info("Result: %s (expected: 'Payment already cancelled')", result3)

    # --- Scenario 5: Duplicate payment (idempotency) ---
    logger.info("\n=== Scenario 5: Duplicate Payment (Idempotency) ===")
    logger.info("  Payment ID: %s", payment_id)

    ctx5 = HostCalls(workflow_id=payment_id)
    result5 = await make_payment(
        ctx5,
        PaymentRequest(
            payment_id=payment_id,
            account_id=account_id,
            amount_cents=9999,
        ),
    )
    logger.info(
        "Result: %s (expected: 'Payment already completed in prior call')",
        result5,
    )

    # --- Scenario 6: Insufficient funds ---
    logger.info("\n=== Scenario 6: Insufficient Funds ===")
    payment_id6 = f"payment-{uuid.uuid4().hex[:8]}"

    # Make a very large withdrawal that should exceed balance
    ctx6 = HostCalls(workflow_id=payment_id6)
    result6 = await make_payment(
        ctx6,
        PaymentRequest(
            payment_id=payment_id6,
            account_id=account_id,
            amount_cents=999_999_999,  # Way more than random initial balance
        ),
    )
    logger.info("Result: %s (expected: 'Insufficient funds...')", result6)

    # Verify status is back to NEW (not stuck in PROCESSING)
    from workflow import _get_status

    status6 = _get_status(ctx6, payment_id6)
    logger.info("Status after failed payment: %s (expected: NEW)", status6)

    # --- Scenario 7: Cancel then re-pay (retry after cancellation) ---
    logger.info("\n=== Scenario 7: Cancel Then Re-pay ===")
    payment_id7 = f"payment-{uuid.uuid4().hex[:8]}"
    account_id7 = f"acct-{uuid.uuid4().hex[:8]}"

    await simulate_payment_workflow(
        payment_id=payment_id7,
        account_id=account_id7,
        amount_cents=5000,
        cancel=True,
    )

    # Try to pay again - should be blocked
    ctx7b = HostCalls(workflow_id=payment_id7)
    result7b = await make_payment(
        ctx7b,
        PaymentRequest(
            payment_id=payment_id7,
            account_id=account_id7,
            amount_cents=5000,
        ),
    )
    logger.info(
        "Result after cancel: %s (expected: 'Payment already cancelled')",
        result7b,
    )

    logger.info("\nAll scenarios completed.")


if __name__ == "__main__":
    asyncio.run(main())
