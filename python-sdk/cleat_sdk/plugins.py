"""Typed plugin convenience wrappers for the Cleat Python SDK.

Provides the :class:`Plugins` class that wraps a :class:`HostCalls` instance
and provides typed Python methods for every cleat plugin host function.

Each method:

1. Constructs the JSON input dict from typed Python parameters
2. Calls ``HostCalls.plugin_call(plugin_name, function_name, input)``
3. Parses the JSON response into a typed dataclass
4. Raises :exc:`RuntimeError` on protocol errors

Usage::

    from cleat_sdk import HostCalls, Plugins

    host = HostCalls()
    p = Plugins(host)

    # blobstore
    result = p.blobstore_put("my-key", b"binary data",
                             content_type="application/octet-stream")
    print(f"Stored at key={result.key} sha256={result.sha256}")

    # event triggers
    ev = p.await_event("payment_received", timeout_ms=30000)
    if ev.found:
        print(f"Got event {ev.event_id}: {ev.event_data}")

    # feature flags
    flag = p.evaluate_flag("new_checkout_flow", context={"user_id": "u-42"})
    if flag.enabled:
        print(f"Flag {flag.key} is enabled")

    # kafka
    result = p.produce("kafka-config-1", '{"order": "ord-42"}', key="ord-42")
    if not result.success:
        print(f"Kafka produce failed: {result.error}")

    # notifications
    result = p.send_webhook("wh-1", "order.shipped",
                            payload={"order_id": "ord-42"})
    print(f"Webhook delivery ID: {result.delivery_id}")

    # pagerduty
    incident = p.trigger_incident("pd-1", "Service is down",
                                  "critical", "monitoring")
    result = p.resolve_incident("pd-1", incident.incident_key)

    # slack
    result = p.send_message("slack-1", "Hello, world!", channel="#general")

    # webhook ingestion (inbound)
    wh = p.await_webhook("source-1", event_type="order.created")
    if wh.found:
        print(f"Got webhook: {wh.payload}")
"""

from __future__ import annotations

import base64
import json
from dataclasses import dataclass, field
from typing import Any

from .host_calls import HostCalls

# ========================================================================
# Result dataclasses
# ========================================================================


# -- blobstore -----------------------------------------------------------


@dataclass
class BlobPutResult:
    """Result of a :meth:`Plugins.blobstore_put` call."""

    key: str
    """The key under which the blob was stored."""

    sha256: str
    """SHA-256 hex digest of the stored blob."""

    size: int
    """Size of the stored blob in bytes."""


@dataclass
class BlobGetResult:
    """Result of a :meth:`Plugins.blobstore_get` call."""

    key: str
    """The key that was retrieved."""

    sha256: str
    """SHA-256 hex digest of the retrieved blob."""

    size: int
    """Size of the retrieved blob in bytes."""

    content_type: str = ""
    """Content type of the blob (empty if unknown)."""

    data: str = ""
    """Base64-encoded blob data."""


# -- eventtriggers -------------------------------------------------------


@dataclass
class AwaitEventResult:
    """Result of an :meth:`Plugins.await_event` call."""

    found: bool
    """``True`` if an event was received before the timeout."""

    event_id: str = ""
    """Unique event ID (empty if no event was found)."""

    event_type: str = ""
    """The received event type (empty if no event was found)."""

    event_data: Any = None
    """Arbitrary event payload (``None`` if no event was found)."""

    received_at: str = ""
    """ISO-8601 timestamp of when the event was received."""


# -- featureflags --------------------------------------------------------


@dataclass
class EvaluateFlagResult:
    """Result of an :meth:`Plugins.evaluate_flag` call."""

    enabled: bool
    """``True`` if the feature flag is enabled for the given context."""

    key: str
    """The feature flag key."""

    evaluation: dict = field(default_factory=dict)
    """Detailed evaluation metadata (may be empty)."""


# -- kafkaconnect --------------------------------------------------------


