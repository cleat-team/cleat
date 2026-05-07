"""LangChain integration for the Cleat durable execution framework.

Provides callback handlers that record LangChain agent steps as cleat
durable events, enabling crash recovery and deterministic replay.
"""

from .callbacks import CleatCallbackHandler

__all__ = ["CleatCallbackHandler"]
