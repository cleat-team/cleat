"""External client API for the Cleat durable execution framework.

Provides the :class:`CleatClient` class for programmatically interacting with
workflows from outside the WASM runtime.  This client communicates with the
Cleat host via REST API calls.

Usage::

    from cleat_sdk.client import CleatClient

    client = CleatClient(base_url="http://localhost:8080")

    # Start a workflow
    run_id = client.start_workflow(
        "order_workflow",
        {"order_id": "ord-42", "items": ["item-1"]},
    )

    # Send a signal to a running workflow
    client.send_signal(run_id, "payment_received", {"amount": 100})

    # Resolve a durable promise
    client.resolve_promise("promise-abc-123", json.dumps({"approved": True}))

    # Query workflow state
    state = client.get_query_state(run_id, "status")
"""

from __future__ import annotations

import json
from typing import Any, Optional

import urllib.request
import urllib.error
import urllib.parse


class CleatClient:
    """Client for interacting with the Cleat durable execution host.

    This client communicates with the Cleat REST API to start workflows,
    send signals, resolve promises, query state, and check status.

    Parameters
    ----------
    base_url : str
        Base URL of the Cleat host (e.g., ``"http://localhost:8080"``).
    timeout : float
        Request timeout in seconds.  Default 30.
    """

    def __init__(
        self,
        base_url: str = "http://localhost:8080",
        timeout: float = 30.0,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._timeout = timeout

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _request(
        self,
        method: str,
        path: str,
        body: Optional[str] = None,
    ) -> tuple[int, str]:
        """Make an HTTP request to the Cleat host.

        Parameters
        ----------
        method : str
            HTTP method (``"GET"``, ``"POST"``, etc.).
        path : str
            URL path (e.g., ``"/api/workflows"``).
        body : str or None
            Request body (JSON string).

        Returns
        -------
        tuple[int, str]
            ``(status_code, response_body)``.

        Raises
        ------
        RuntimeError
            If the request fails or the host returns an error status.
        """
        url = f"{self._base_url}{path}"
        data = body.encode("utf-8") if body else None
        req = urllib.request.Request(
            url,
            data=data,
            method=method,
            headers={"Content-Type": "application/json"} if body else {},
        )
        try:
            with urllib.request.urlopen(req, timeout=self._timeout) as resp:
                return (resp.status, resp.read().decode("utf-8"))
        except urllib.error.HTTPError as e:
            error_body = e.read().decode("utf-8", errors="replace")
            raise RuntimeError(
                f"Cleat API error {e.code} for {method} {path}: {error_body}"
            ) from e
        except urllib.error.URLError as e:
            raise RuntimeError(
                f"Cleat API request failed for {method} {path}: {e.reason}"
            ) from e

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def start_workflow(
        self,
        name: str,
        input: Any,
        idempotency_key: Optional[str] = None,
    ) -> str:
        """Start a new workflow execution.

        Parameters
        ----------
        name : str
            The workflow definition name (matches ``@durable_entry``).
        input : Any
            Input payload (JSON-serialisable).
        idempotency_key : str or None
            Optional idempotency key for deduplication.

        Returns
        -------
        str
            The run ID of the started workflow.

        Raises
        ------
        RuntimeError
            If the host returns an error.
        """
        body: dict[str, Any] = {
            "workflow_name": name,
            "input": input,
        }
        if idempotency_key is not None:
            body["idempotency_key"] = idempotency_key

        status, response = self._request(
            "POST", "/api/workflows", json.dumps(body)
        )
        data = json.loads(response)
        run_id = data.get("run_id") or data.get("id", "")
        if not run_id:
            raise RuntimeError(
                f"start_workflow response missing run_id: {response}"
            )
        return run_id

    def send_signal(
        self,
        run_id: str,
        signal_name: str,
        payload: Any,
    ) -> None:
        """Send a signal to a running workflow.

        Parameters
        ----------
        run_id : str
            The workflow run ID.
        signal_name : str
            The signal name.
        payload : Any
            Signal payload (JSON-serialisable).

        Raises
        ------
        RuntimeError
            If the host returns an error.
        """
        body = json.dumps(payload)
        self._request(
            "POST",
            f"/api/workflows/{run_id}/signal/{signal_name}",
            body,
        )

    def resolve_promise(self, promise_id: str, value: str) -> None:
        """Resolve a durable promise with a value.

        Parameters
        ----------
        promise_id : str
            The promise ID to resolve.
        value : str
            The JSON value to resolve the promise with.

        Raises
        ------
        RuntimeError
            If the host returns an error.
        """
        body = json.dumps({"value": value})
        self._request(
            "POST",
            f"/api/promises/{promise_id}/resolve",
            body,
        )

    def get_query_state(self, run_id: str, key: str) -> Any:
        """Get a queryable state value from a workflow.

        Parameters
        ----------
        run_id : str
            The workflow run ID.
        key : str
            The state key to query.

        Returns
        -------
        Any
            The state value (parsed from JSON).

        Raises
        ------
        RuntimeError
            If the host returns an error.
        """
        status, response = self._request(
            "GET",
            f"/api/workflows/{run_id}/state/{key}",
        )
        return json.loads(response)

    def get_workflow_status(self, run_id: str) -> dict:
        """Get the current status of a workflow.

        Parameters
        ----------
        run_id : str
            The workflow run ID.

        Returns
        -------
        dict
            Workflow status including state, result, error, and timestamps.

        Raises
        ------
        RuntimeError
            If the host returns an error.
        """
        status, response = self._request(
            "GET",
            f"/api/workflows/{run_id}",
        )
        return json.loads(response)

    def send_update(
        self,
        run_id: str,
        update_name: str,
        payload: Any,
    ) -> dict:
        """Send an update to a running workflow.

        Updates are like signals but carry a response from the workflow.
        The update handler registered in the workflow processes the payload
        and returns a result.

        Parameters
        ----------
        run_id : str
            The workflow run ID.
        update_name : str
            The update handler name (matches a registered update handler
            in the workflow).
        payload : Any
            Update payload (JSON-serialisable).

        Returns
        -------
        dict
            The update response from the workflow.

        Raises
        ------
        RuntimeError
            If the host returns an error.
        """
        body = json.dumps(payload)
        status, response = self._request(
            "POST",
            f"/api/workflows/{run_id}/update/{update_name}",
            body,
        )
        return json.loads(response)