@dataclass
class ProduceResult:
    """Result of a :meth:`Plugins.produce` call."""

    success: bool
    """``True`` if the message was produced successfully."""

    error: str = ""
    """Error message (empty on success)."""


# -- notifications -------------------------------------------------------


@dataclass
class SendWebhookResult:
    """Result of a :meth:`Plugins.send_webhook` call."""

    delivery_id: str
    """Unique ID for the webhook delivery."""


# -- pagerdutyalert ------------------------------------------------------


@dataclass
class TriggerIncidentResult:
    """Result of a :meth:`Plugins.trigger_incident` call."""

    incident_key: str
    """Unique key for the triggered incident."""

    status: str
    """Incident status (e.g. ``"triggered"``, ``"acknowledged"``)."""


@dataclass
class ResolveIncidentResult:
    """Result of a :meth:`Plugins.resolve_incident` call."""

    status: str
    """Resolution status (e.g. ``"resolved"``)."""


# -- slacknotify ---------------------------------------------------------


@dataclass
class SendMessageResult:
    """Result of a :meth:`Plugins.send_message` call."""

    success: bool
    """``True`` if the message was sent successfully."""

    ts: str = ""
    """Slack message timestamp (empty on failure)."""


# -- webhookingest -------------------------------------------------------


@dataclass
class AwaitWebhookResult:
    """Result of an :meth:`Plugins.await_webhook` call."""

    found: bool
    """``True`` if a matching webhook was available."""

    id: str = ""
    """Unique webhook ID (empty if no webhook was found)."""

    event_type: str = ""
    """The received webhook event type (empty if no webhook was found)."""

    payload: Any = None
    """Arbitrary webhook payload (``None`` if no webhook was found)."""

    received_at: str = ""
    """ISO-8601 timestamp of when the webhook was received."""


# ========================================================================
# Plugins -- typed convenience wrapper
# ========================================================================


