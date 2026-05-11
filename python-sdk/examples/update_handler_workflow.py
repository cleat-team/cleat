"""Update handler workflow - demonstrates registering and handling workflow updates.

Update handlers allow external clients to send update requests to a running
workflow. The handler receives a JSON payload and returns a JSON result.

Usage pattern::

    # Workflow code registers a handler
    h.register_update_handler(
        "approve",
        handler=lambda payload: json.dumps({"approved": True, "payload": payload}),
        validator=lambda payload: json.loads(payload).get("amount", 0) > 0,
    )

    # External client sends an update
    # POST /workflows/{run_id}/update/approve {"amount": 100}
    # Response: {"approved": true, "payload": "{\"amount\": 100}"}
"""

import json
from dataclasses import dataclass
from cleat_sdk import HostCalls, cleat_entry


@dataclass
class ApprovalRequest:
    task_id: str
    requires_approval: bool = True


@cleat_entry
def approval_workflow(h: HostCalls, request: ApprovalRequest) -> str:
    """A workflow that registers an update handler for external approval."""
    h.cleat_log(f"Starting approval workflow for task {request.task_id}")

    # Register an update handler that external clients can call
    def handle_approve(payload: str) -> str:
        """Handle an approval update from an external client.

        Parameters
        ----------
        payload : str
            JSON string with approval decision, e.g. {"approved": true, "reason": "ok"}

        Returns
        -------
        str
            JSON result string with the approval status.
        """
        data = json.loads(payload)
        approved = data.get("approved", False)
        h.cleat_log(f"Approval decision for {request.task_id}: {approved}")
        return json.dumps(
            {
                "task_id": request.task_id,
                "approved": approved,
                "reason": data.get("reason", ""),
            }
        )

    def validate_approve(payload: str) -> bool:
        """Validate the approval payload.

        Parameters
        ----------
        payload : str
            JSON string to validate.

        Returns
        -------
        bool
            True if the payload is valid.
        """
        try:
            data = json.loads(payload)
            return "approved" in data
        except (json.JSONDecodeError, TypeError):
            return False

    h.register_update_handler(
        "approve",
        handler=handle_approve,
        validator=validate_approve,
    )

    h.cleat_log(f"Update handler registered for task {request.task_id}")
    return json.dumps(
        {
            "status": "waiting",
            "task_id": request.task_id,
            "message": "Approval handler registered. Send update to /update/approve",
        }
    )
