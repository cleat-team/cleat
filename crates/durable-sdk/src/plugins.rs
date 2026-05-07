// Typed plugin wrappers for the cleat host plugin system.
//
// Each method builds a JSON input object matching the plugin's expected schema,
// calls [`plugin_call`] on the host, and deserializes the response into a
// strongly-typed result struct.
//
// Plugin name strings match the Go plugin registration names (e.g.
// "event-triggers", "feature-flags", "kafka-connect").

use crate::HostCalls;
use serde::{Deserialize, Serialize};

// ---------------------------------------------------------------------------
// Result types
// ---------------------------------------------------------------------------

/// Result from blobstore_put.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BlobPutResult {
    pub key: String,
    pub sha256: String,
    pub size: i64,
}

/// Result from blobstore_get.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BlobGetResult {
    pub key: String,
    pub sha256: String,
    pub size: i64,
    pub content_type: String,
    pub data: String,
}

/// Result from await_event.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AwaitEventResult {
    pub found: bool,
    #[serde(default)]
    pub event_id: String,
    #[serde(default)]
    pub event_type: String,
    #[serde(default)]
    pub event_data: serde_json::Value,
    #[serde(default)]
    pub received_at: String,
}

/// Result from evaluate_flag.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EvaluateFlagResult {
    pub enabled: bool,
    pub key: String,
    #[serde(default)]
    pub evaluation: Option<serde_json::Value>,
}

/// Result from Kafka produce.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProduceResult {
    pub success: bool,
    #[serde(default)]
    pub error: String,
}

/// Result from send_webhook.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SendWebhookResult {
    pub delivery_id: String,
}

/// Result from trigger_incident.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TriggerIncidentResult {
    pub incident_key: String,
    pub status: String,
}

/// Result from resolve_incident.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ResolveIncidentResult {
    pub status: String,
}

/// Result from send_message.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SendMessageResult {
    pub success: bool,
    #[serde(default)]
    pub ts: String,
}

/// Result from await_webhook.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AwaitWebhookResult {
    pub found: bool,
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub event_type: String,
    #[serde(default)]
    pub payload: Option<serde_json::Value>,
    #[serde(default)]
    pub received_at: String,
}

// ---------------------------------------------------------------------------
// Plugins wrapper
// ---------------------------------------------------------------------------

/// Typed wrapper around [`HostCalls`] for cleat plugin host functions.
///
/// Each method corresponds to a plugin registered with the cleat host runtime.
/// Methods return a `(Result, Option<String>)` tuple where the second element
/// is `Some(error_message)` on failure.
pub struct Plugins<'a> {
    host: &'a HostCalls,
}

impl<'a> Plugins<'a> {
    /// Create a new Plugins wrapper around the given host.
    pub fn new(host: &'a HostCalls) -> Self {
        Self { host }
    }

    // -----------------------------------------------------------------------
    // blobstore
    // -----------------------------------------------------------------------

    /// Store a blob by key. The `data` field must be base64-encoded.
    pub fn blobstore_put(
        &self,
        key: &str,
        data: &str,
        content_type: &str,
        tags: Option<&std::collections::HashMap<String, String>>,
        ttl: &str,
    ) -> (BlobPutResult, Option<String>) {
        let mut m = serde_json::Map::new();
        m.insert("key".into(), key.into());
        m.insert("data".into(), data.into());
        m.insert("content_type".into(), content_type.into());
        if let Some(t) = tags {
            m.insert("tags".into(), serde_json::to_value(t).unwrap_or_default());
        }
        if !ttl.is_empty() {
            m.insert("ttl".into(), ttl.into());
        }
        let (resp, err) = self.host.plugin_call("blobstore", "put", &serde_json::Value::Object(m).to_string());
        default_on_error::<BlobPutResult>(resp, err, || BlobPutResult {
            key: String::new(),
            sha256: String::new(),
            size: 0,
        })
    }