class Plugins:
    """Typed convenience wrappers for cleat plugin host functions.

    Wraps a :class:`HostCalls` instance and exposes every cleat plugin
    function as a typed Python method with documented parameters and output
    dataclasses.

    Parameters
    ----------
    host : HostCalls
        The ``HostCalls`` instance to use for low-level plugin calls.
    """

    def __init__(self, host: HostCalls) -> None:
        self._h = host

    # --------------------------------------------------------------------
    # Internal helpers
    # --------------------------------------------------------------------

    @staticmethod
    def _call(
        host: HostCalls,
        plugin: str,
        function: str,
        inp: Any,
        result_type: type,
    ) -> Any:
        """Call *plugin.function*(*inp*) and parse the JSON response.

        Parameters
        ----------
        host : HostCalls
            The host calls instance.
        plugin : str
            Plugin name.
        function : str
            Function name within the plugin.
        inp : dict
            Input dictionary (JSON-serialised automatically).
        result_type : type
            Dataclass type to construct from the JSON response.

        Returns
        -------
        T
            An instance of *result_type*.

        Raises
        ------
        RuntimeError
            If the plugin call fails or the response is not a JSON object.
        """
        response = host.plugin_call(plugin, function, inp)
        try:
            data = json.loads(response)
        except json.JSONDecodeError as e:
            raise RuntimeError(
                f"Plugin {plugin}.{function} returned invalid JSON: {e}"
            ) from e
        if isinstance(data, dict):
            return result_type(**data)
        raise RuntimeError(
            f"Plugin {plugin}.{function} expected a JSON object response, "
            f"got {type(data).__name__}"
        )

    # --------------------------------------------------------------------
    # blobstore
    # --------------------------------------------------------------------

    def blobstore_put(
        self,
        key: str,
        data: bytes | str,
        content_type: str = "",
        tags: dict | None = None,
        ttl: str = "",
    ) -> BlobPutResult:
        """Store a blob in the blobstore plugin.

        Parameters
        ----------
        key : str
            The key to store the blob under.
        data : bytes or str
            The blob data.  ``bytes`` values are base64-encoded
            automatically before being sent to the plugin.
        content_type : str
            Optional MIME content type.
        tags : dict or None
            Optional tags to attach to the blob.
        ttl : str
            Optional TTL duration (e.g. ``"24h"``, ``"7d"``).

        Returns
        -------
        BlobPutResult
            The result including the key, SHA-256 digest, and size.

        Raises
        ------
        RuntimeError
            If the plugin call fails.
        """
        if isinstance(data, bytes):
            data = base64.b64encode(data).decode("ascii")

        inp: dict = {"key": key, "data": data}
        if content_type:
            inp["content_type"] = content_type
        if tags is not None:
            inp["tags"] = tags
        if ttl:
            inp["ttl"] = ttl

        return self._call(self._h, "blobstore", "put", inp, BlobPutResult)

    def blobstore_get(self, key: str) -> BlobGetResult:
        """Retrieve a blob from the blobstore plugin.

        Parameters
        ----------
        key : str
            The key to retrieve.

        Returns
        -------
        BlobGetResult
            The blob metadata and base64-encoded data.

        Raises
        ------
        RuntimeError
            If the plugin call fails.
        """
        return self._call(
            self._h, "blobstore", "get", {"key": key}, BlobGetResult
        )

    # --------------------------------------------------------------------
    # eventtriggers
    # --------------------------------------------------------------------

    def await_event(
        self,
        event_type: str,
        timeout_ms: int = 60000,
    ) -> AwaitEventResult:
        """Wait for an external event to arrive.

        Parameters
        ----------
        event_type : str
            The event type to wait for.
        timeout_ms : int
            Maximum wait time in milliseconds (default 60 000).

        Returns
        -------
        AwaitEventResult
            The received event (if found within the timeout).

        Raises
        ------
        RuntimeError
            If the plugin call fails.
        """
        return self._call(
            self._h,
            "eventtriggers",
            "await_event",
            {"event_type": event_type, "timeout_ms": timeout_ms},
            AwaitEventResult,
        )

    # --------------------------------------------------------------------
    # featureflags
    # --------------------------------------------------------------------

    def evaluate_flag(
        self,
        key: str,
        context: dict | None = None,
    ) -> EvaluateFlagResult:
        """Evaluate a feature flag for a given context.

        Parameters
        ----------
        key : str
            The feature flag key.
        context : dict or None
            Optional evaluation context (e.g. ``{"user_id": "u-42"}``).

        Returns
        -------
        EvaluateFlagResult
            The flag evaluation result.

        Raises
        ------
        RuntimeError
            If the plugin call fails.
        """
        inp: dict = {"key": key}
        if context is not None:
            inp["context"] = context
        return self._call(
            self._h, "featureflags", "evaluate_flag", inp, EvaluateFlagResult
        )

    # --------------------------------------------------------------------
    # kafkaconnect
    # --------------------------------------------------------------------

    def produce(
        self,
        config_id: str,
        value: str,
        key: str = "",
        headers: dict | None = None,
    ) -> ProduceResult:
        """Produce a message to a Kafka topic.

        Parameters
        ----------
        config_id : str
            The Kafka connector configuration ID.
        value : str
            The message value (JSON string).
        key : str
            Optional message key.
        headers : dict or None
            Optional message headers.

        Returns
        -------
        ProduceResult
            Whether the message was produced successfully.

        Raises
        ------
        RuntimeError
            If the plugin call fails.
        """
        inp: dict = {"config_id": config_id, "value": value}
        if key:
            inp["key"] = key
        if headers is not None:
            inp["headers"] = headers
        return self._call(
            self._h, "kafkaconnect", "produce", inp, ProduceResult
        )

    # --------------------------------------------------------------------
    # notifications
    # --------------------------------------------------------------------

    def send_webhook(
        self,
        webhook_id: str,
        event_type: str,
        payload: Any = None,
    ) -> SendWebhookResult:
        """Send a webhook notification.

        Parameters
        ----------
        webhook_id : str
            The webhook configuration ID.
        event_type : str
            The event type to send.
        payload : Any
            Optional payload to include in the webhook body.

        Returns
        -------
        SendWebhookResult
            The webhook delivery ID.

        Raises
        ------
        RuntimeError
            If the plugin call fails.
        """
        inp: dict = {"webhook_id": webhook_id, "event_type": event_type}
        if payload is not None:
            inp["payload"] = payload
        return self._call(
            self._h, "notifications", "send_webhook", inp, SendWebhookResult
        )

    # --------------------------------------------------------------------
    # pagerdutyalert
    # --------------------------------------------------------------------

    def trigger_incident(
        self,
        config_id: str,
        summary: str,
        severity: str,
        source: str,
        details: str = "",
    ) -> TriggerIncidentResult:
        """Trigger a PagerDuty incident.

        Parameters
        ----------
        config_id : str
            The PagerDuty connector configuration ID.
        summary : str
            A short summary of the incident.
        severity : str
            Severity level (e.g. ``"critical"``, ``"warning"``, ``"info"``).
        source : str
            The source of the incident (e.g. ``"monitoring"``).
        details : str
            Optional detailed description.

        Returns
        -------
        TriggerIncidentResult
            The triggered incident details.

        Raises
        ------
        RuntimeError
            If the plugin call fails.
        """
        inp: dict = {
            "config_id": config_id,
            "summary": summary,
            "severity": severity,
            "source": source,
        }
        if details:
            inp["details"] = details
        return self._call(
            self._h,
            "pagerdutyalert",
            "trigger_incident",
            inp,
            TriggerIncidentResult,
        )

    def resolve_incident(
        self,
        config_id: str,
        incident_key: str,
    ) -> ResolveIncidentResult:
        """Resolve a PagerDuty incident.

        Parameters
        ----------
        config_id : str
            The PagerDuty connector configuration ID.
        incident_key : str
            The incident key (from :meth:`trigger_incident`).

        Returns
        -------
        ResolveIncidentResult
            The resolution status.

        Raises
        ------
        RuntimeError
            If the plugin call fails.
        """
        return self._call(
            self._h,
            "pagerdutyalert",
            "resolve_incident",
            {"config_id": config_id, "incident_key": incident_key},
            ResolveIncidentResult,
        )

    # --------------------------------------------------------------------
    # slacknotify
    # --------------------------------------------------------------------

    def send_message(
        self,
        config_id: str,
        text: str,
        channel: str = "",
        blocks: Any = None,
    ) -> SendMessageResult:
        """Send a Slack message.

        Parameters
        ----------
        config_id : str
            The Slack connector configuration ID.
        text : str
            The message text.
        channel : str
            Optional target channel (overrides the configured default).
        blocks : Any
            Optional Slack Block Kit blocks (as a list of dicts).

        Returns
        -------
        SendMessageResult
            Whether the message was sent successfully.

        Raises
        ------
        RuntimeError
            If the plugin call fails.
        """
        inp: dict = {"config_id": config_id, "text": text}
        if channel:
            inp["channel"] = channel
        if blocks is not None:
            inp["blocks"] = blocks
        return self._call(
            self._h, "slacknotify", "send_message", inp, SendMessageResult
        )

    # --------------------------------------------------------------------
    # webhookingest
    # --------------------------------------------------------------------

    def await_webhook(
        self,
        source_id: str,
        event_type: str = "",
    ) -> AwaitWebhookResult:
        """Wait for an incoming webhook from a webhook source.

        Parameters
        ----------
        source_id : str
            The webhook source configuration ID.
        event_type : str
            Optional event type filter.

        Returns
        -------
        AwaitWebhookResult
            The received webhook (if one was available).

        Raises
        ------
        RuntimeError
            If the plugin call fails.
        """
        inp: dict = {"source_id": source_id}
        if event_type:
            inp["event_type"] = event_type
        return self._call(
            self._h,
            "webhookingest",
            "await_webhook",
            inp,
            AwaitWebhookResult,
        )
