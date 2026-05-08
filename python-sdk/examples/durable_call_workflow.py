"""Durable Call workflow - demonstrates the core cleat DurableCall pattern.

This workflow:
  1. Takes an input string (user_id and message)
  2. Makes a DurableCall to an external "notifier" service
  3. Returns the service response

Used by the Python WASM end-to-end test to validate the full
Python -> WASM -> Host -> WASM -> Python round trip.
"""
from dataclasses import dataclass
from cleat_sdk import HostCalls, cleat_entry


@dataclass
class NotifyRequest:
    user_id: str
    message: str
    channel: str = "email"


@cleat_entry
def durable_call_workflow(h: HostCalls, request: NotifyRequest) -> str:
    """A workflow that calls a notification service durably.

    This exercises the core DurableCall path across the Python-WASM-Go boundary:
    - Input serialization: Python dataclass -> JSON -> WASM linear memory
    - Host call: WASM import "cleat_call" -> Go host handler
    - Output deserialization: WASM linear memory -> JSON -> Python string
    """
    h.cleat_log(f"Notifying user {request.user_id} via {request.channel}")

    # Make a durable recorded call to the external notification service.
    response = h.cleat_call(
        "notifier", "SendNotification",
        {
            "user_id": request.user_id,
            "message": request.message,
            "channel": request.channel,
        },
    )

    h.cleat_log(f"Notification result: {response}")
    return response