    /// Retrieve a blob by key. The `data` field in the result is base64-encoded.
    pub fn blobstore_get(&self, key: &str) -> (BlobGetResult, Option<String>) {
        let mut m = serde_json::Map::new();
        m.insert("key".into(), key.into());
        let (resp, err) = self.host.plugin_call("blobstore", "get", &serde_json::Value::Object(m).to_string());
        default_on_error::<BlobGetResult>(resp, err, || BlobGetResult {
            key: String::new(),
            sha256: String::new(),
            size: 0,
            content_type: String::new(),
            data: String::new(),
        })
    }

    // -----------------------------------------------------------------------
    // event-triggers
    // -----------------------------------------------------------------------

    /// Await an event of the given type with a timeout.
    pub fn await_event(&self, event_type: &str, timeout_ms: i64) -> (AwaitEventResult, Option<String>) {
        let mut m = serde_json::Map::new();
        m.insert("event_type".into(), event_type.into());
        m.insert("timeout_ms".into(), timeout_ms.into());
        let (resp, err) = self.host.plugin_call("event-triggers", "await_event", &serde_json::Value::Object(m).to_string());
        default_on_error::<AwaitEventResult>(resp, err, || AwaitEventResult {
            found: false,
            event_id: String::new(),
            event_type: String::new(),
            event_data: serde_json::Value::Null,
            received_at: String::new(),
        })
    }

    // -----------------------------------------------------------------------
    // feature-flags
    // -----------------------------------------------------------------------

    /// Evaluate a feature flag. The optional `context` is a JSON object
    /// typically containing `"user_id"` and/or `"attributes"` fields.
    pub fn evaluate_flag(
        &self,
        key: &str,
        context: Option<&serde_json::Value>,
    ) -> (EvaluateFlagResult, Option<String>) {
        let mut m = serde_json::Map::new();
        m.insert("key".into(), key.into());
        if let Some(ctx) = context {
            m.insert("context".into(), ctx.clone());
        }
        let (resp, err) = self.host.plugin_call("feature-flags", "evaluate_flag", &serde_json::Value::Object(m).to_string());
        default_on_error::<EvaluateFlagResult>(resp, err, || EvaluateFlagResult {
            enabled: false,
            key: String::new(),
            evaluation: None,
        })
    }

    // -----------------------------------------------------------------------
    // kafka-connect
    // -----------------------------------------------------------------------

    /// Produce a message to a Kafka topic.
    pub fn produce(
        &self,
        config_id: &str,
        value: &str,
        key: &str,
        headers: Option<&std::collections::HashMap<String, String>>,
    ) -> (ProduceResult, Option<String>) {
        let mut m = serde_json::Map::new();
        m.insert("config_id".into(), config_id.into());
        m.insert("value".into(), value.into());
        if !key.is_empty() {
            m.insert("key".into(), key.into());
        }
        if let Some(h) = headers {
            m.insert("headers".into(), serde_json::to_value(h).unwrap_or_default());
        }
        let (resp, err) = self.host.plugin_call("kafka-connect", "produce", &serde_json::Value::Object(m).to_string());
        default_on_error::<ProduceResult>(resp, err, || ProduceResult {
            success: false,
            error: String::new(),
        })
    }

    // -----------------------------------------------------------------------
    // notifications
    // -----------------------------------------------------------------------

    /// Send a webhook.
    pub fn send_webhook(
        &self,
        webhook_id: &str,
        event_type: &str,
        payload: Option<&serde_json::Value>,
    ) -> (SendWebhookResult, Option<String>) {
        let mut m = serde_json::Map::new();
        m.insert("webhook_id".into(), webhook_id.into());
        m.insert("event_type".into(), event_type.into());
        if let Some(p) = payload {
            m.insert("payload".into(), p.clone());
        }
        let (resp, err) = self.host.plugin_call("notifications", "send_webhook", &serde_json::Value::Object(m).to_string());
        default_on_error::<SendWebhookResult>(resp, err, || SendWebhookResult {
            delivery_id: String::new(),
        })
    }

