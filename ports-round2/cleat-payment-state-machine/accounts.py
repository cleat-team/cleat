"""
Account management service for the payment state machine.

This is a cleat durable entry point that manages account balances.
In the original Restate example, this was a Virtual Object with key-scoped
state. In Cleat, we use cleat_entry and manually prefix state keys with
the account_id since Cleat does not have Virtual Object semantics.

Migration notes from Restate:
- Original: VirtualObject("account") with @handler() decorator
- Cleat: @cleat_entry(name="...") with explicit key handling
- Original: ctx.get("balance", type_hint=int) [key-scoped to object instance]
- Cleat: ctx.get_state(account_state_key("balance", account_id)) [manually prefixed]
- Original: ctx.set("balance", value) [key-scoped]
- Cleat: ctx.set_state(account_state_key("balance", account_id), value)
- Original: TerminalError for validation failures
- Cleat: cleat_sdk.TerminalError (same concept)
"""

import random
from dataclasses import dataclass
from typing import Any, Dict, Optional

from cleat_sdk import HostCalls, TerminalError, cleat_entry

# ---------------------------------------------------------------------------
# Data models
# ---------------------------------------------------------------------------


@dataclass
class WithdrawRequest:
    """Request to withdraw funds from an account."""
    account_id: str
    amount_cents: int


@dataclass
class DepositRequest:
    """Request to deposit funds to an account."""
    account_id: str
    amount_cents: int


@dataclass
class Result:
    """Result of an account operation."""
    success: bool
    message: str


# ---------------------------------------------------------------------------
# State key helpers
# ---------------------------------------------------------------------------
# NOTE: Cleat lacks Virtual Object / key-scoped state. To achieve per-account
# state isolation, we manually prefix state keys with the account_id.
# This is a workaround documented in ISSUES.md (#1).


def _account_state_key(account_id: str, field: str) -> str:
    """Build a namespaced state key for a given account and field.

    In Restate, the Virtual Object runtime handles this scoping.
    In Cleat, we must do it manually.
    """
    return f"account:{account_id}:{field}"


BALANCE_FIELD = "balance"

# ---------------------------------------------------------------------------
# Random initial balance (for self-contained example)
# ---------------------------------------------------------------------------


def _initialize_random_amount() -> int:
    """Generate a random initial account balance.

    NOTE: This uses Python's random module, which is NOT deterministic.
    In a real Cleat deployment, non-deterministic calls should be wrapped
    in idempotent steps or use ctx.random() instead.
    """
    return random.randint(100_000, 200_000)


def dataclass_to_dict(obj) -> Dict[str, Any]:
    """Convert a dataclass instance to a dict for serialization."""
    return {f.name: getattr(obj, f.name) for f in obj.__dataclass_fields__.values()}


# ---------------------------------------------------------------------------
# Durable entry points
# ---------------------------------------------------------------------------


@cleat_entry(name="account.deposit")
async def deposit(ctx: HostCalls, request: DepositRequest) -> None:
    """Deposit funds into an account.

    This is ported from:
        @account.handler()
        async def deposit(ctx: restate.ObjectContext, amount_cents: int):

    Original used positional arg; cleat uses a request object for
    compatibility with cleat_call's serialization.

    Raises:
        TerminalError: If amount_cents <= 0.
    """
    if request.amount_cents <= 0:
        raise TerminalError("Amount must be greater than 0")

    balance_key = _account_state_key(request.account_id, BALANCE_FIELD)
    balance_cents = ctx.get_state(balance_key) or _initialize_random_amount()
    ctx.set_state(balance_key, balance_cents + request.amount_cents)


@cleat_entry(name="account.withdraw")
async def withdraw(ctx: HostCalls, request: WithdrawRequest) -> Result:
    """Withdraw funds from an account if sufficient balance exists.

    This is ported from:
        @account.handler()
        async def withdraw(ctx: restate.ObjectContext, amount_cents: int) -> Result:

    Returns:
        Result with success=True if withdrawal was processed,
        Result with success=False if insufficient funds.

    Raises:
        TerminalError: If amount_cents <= 0.
    """
    if request.amount_cents <= 0:
        raise TerminalError("Amount must be greater than 0")

    balance_key = _account_state_key(request.account_id, BALANCE_FIELD)
    balance_cents = ctx.get_state(balance_key) or _initialize_random_amount()

    if balance_cents < request.amount_cents:
        return Result(
            success=False,
            message=f"Insufficient funds: {balance_cents} cents",
        )

    ctx.set_state(balance_key, balance_cents - request.amount_cents)
    return Result(success=True, message="Withdrawal successful")
