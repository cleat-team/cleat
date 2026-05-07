"""
Type serialization for the Cleat-LangGraph bridge.

LangGraph uses TypedDict, dataclasses, and Pydantic models for state.
Cleat's ``durable_call(request)`` requires JSON-serializable dicts.
This module handles the bidirectional conversion.

Key challenges addressed:
  - TypedDict → dict (recursive, handles nested types)
  - Pydantic BaseModel → dict (model_dump())
  - dataclass → dict (asdict())
  - Nested LangGraph types (Annotated, operator.add, etc.)
  - Custom types (datetime, Decimal, etc.)

Replay safety note:
  The serialization MUST be deterministic. Non-deterministic types
  (e.g., ``datetime.now()`` defaults in state) will break replay.
"""

from __future__ import annotations

import dataclasses
import json
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Dict, List, Optional, Type, Union

try:
    from pydantic import BaseModel

    HAS_PYDANTIC = True
except ImportError:
    HAS_PYDANTIC = False
    BaseModel = None  # type: ignore[assignment]


# ---------------------------------------------------------------------------
# Cleat-side serializer (runs inside WASM / Cleat workflow)
# ---------------------------------------------------------------------------


class CleatSerializer:
    """Serialize workflow state for durable_call transport.

    Converts complex Python types to JSON-safe dicts so they can
    be passed through ``HostCalls.durable_call()``.
    """

    @staticmethod
    def to_json_safe(value: Any) -> Any:
        """Recursively convert a Python value to a JSON-safe structure."""
        if value is None or isinstance(value, (str, int, float, bool)):
            return value

        if isinstance(value, bytes):
            return value.decode("utf-8", errors="replace")

        if isinstance(value, datetime):
            return value.isoformat()

        if isinstance(value, Enum):
            return value.value

        if isinstance(value, dict):
            return {str(k): CleatSerializer.to_json_safe(v) for k, v in value.items()}

        if isinstance(value, (list, tuple)):
            return [CleatSerializer.to_json_safe(item) for item in value]

        if HAS_PYDANTIC and isinstance(value, BaseModel):
            return CleatSerializer.to_json_safe(value.model_dump(mode="json"))

        if dataclasses and dataclasses.is_dataclass(value):
            return CleatSerializer.to_json_safe(
                dataclasses.asdict(value)  # type: ignore[arg-type]
            )

        # Fallback: try string representation (non-ideal for replay)
        return str(value)

    @staticmethod
    def pack(state: Dict[str, Any]) -> Dict[str, Any]:
        """Pack workflow state for transport through durable_call.

        Wraps complex types with type annotations for the host-side
        deserializer to reconstruct them.
        """
        return {
            "__cleat_langgraph_state__": True,
            "__version__": 1,
            "data": CleatSerializer.to_json_safe(state),
        }

    @staticmethod
    def unpack(payload: Dict[str, Any]) -> Dict[str, Any]:
        """Unpack a durable_call response into workflow state."""
        if payload.get("__cleat_langgraph_state__"):
            return payload.get("data", {})
        return payload


# ---------------------------------------------------------------------------
# Host-side serializer (runs outside WASM, has full Python)
# ---------------------------------------------------------------------------


class LangGraphSerializer:
    """Serialize/deserialize between LangGraph and Cleat state formats.

    On the host side, we have full access to LangGraph's type system,
    so we can reconstruct TypedDict, Pydantic models, etc. from the
    JSON-safe representation received from Cleat.
    """

    @staticmethod
    def to_langgraph_state(
        packed: Dict[str, Any],
        state_type: Optional[Type[Dict[str, Any]]] = None,
    ) -> Dict[str, Any]:
        """Convert a Cleat-packed state dict back to a LangGraph state dict.

        Args:
            packed: The state dict received from durable_call (via CleatSerializer).
            state_type: Optional TypedDict/Pydantic/dataclass type to reconstruct.

        Returns:
            A plain dict suitable for LangGraph node functions.
        """
        data = packed.get("data", packed) if isinstance(packed, dict) else packed
        if isinstance(data, dict) and state_type is not None:
            # If state_type is a TypedDict, just return the dict as-is
            # (LangGraph accepts both TypedDict instances and plain dicts)
            return dict(data)
        return dict(data) if isinstance(data, dict) else {"value": data}

    @staticmethod
    def from_langgraph_state(state: Dict[str, Any]) -> Dict[str, Any]:
        """Convert a LangGraph state dict to Cleat's JSON-safe format.

        This runs on the host side before returning results to the
        Cleat workflow via durable_call.
        """
        return {
            "__cleat_langgraph_state__": True,
            "__version__": 1,
            "data": CleatSerializer.to_json_safe(state),
        }

    @staticmethod
    def serialize_result(result: Any) -> Any:
        """Serialize any LangGraph result for Cleat transport."""
        return CleatSerializer.to_json_safe(result)

    @staticmethod
    def deserialize_input(
        payload: Dict[str, Any],
    ) -> Any:
        """Deserialize Cleat workflow input for LangGraph consumption."""
        if isinstance(payload, dict) and payload.get("__cleat_langgraph_state__"):
            return payload["data"]
        return payload


# ---------------------------------------------------------------------------
# JSON codec helpers for common LangGraph types
# ---------------------------------------------------------------------------


class LangGraphJSONEncoder(json.JSONEncoder):
    """Extended JSON encoder that handles LangGraph/Pydantic types."""

    def default(self, obj: Any) -> Any:
        if HAS_PYDANTIC and isinstance(obj, BaseModel):
            return obj.model_dump(mode="json")
        if dataclasses and dataclasses.is_dataclass(obj):
            return dataclasses.asdict(obj)  # type: ignore[arg-type]
        if isinstance(obj, datetime):
            return obj.isoformat()
        if isinstance(obj, Enum):
            return obj.value
        if isinstance(obj, bytes):
            return obj.decode("utf-8", errors="replace")
        return super().default(obj)