    // -----------------------------------------------------------------------
    // pagerduty-alert
    // -----------------------------------------------------------------------

    /// Trigger a PagerDuty incident.
    pub fn trigger_incident(
        &self,
        config_id: &str,
        summary: &str,
        severity: &str,
        source: &str,
        details: Option<&str>,
    ) -> (TriggerIncidentResult, Option<String>) {
        let mut m = serde_json::Map::new();
        m.insert("config_id".into(), config_id.into());
        m.insert("summary".into(), summary.into());
        m.insert("severity".into(), severity.into());
        m.insert("source".into(), source.into());
        if let Some(d) = details {
            m.insert("details".into(), d.into());
        }
        let (resp, err) = self.host.plugin_call("pagerduty-alert", "trigger_incident", &serde_json::Value::Object(m).to_string());
        default_on_error::<TriggerIncidentResult>(resp, err, || TriggerIncidentResult {
            incident_key: String::new(),
            status: String::new(),
        })
    }

    /// Resolve a PagerDuty incident by its incident key.
    pub fn resolve_incident(&self, config_id: &str, incident_key: &str) -> (ResolveIncidentResult, Option<String>) {
        let mut m = serde_json::Map::new();
        m.insert("config_id".into(), config_id.into());
        m.insert("incident_key".into(), incident_key.into());
        let (resp, err) = self.host.plugin_call("pagerduty-alert", "resolve_incident", &serde_json::Value::Object(m).to_string());
        default_on_error::<ResolveIncidentResult>(resp, err, || ResolveIncidentResult {
            status: String::new(),
        })
    }

    // -----------------------------------------------------------------------
    // slack-notify
    // -----------------------------------------------------------------------

    /// Send a Slack message via a configured webhook.
    pub fn send_message(
        &self,
        config_id: &str,
        text: &str,
        channel: &str,
        blocks: Option<&serde_json::Value>,
    ) -> (SendMessageResult, Option<String>) {
        let mut m = serde_json::Map::new();
        m.insert("config_id".into(), config_id.into());
        m.insert("text".into(), text.into());
        if !channel.is_empty() {
            m.insert("channel".into(), channel.into());
        }
        if let Some(b) = blocks {
            m.insert("blocks".into(), b.clone());
        }
        let (resp, err) = self.host.plugin_call("slack-notify", "send_message", &serde_json::Value::Object(m).to_string());
        default_on_error::<SendMessageResult>(resp, err, || SendMessageResult {
            success: false,
            ts: String::new(),
        })
    }

    // -----------------------------------------------------------------------
    // webhook-ingest
    // -----------------------------------------------------------------------

    /// Await a webhook event from a source.
    pub fn await_webhook(&self, source_id: &str, event_type: &str) -> (AwaitWebhookResult, Option<String>) {
        let mut m = serde_json::Map::new();
        m.insert("source_id".into(), source_id.into());
        m.insert("event_type".into(), event_type.into());
        let (resp, err) = self.host.plugin_call("webhook-ingest", "await_webhook", &serde_json::Value::Object(m).to_string());
        default_on_error::<AwaitWebhookResult>(resp, err, || AwaitWebhookResult {
            found: false,
            id: String::new(),
            event_type: String::new(),
            payload: None,
            received_at: String::new(),
        })
    }
}

/// Helper: if there is an error, return a default-constructed result struct
/// with the error.  Otherwise attempt to deserialize the response JSON.
fn default_on_error<T>(resp: String, err: Option<String>, default: impl FnOnce() -> T) -> (T, Option<String>)
where
    T: for<'de> Deserialize<'de>,
{
    if let Some(e) = err {
        return (default(), Some(e));
    }
    match serde_json::from_str::<T>(&resp) {
        Ok(r) => (r, None),
        Err(e) => (default(), Some(format!("parse error: {}", e))),
    }
}
