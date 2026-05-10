//! Test harness for cleat workflows — WASM-free mock HostCalls for testing
//! workflows with `cargo test` without WASM compilation.
//!
//! Provides:
//!   - [`MockHostCalls`] — mock implementation of the HostCalls API
//!   - Call recording — every host call is tracked in a `Vec<CallRecord>`
//!   - Stub API — pre-configure responses with `register_call_stub()`
//!   - Signal simulation — `deliver_signal()` for external signals
//!   - Test runner — `run_workflow()` to execute workflow functions with the mock
//!   - Assertions — `assert_called()`, `assert_not_called()`, `assert_state()`
//!   - Retry simulation — simulate transient failures with `set_retry_simulation()`
//!
//! # Usage
//!
//! ```rust
//! use cleat_sdk::test::{CleatTest, CallRecord};
//!
//! #[test]
//! fn test_payment_workflow() {
//!     let mut env = CleatTest::new();
//!
//!     // Stub an external service call
//!     env.register_call_stub("payment", "charge", r#"{"id":"ch_123"}"#);
//!
//!     // Deliver a signal
//!     env.deliver_signal("order_confirmed", r#"{"orderId":"ord_1"}"#);
//!
//!     // Run the workflow
//!     let result = env.run_workflow(|h: &MockHostCalls, input: &str| -> String {
//!         // Workflow logic using h instead of real HostCalls
//!         let outcome = h.cleat_call("payment", "charge", input);
//!         format!("{{ \"status\": \"ok\", \"input\": {} }}", input)
//!     }, r#"{"items":[]}"#);
//!
//!     // Assert calls were made
//!     assert!(env.assert_called("payment", "charge"));
//!     assert!(env.assert_not_called("shipping", "ship"));
//! }
//! ```
//!
//! Mirrors Go `durabletest.TestEnv` at durable/durabletest/durabletest.go.

use std::collections::HashMap;

use crate::host_calls::{FetchResult, RetryPolicy};

// ═════════════════════════════════════════════════════════════════════════════
// Public types
// ═════════════════════════════════════════════════════════════════════════════

/// A single recorded call through the test environment.
#[derive(Debug, Clone)]
pub struct CallRecord {
    pub service: String,
    pub operation: String,
    pub request: String,
    pub response: String,
    pub error: Option<String>,
}

/// A pre-configured stub for a call response.
#[derive(Debug, Clone)]
struct CallStub {
    service: String,
    operation: String,
    response: String,
    error: Option<String>,
}

/// A pending signal in the test environment's signal queue.
#[derive(Debug, Clone)]
struct PendingSignal {
    name: String,
    payload: String,
}

/// A stub for a child workflow.
#[derive(Debug, Clone)]
struct ChildWorkflowStub {
    result: String,
    error: Option<String>,
}

/// A stub for a plugin call.
#[derive(Debug, Clone)]
struct PluginCallStub {
    plugin_name: String,
    function_name: String,
    result: String,
    error: Option<String>,
}

/// State of a durable promise in the test environment.
#[derive(Debug, Clone)]
enum PromiseState {
    Pending,
    Resolved(String),
    Rejected(String),
}

/// Result of `await_signals`.
#[derive(Debug, Clone)]
pub struct AwaitSignalsOutcome {
    pub signal_name: String,
    pub payload: String,
    pub timed_out: bool,
    pub error: Option<String>,
}

// ═════════════════════════════════════════════════════════════════════════════
// MockHostCalls — mock implementation of the HostCalls API
// ═════════════════════════════════════════════════════════════════════════════

/// Mock implementation of the cleat HostCalls API for testing workflows
/// without WASM compilation.
///
/// Every host call is recorded for later assertion. Pre-programmed responses
/// are returned when stubs are registered.
#[derive(Debug, Clone)]
pub struct MockHostCalls {
    // ---- Call recording ----
    /// Recorded call history.
    pub call_history: Vec<CallRecord>,

    // ---- Stubs ----
    call_stubs: Vec<CallStub>,
    child_workflow_stubs: HashMap<String, ChildWorkflowStub>,
    child_results: HashMap<String, String>,
    child_errors: HashMap<String, String>,
    plugin_call_stubs: Vec<PluginCallStub>,

    // ---- Signals ----
    pending_signals: Vec<PendingSignal>,
    sent_signals: Vec<String>,

    // ---- Time ----
    /// Simulated clock time in ms since epoch.
    pub now_ms: i64,

    // ---- Version ----
    pub version_val: i32,
    pub min_version_val: i32,

    // ---- Random ----
    random_seq: Vec<i64>,
    random_idx: usize,

    // ---- State ----
    query_state: HashMap<String, String>,
    workflow_state: HashMap<String, String>,

    // ---- Promises ----
    promises: HashMap<String, PromiseState>,

    // ---- Scope ----
    scope_prefix: String,

    // ---- Cancellation ----
    cancelled: bool,
    cancel_reason: String,

    // ---- Retry simulation ----
    retry_sim_count: u32,
    retry_sim_attempts: HashMap<String, u32>,

    // ---- Signal reply ----
    signal_reply_channels: HashMap<String, String>,
    signal_reply_corr_id_counter: u64,

    // ---- Other ----
    defer_counter: u64,
    child_run_id_counter: u64,
    update_handlers: Vec<String>,
    query_handlers: Vec<String>,
    continue_as_new_called: bool,
    pub workflow_id: String,
    pub run_id: String,

    // ---- Scheduled invocations ----
    scheduled_invocations: Vec<String>,
}

impl MockHostCalls {
    /// Create a new MockHostCalls with default state.
    pub fn new() -> Self {
        Self {
            call_history: Vec::new(),
            call_stubs: Vec::new(),
            child_workflow_stubs: HashMap::new(),
            child_results: HashMap::new(),
            child_errors: HashMap::new(),
            plugin_call_stubs: Vec::new(),
            pending_signals: Vec::new(),
            sent_signals: Vec::new(),
            now_ms: 1704067200000, // 2024-01-01T00:00:00Z
            version_val: 1,
            min_version_val: 1,
            random_seq: Vec::new(),
            random_idx: 0,
            query_state: HashMap::new(),
            workflow_state: HashMap::new(),
            promises: HashMap::new(),
            scope_prefix: String::new(),
            cancelled: false,
            cancel_reason: String::new(),
            retry_sim_count: 0,
            retry_sim_attempts: HashMap::new(),
            signal_reply_channels: HashMap::new(),
            signal_reply_corr_id_counter: 0,
            defer_counter: 0,
            child_run_id_counter: 0,
            update_handlers: Vec::new(),
            query_handlers: Vec::new(),
            continue_as_new_called: false,
            workflow_id: "test-workflow".to_string(),
            run_id: "test-run-001".to_string(),
            scheduled_invocations: Vec::new(),
        }
    }

    // ────────────────────────────────────────────
    // HostCalls API methods
    // ────────────────────────────────────────────

    /// Make a recorded API call to an external service.
    /// Returns `(response_json, error_message)` matching the Rust SDK's `HostCalls::cleat_call`.
    pub fn cleat_call(&mut self, service: &str, operation: &str, request_json: &str) -> (String, Option<String>) {
        // Retry simulation
        if self.retry_sim_count > 0 {
            let key = format!("{}/{}", service, operation);
            let attempt = self.retry_sim_attempts.entry(key.clone()).or_insert(0);
            if *attempt < self.retry_sim_count {
                *attempt += 1;
                let err = format!(
                    "simulated transient failure for {} (attempt {}/{})",
                    key, *attempt, self.retry_sim_count
                );
                self.call_history.push(CallRecord {
                    service: service.to_string(),
                    operation: operation.to_string(),
                    request: request_json.to_string(),
                    response: String::new(),
                    error: Some(err.clone()),
                });
                return (String::new(), Some(err));
            }
        }

        // Find matching stub
        let mut stub_idx: Option<usize> = None;
        for (i, stub) in self.call_stubs.iter().enumerate() {
            if stub.service == service && stub.operation == operation {
                stub_idx = Some(i);
                break;
            }
        }

        if let Some(idx) = stub_idx {
            let stub = self.call_stubs.remove(idx);
            let (resp, err) = (stub.response, stub.error);
            self.call_history.push(CallRecord {
                service: service.to_string(),
                operation: operation.to_string(),
                request: request_json.to_string(),
                response: resp.clone(),
                error: err.clone(),
            });
            return (resp, err);
        }

        // No stub registered
        let err = format!("no stub registered for {}.{}", service, operation);
        self.call_history.push(CallRecord {
            service: service.to_string(),
            operation: operation.to_string(),
            request: request_json.to_string(),
            response: String::new(),
            error: Some(err.clone()),
        });
        (String::new(), Some(err))
    }

    /// Typed version of cleat_call_with_retry. In mock mode, delegates to cleat_call.
    pub fn cleat_call_with_retry_typed<T: serde::Serialize, R: serde::de::DeserializeOwned>(
        &mut self, service: &str, operation: &str, request: &T, _retry_policy: &RetryPolicy,
    ) -> Result<R, String> {
        let request_json = serde_json::to_string(request).map_err(|e| format!("serialize: {}", e))?;
        let (resp_json, err) = self.cleat_call(service, operation, &request_json);
        if let Some(e) = err { return Err(e); }
        serde_json::from_str(&resp_json).map_err(|e| format!("deserialize: {}", e))
    }

    /// Typed cleat_call with heartbeat interval. Delegates to cleat_call in mock mode.
    pub fn cleat_call_heartbeat_typed<T: serde::Serialize, R: serde::de::DeserializeOwned>(
        &mut self, service: &str, operation: &str, request: &T, _heartbeat_interval_ms: i64,
    ) -> Result<R, String> {
        let request_json = serde_json::to_string(request).map_err(|e| format!("serialize: {}", e))?;
        let (resp_json, err) = self.cleat_call(service, operation, &request_json);
        if let Some(e) = err { return Err(e); }
        serde_json::from_str(&resp_json).map_err(|e| format!("deserialize: {}", e))
    }

    /// Simulate workflow suspension for a duration.
    pub fn cleat_sleep(&mut self, duration_ms: i64) -> bool {
        self.now_ms += duration_ms;
        false
    }

    /// Get the current simulated wall-clock time.
    pub fn now(&self) -> i64 {
        self.now_ms
    }

    /// Get a deterministic random value from the pre-configured sequence.
    pub fn random(&mut self) -> i64 {
        if self.random_idx < self.random_seq.len() {
            let val = self.random_seq[self.random_idx];
            self.random_idx += 1;
            val
        } else {
            0
        }
    }

    /// Log a message (no-op in test mode).
    pub fn cleat_log(&self, _message: &str) {
        // No-op for testing
    }

    /// Get the workflow definition version.
    pub fn version(&self) -> i32 {
        self.version_val
    }

    /// Get the minimum supported version.
    pub fn min_version(&self) -> i32 {
        self.min_version_val
    }

    /// Register a deferred cleanup action.
    pub fn cleat_defer(&mut self, _description: &str) -> (String, Option<String>) {
        self.defer_counter += 1;
        let defer_id = format!("defer-{}", self.defer_counter);
        (defer_id, None)
    }

    /// Check whether cancellation has been requested.
    pub fn poll_cancellation(&self) -> (bool, String) {
        (self.cancelled, self.cancel_reason.clone())
    }

    /// Poll for a specific pending signal.
    pub fn poll_signal(&mut self, name: &str) -> (String, bool, Option<String>) {
        let mut found_idx: Option<usize> = None;
        for (i, sig) in self.pending_signals.iter().enumerate() {
            if sig.name == name {
                found_idx = Some(i);
                break;
            }
        }

        if let Some(idx) = found_idx {
            let sig = self.pending_signals.remove(idx);
            (sig.payload, true, None)
        } else {
            (String::new(), false, None)
        }
    }

    /// Simulate continue-as-new.
    pub fn continue_as_new(&mut self, _input_json: &str) -> Option<String> {
        self.continue_as_new_called = true;
        None
    }

    /// Start a child workflow instance.
    pub fn child_workflow(&mut self, name: &str, _input_json: &str) -> (String, Option<String>) {
        self.child_run_id_counter += 1;
        let run_id = format!("child-{}-{}", name, self.child_run_id_counter);

        // Check for a child workflow stub
        if let Some(stub) = self.child_workflow_stubs.get(name) {
            if let Some(ref err) = stub.error {
                self.child_errors.insert(run_id.clone(), err.clone());
            } else {
                self.child_results.insert(run_id.clone(), stub.result.clone());
            }
        } else {
            // Default: auto-complete with empty result
            self.child_results.insert(run_id.clone(), r#"{"status":"completed"}"#.to_string());
        }

        (run_id, None)
    }

    /// Start a child workflow with version options (mock: delegates to child_workflow).
    pub fn child_workflow_with_options(&mut self, name: &str, input_json: &str, _version: i32) -> (String, Option<String>) {
        self.child_workflow(name, input_json)
    }

    /// Wait for a child workflow to complete.
    pub fn await_child(&mut self, run_id: &str) -> (String, Option<String>) {
        if let Some(err) = self.child_errors.get(run_id) {
            return (String::new(), Some(err.clone()));
        }
        if let Some(result) = self.child_results.get(run_id) {
            return (result.clone(), None);
        }
        (r#"{"status":"completed"}"#.to_string(), None)
    }

    /// Wait for one or more external signals with a timeout.
    pub fn await_signals(&mut self, signal_names: &[&str], timeout_ms: i64) -> (String, String, bool, Option<String>) {
        let mut found_idx: Option<usize> = None;

        for (i, sig) in self.pending_signals.iter().enumerate() {
            if signal_names.contains(&sig.name.as_str()) {
                found_idx = Some(i);
                break;
            }
        }

        if let Some(idx) = found_idx {
            let sig = self.pending_signals.remove(idx);
            (sig.name, sig.payload, false, None)
        } else {
            // No matching signal — return timeout
            if timeout_ms > 0 {
                self.now_ms += timeout_ms;
            }
            (String::new(), String::new(), true, None)
        }
    }

    /// Set a key-value pair in query state.
    pub fn set_query_state(&mut self, key: &str, value: &str) {
        self.query_state.insert(self.scoped_key(key), value.to_string());
    }

    /// Create a new durable promise.
    pub fn create_promise(&mut self, name: &str) -> (String, Option<String>) {
        self.defer_counter += 1;
        let promise_id = format!("prom-{}-{}", name, self.defer_counter);
        self.promises.insert(promise_id.clone(), PromiseState::Pending);
        (promise_id, None)
    }

    /// Wait for a durable promise to resolve.
    pub fn await_promise(&mut self, id: &str, timeout_ms: i64) -> (String, bool, Option<String>) {
        match self.promises.get(id) {
            None => (String::new(), false, Some(format!("promise not found: {}", id))),
            Some(PromiseState::Resolved(val)) => (val.clone(), false, None),
            Some(PromiseState::Rejected(err)) => (String::new(), false, Some(err.clone())),
            Some(PromiseState::Pending) => {
                self.now_ms += timeout_ms;
                (String::new(), true, None)
            }
        }
    }

    /// Register an update handler.
    pub fn register_update_handler(&mut self, name: &str) {
        self.update_handlers.push(name.to_string());
    }

    /// Call a plugin function via the host runtime.
    pub fn plugin_call(&mut self, plugin_name: &str, function_name: &str, _input_json: &str) -> (String, Option<String>) {
        for stub in &self.plugin_call_stubs {
            if stub.plugin_name == plugin_name && stub.function_name == function_name {
                if let Some(ref err) = stub.error {
                    return (String::new(), Some(err.clone()));
                }
                return (stub.result.clone(), None);
            }
        }
        (
            String::new(),
            Some(format!("no stub registered for plugin {}.{}", plugin_name, function_name)),
        )
    }

    /// Get the current workflow ID.
    pub fn workflow_id(&self) -> String {
        self.workflow_id.clone()
    }

    /// Get the current run ID.
    pub fn run_id(&self) -> String {
        self.run_id.clone()
    }

    /// Resolve a promise.
    pub fn resolve_promise(&mut self, id: &str, value: &str) -> Result<(), String> {
        match self.promises.get_mut(id) {
            None => Err(format!("promise not found: {}", id)),
            Some(state) => {
                *state = PromiseState::Resolved(value.to_string());
                Ok(())
            }
        }
    }

    /// Reject a promise.
    pub fn reject_promise(&mut self, id: &str, error: &str) -> Result<(), String> {
        match self.promises.get_mut(id) {
            None => Err(format!("promise not found: {}", id)),
            Some(state) => {
                *state = PromiseState::Rejected(error.to_string());
                Ok(())
            }
        }
    }

    /// Fire-and-forget durable call.
    pub fn cleat_send(&mut self, service: &str, operation: &str, request_json: &str) -> Result<(), String> {
        self.call_history.push(CallRecord {
            service: service.to_string(),
            operation: operation.to_string(),
            request: request_json.to_string(),
            response: String::new(),
            error: None,
        });
        Ok(())
    }

    /// Schedule a delayed invocation.
    pub fn schedule_invoke(&mut self, service: &str, operation: &str, _request_json: &str, delay_ms: i64) -> Result<(), String> {
        self.scheduled_invocations.push(format!("{}.{}:{}ms", service, operation, delay_ms));
        Ok(())
    }

    /// Schedule a delayed invocation (ms variant, alias for schedule_invoke).
    pub fn schedule_invoke_ms(&mut self, service: &str, operation: &str, request_json: &str, delay_ms: i64) -> Result<(), String> {
        self.schedule_invoke(service, operation, request_json, delay_ms)
    }

    /// Acquire a concurrency lock with a TTL in milliseconds. Simple mock: always succeeds.
    pub fn acquire_lock_ms(&mut self, _key: &str, _ttl_ms: i64) -> (bool, Option<String>) {
        (true, None)
    }

    /// Acquire a concurrency lock with a Duration TTL. Delegates to acquire_lock_ms.
    pub fn acquire_lock(&mut self, key: &str, ttl: std::time::Duration) -> (bool, Option<String>) {
        self.acquire_lock_ms(key, ttl.as_millis() as i64)
    }

    /// Release a concurrency lock. Simple mock: always succeeds.
    pub fn release_lock(&mut self, _key: &str) -> Option<String> {
        None
    }

    /// Make an HTTP fetch request. Returns a default FetchResult with status 200.
    pub fn cleat_fetch(&mut self, _method: &str, _url: &str, _headers: &str, _body: &str) -> Result<FetchResult, String> {
        Ok(FetchResult {
            status: 200,
            headers: std::collections::HashMap::new(),
            body: String::new(),
        })
    }

    /// Generate a deterministic UUID from a seed.
    pub fn uuid(&mut self, seed: &str) -> String {
        format!("mock-uuid-{}", seed)
    }

    /// Register a query handler.
    pub fn register_query_handler(&mut self, name: &str) -> Result<(), String> {
        self.query_handlers.push(name.to_string());
        Ok(())
    }

    /// Run a workflow in detached mode.
    pub fn run_detached(&mut self, _name: &str, _input_json: &str) -> Result<(), String> {
        Ok(())
    }

    /// Set a state value.
    pub fn set_state(&mut self, key: &str, value: &str) -> Result<(), String> {
        self.workflow_state.insert(self.scoped_key(key), value.to_string());
        Ok(())
    }

    /// Get a state value.
    pub fn get_state(&self, key: &str) -> Result<String, String> {
        let scoped = self.scoped_key(key);
        self.workflow_state
            .get(&scoped)
            .cloned()
            .ok_or_else(|| format!("key not found: {}", key))
    }

    /// Delete a state key.
    pub fn delete_state(&mut self, key: &str) -> Result<(), String> {
        self.workflow_state.remove(&self.scoped_key(key));
        Ok(())
    }

    /// Atomically increment a state counter.
    pub fn incr_state(&mut self, key: &str, delta: i64) -> Result<i64, String> {
        let scoped = self.scoped_key(key);
        let current = self.workflow_state
            .get(&scoped)
            .and_then(|v| v.parse::<i64>().ok())
            .unwrap_or(0);
        let new_val = current + delta;
        self.workflow_state.insert(scoped, new_val.to_string());
        Ok(new_val)
    }

    /// Check if a state key exists.
    pub fn has_state(&self, key: &str) -> bool {
        self.workflow_state.contains_key(&self.scoped_key(key))
    }

    /// List state keys with a given prefix.
    pub fn list_state(&self, prefix: &str) -> Result<Vec<String>, String> {
        let scoped = self.scoped_key(prefix);
        Ok(self.workflow_state
            .keys()
            .filter(|k| k.starts_with(&scoped))
            .cloned()
            .collect())
    }

    /// Await all children workflows.
    pub fn await_all_children(&self, _run_ids: &[&str]) -> Result<String, String> {
        // Build a simplified JSON array of child results
        let results: Vec<String> = self.child_results
            .iter()
            .map(|(run_id, result)| {
                format!(r#"{{"runId":"{}","result":{}}}"#, run_id, result)
            })
            .collect();
        Ok(format!("[{}]", results.join(",")))
    }

    /// Send a signal to a target workflow and wait for a response.
    pub fn send_signal_and_wait(&mut self, target_run_id: &str, signal_name: &str, payload: &str, timeout_ms: i64) -> Result<String, String> {
        self.signal_reply_corr_id_counter += 1;
        let correlation_id = format!("corr-{}-{}-{}", target_run_id, signal_name, self.signal_reply_corr_id_counter);

        // Register a reply channel
        self.signal_reply_channels.insert(correlation_id.clone(), "__pending__".to_string());

        // Send the signal
        let _ = self.signal_workflow(target_run_id, signal_name, payload);

        // Check if reply was already sent
        if let Some(reply) = self.signal_reply_channels.get(&correlation_id) {
            if reply != "__pending__" {
                let resp = reply.clone();
                self.signal_reply_channels.remove(&correlation_id);
                return Ok(resp);
            }
        }

        // Simulate timeout
        self.now_ms += timeout_ms;
        Err(format!("SendSignalAndWait(target={}, signal={}) timed out after {}ms", target_run_id, signal_name, timeout_ms))
    }

    /// Send signal and wait (ms variant, alias for send_signal_and_wait).
    pub fn send_signal_and_wait_ms(&mut self, target_run_id: &str, signal_name: &str, payload: &str, timeout_ms: i64) -> Result<String, String> {
        self.send_signal_and_wait(target_run_id, signal_name, payload, timeout_ms)
    }

    /// Reply to a signal from within a handler.
    pub fn reply_to_signal(&mut self, correlation_id: &str, response: &str) -> Result<(), String> {
        if let Some(ch) = self.signal_reply_channels.get_mut(correlation_id) {
            *ch = response.to_string();
            Ok(())
        } else {
            Err(format!("no pending signal for correlation ID: {}", correlation_id))
        }
    }

    /// Send a signal to a target workflow (fire-and-forget).
    pub fn signal_workflow(&mut self, _target_run_id: &str, signal_name: &str, payload: &str) -> Result<(), String> {
        self.sent_signals.push(format!("{}:{}", _target_run_id, signal_name));
        self.pending_signals.push(PendingSignal {
            name: signal_name.to_string(),
            payload: payload.to_string(),
        });
        Ok(())
    }

    /// Set virtual object scope.
    pub fn set_scope(&mut self, object_type: &str, instance_key: &str) -> String {
        let prev = self.scope_prefix.clone();
        self.scope_prefix = if !object_type.is_empty() && !instance_key.is_empty() {
            format!("vo:{}:{}:", object_type, instance_key)
        } else {
            String::new()
        };
        prev
    }

    /// Get the current scope.
    pub fn get_scope(&self) -> (String, String) {
        if self.scope_prefix.is_empty() {
            return (String::new(), String::new());
        }
        let trimmed = self.scope_prefix.trim_end_matches(':');
        let parts: Vec<&str> = trimmed.splitn(3, ':').collect();
        if parts.len() == 3 && parts[0] == "vo" {
            (parts[1].to_string(), parts[2].to_string())
        } else {
            (String::new(), String::new())
        }
    }

    /// Clear the current scope.
    pub fn clear_scope(&mut self) -> String {
        let prev = self.scope_prefix.clone();
        self.scope_prefix = String::new();
        prev
    }

    // ────────────────────────────────────────────
    // Private helpers
    // ────────────────────────────────────────────

    fn scoped_key(&self, key: &str) -> String {
        if self.scope_prefix.is_empty() {
            key.to_string()
        } else {
            format!("{}{}", self.scope_prefix, key)
        }
    }

    // ────────────────────────────────────────────
    // Mock-specific configuration
    // ────────────────────────────────────────────

    /// Register a pre-programmed response for a call to `service.operation`.
    pub fn register_call_stub(&mut self, service: &str, operation: &str, response: &str) {
        self.call_stubs.push(CallStub {
            service: service.to_string(),
            operation: operation.to_string(),
            response: response.to_string(),
            error: None,
        });
    }

    /// Register a pre-programmed error for a call to `service.operation`.
    pub fn register_call_error(&mut self, service: &str, operation: &str, error: &str) {
        self.call_stubs.push(CallStub {
            service: service.to_string(),
            operation: operation.to_string(),
            response: String::new(),
            error: Some(error.to_string()),
        });
    }

    /// Register a stub for a child workflow with the given name.
    pub fn register_child_workflow_stub(&mut self, name: &str, result: &str, error: Option<&str>) {
        self.child_workflow_stubs.insert(
            name.to_string(),
            ChildWorkflowStub {
                result: result.to_string(),
                error: error.map(|e| e.to_string()),
            },
        );
    }

    /// Pre-set the result that a child workflow run will return.
    pub fn register_child_result(&mut self, run_id: &str, result: &str, error: Option<&str>) {
        if let Some(e) = error {
            self.child_errors.insert(run_id.to_string(), e.to_string());
        } else {
            self.child_results.insert(run_id.to_string(), result.to_string());
        }
    }

    /// Register a plugin call stub.
    pub fn register_plugin_call_stub(&mut self, plugin_name: &str, function_name: &str, result: &str) {
        self.plugin_call_stubs.push(PluginCallStub {
            plugin_name: plugin_name.to_string(),
            function_name: function_name.to_string(),
            result: result.to_string(),
            error: None,
        });
    }

    /// Deliver a signal immediately.
    pub fn deliver_signal(&mut self, name: &str, payload: &str) {
        self.pending_signals.push(PendingSignal {
            name: name.to_string(),
            payload: payload.to_string(),
        });
    }

    /// Configure the random value sequence.
    pub fn set_random_seq(&mut self, seq: Vec<i64>) {
        self.random_seq = seq;
        self.random_idx = 0;
    }

    /// Set the simulated clock time.
    pub fn set_time(&mut self, ms: i64) {
        self.now_ms = ms;
    }

    /// Advance the simulated clock.
    pub fn advance_time(&mut self, ms: i64) {
        self.now_ms += ms;
    }

    /// Set the workflow version.
    pub fn set_version(&mut self, v: i32) {
        self.version_val = v;
    }

    /// Set the minimum workflow version.
    pub fn set_min_version(&mut self, v: i32) {
        self.min_version_val = v;
    }

    /// Configure cancellation simulation.
    pub fn set_cancelled(&mut self, cancelled: bool, reason: &str) {
        self.cancelled = cancelled;
        self.cancel_reason = reason.to_string();
    }

    /// Configure retry simulation: fail the first `n` calls per (service, operation).
    pub fn set_retry_simulation(&mut self, n: u32) {
        self.retry_sim_count = n;
        self.retry_sim_attempts.clear();
    }

    /// Set the workflow ID returned by `workflow_id()`.
    pub fn set_workflow_id(&mut self, id: &str) {
        self.workflow_id = id.to_string();
    }

    /// Set the run ID returned by `run_id()`.
    pub fn set_run_id(&mut self, id: &str) {
        self.run_id = id.to_string();
    }

    // ────────────────────────────────────────────
    // Query helpers
    // ────────────────────────────────────────────

    /// Read a query state value set via `set_query_state`.
    pub fn read_query_state(&self, key: &str) -> Option<&String> {
        self.query_state.get(key)
    }

    // ────────────────────────────────────────────
    // Assertion helpers
    // ────────────────────────────────────────────

    /// Check if a call to the given service+operation was recorded.
    pub fn call_count(&self, service: &str, operation: &str) -> usize {
        self.call_history
            .iter()
            .filter(|rec| rec.service == service && rec.operation == operation)
            .count()
    }

    /// Get the call history.
    pub fn get_call_history(&self) -> &[CallRecord] {
        &self.call_history
    }

    /// Clear all recorded calls, stubs, signals, and state.
    pub fn reset(&mut self) {
        self.call_stubs.clear();
        self.call_history.clear();
        self.pending_signals.clear();
        self.query_state.clear();
        self.workflow_state.clear();
        self.random_seq.clear();
        self.random_idx = 0;
        self.defer_counter = 0;
        self.child_workflow_stubs.clear();
        self.child_results.clear();
        self.child_errors.clear();
        self.plugin_call_stubs.clear();
        self.promises.clear();
        self.signal_reply_channels.clear();
        self.signal_reply_corr_id_counter = 0;
        self.update_handlers.clear();
        self.query_handlers.clear();
        self.sent_signals.clear();
        self.scheduled_invocations.clear();
        self.cancelled = false;
        self.cancel_reason.clear();
        self.retry_sim_count = 0;
        self.retry_sim_attempts.clear();
        self.continue_as_new_called = false;
        self.now_ms = 1704067200000;
        self.version_val = 1;
        self.min_version_val = 1;
        self.child_run_id_counter = 0;
        self.workflow_id = "test-workflow".to_string();
        self.run_id = "test-run-001".to_string();
        self.scope_prefix.clear();
    }
}

impl Default for MockHostCalls {
    fn default() -> Self {
        Self::new()
    }
}

// ═════════════════════════════════════════════════════════════════════════════
// CleatTest — high-level test environment orchestrator
// ═════════════════════════════════════════════════════════════════════════════

/// High-level test environment for cleat workflows, mirroring Go's
/// `durabletest.TestEnv`.
///
/// Wraps a [`MockHostCalls`] and provides convenient methods for stub
/// registration, signal delivery, workflow execution, and assertions.
///
/// # Usage
///
/// ```rust
/// use cleat_sdk::test::CleatTest;
///
/// #[test]
/// fn test_workflow() {
///     let mut env = CleatTest::new();
///
///     // Stub an external service
///     env.register_call_stub("payment", "charge", r#"{"id":"ch_123"}"#);
///
///     // Run workflow
///     let result = env.run_workflow(
///         |host: &mut MockHostCalls, input: &str| -> String {
///             let (resp, err) = host.cleat_call("payment", "charge", input);
///             if let Some(e) = err {
///                 return format!("{{ \"error\": \"{}\" }}", e);
///             }
///             format!("{{ \"result\": {} }}", resp)
///         },
///         r#"{"amount": 5000}"#,
///     );
///
///     // Assert
///     assert!(env.assert_called("payment", "charge"));
///     assert!(env.assert_not_called("shipping", "ship"));
/// }
/// ```
#[derive(Debug, Clone)]
pub struct CleatTest {
    /// The underlying MockHostCalls instance.
    pub mock: MockHostCalls,
}

impl CleatTest {
    /// Create a new CleatTest with clean state.
    pub fn new() -> Self {
        Self {
            mock: MockHostCalls::new(),
        }
    }

    // ────────────────────────────────────────────
    // Stub registration
    // ────────────────────────────────────────────

    /// Register a stub response for a call to `service.operation`.
    pub fn register_call_stub(&mut self, service: &str, operation: &str, response: &str) {
        self.mock.register_call_stub(service, operation, response);
    }

    /// Register a stub error for a call to `service.operation`.
    pub fn register_call_error(&mut self, service: &str, operation: &str, error: &str) {
        self.mock.register_call_error(service, operation, error);
    }

    /// Register a stub for a child workflow.
    pub fn register_child_workflow_stub(&mut self, name: &str, result: &str) {
        self.mock.register_child_workflow_stub(name, result, None);
    }

    /// Pre-set a child workflow run result.
    pub fn register_child_result(&mut self, run_id: &str, result: &str) {
        self.mock.register_child_result(run_id, result, None);
    }

    /// Register a plugin call stub.
    pub fn register_plugin_call_stub(&mut self, plugin_name: &str, function_name: &str, result: &str) {
        self.mock.register_plugin_call_stub(plugin_name, function_name, result);
    }

    // ────────────────────────────────────────────
    // Signal delivery
    // ────────────────────────────────────────────

    /// Deliver a signal immediately.
    pub fn deliver_signal(&mut self, name: &str, payload: &str) {
        self.mock.deliver_signal(name, payload);
    }

    // ────────────────────────────────────────────
    // Time management
    // ────────────────────────────────────────────

    /// Set the simulated clock time.
    pub fn set_time(&mut self, ms: i64) {
        self.mock.set_time(ms);
    }

    /// Advance the simulated clock.
    pub fn advance_time(&mut self, ms: i64) {
        self.mock.advance_time(ms);
    }

    /// Get the current simulated time.
    pub fn now(&self) -> i64 {
        self.mock.now()
    }

    // ────────────────────────────────────────────
    // Configuration
    // ────────────────────────────────────────────

    /// Set the workflow version.
    pub fn set_version(&mut self, v: i32) {
        self.mock.set_version(v);
    }

    /// Set the minimum workflow version.
    pub fn set_min_version(&mut self, v: i32) {
        self.mock.set_min_version(v);
    }

    /// Configure the random value sequence.
    pub fn set_random_seq(&mut self, seq: Vec<i64>) {
        self.mock.set_random_seq(seq);
    }

    /// Configure retry simulation.
    pub fn set_retry_simulation(&mut self, n: u32) {
        self.mock.set_retry_simulation(n);
    }

    /// Configure cancellation simulation.
    pub fn set_cancelled(&mut self, cancelled: bool, reason: &str) {
        self.mock.set_cancelled(cancelled, reason);
    }

    // ────────────────────────────────────────────
    // Workflow runner
    // ────────────────────────────────────────────

    /// Run a workflow function with the mock HostCalls.
    ///
    /// The workflow closure receives a `&mut MockHostCalls` and the input
    /// string, and returns a result string.
    pub fn run_workflow<F>(&mut self, entry_fn: F, input: &str) -> String
    where
        F: FnOnce(&mut MockHostCalls, &str) -> String,
    {
        entry_fn(&mut self.mock, input)
    }

    // ────────────────────────────────────────────
    // Assertions
    // ────────────────────────────────────────────

    /// Assert that a call to the given service+operation was made.
    pub fn assert_called(&self, service: &str, operation: &str) -> bool {
        self.mock.call_count(service, operation) > 0
    }

    /// Assert that a call to the given service+operation was NOT made.
    pub fn assert_not_called(&self, service: &str, operation: &str) -> bool {
        self.mock.call_count(service, operation) == 0
    }

    /// Assert that a call was made exactly `expected` times.
    pub fn assert_call_count(&self, service: &str, operation: &str, expected: usize) -> bool {
        self.mock.call_count(service, operation) == expected
    }

    /// Assert that a workflow state key has the given value.
    pub fn assert_state(&self, key: &str, value: &str) -> bool {
        self.mock.get_state(key).map_or(false, |v| v == value)
    }

    /// Assert that a signal with the given name was delivered.
    pub fn assert_signal_delivered(&self, signal_name: &str) -> bool {
        self.mock.sent_signals.iter().any(|s| s.contains(&format!(":{}", signal_name)))
    }

    // ────────────────────────────────────────────
    // Query helpers
    // ────────────────────────────────────────────

    /// Read a query state value set by the workflow.
    pub fn read_query_state(&self, key: &str) -> Option<&String> {
        self.mock.read_query_state(key)
    }

    /// Get the full call history.
    pub fn get_call_history(&self) -> &[CallRecord] {
        self.mock.get_call_history()
    }

    /// Get the number of times a specific call was made.
    pub fn call_count(&self, service: &str, operation: &str) -> usize {
        self.mock.call_count(service, operation)
    }

    /// Reset the entire test environment.
    pub fn reset(&mut self) {
        self.mock.reset();
    }
}

impl Default for CleatTest {
    fn default() -> Self {
        Self::new()
    }
}

// ═════════════════════════════════════════════════════════════════════════════
// Tests for the test harness itself
// ═════════════════════════════════════════════════════════════════════════════

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_call_stub_and_recording() {
        let mut env = CleatTest::new();
        env.register_call_stub("payment", "charge", r#"{"id":"ch_123"}"#);

        let (resp, err) = env.mock.cleat_call("payment", "charge", r#"{"amount":5000}"#);
        assert!(err.is_none());
        assert_eq!(resp, r#"{"id":"ch_123"}"#);
        assert_eq!(env.mock.call_count("payment", "charge"), 1);
    }

    #[test]
    fn test_call_error_stub() {
        let mut env = CleatTest::new();
        env.register_call_error("payment", "charge", "service unavailable");

        let (resp, err) = env.mock.cleat_call("payment", "charge", r#"{"amount":5000}"#);
        assert!(resp.is_empty());
        assert_eq!(err.unwrap(), "service unavailable");
    }

    #[test]
    fn test_no_stub_error() {
        let mut env = CleatTest::new();
        let (resp, err) = env.mock.cleat_call("unknown", "op", r#"{}"#);
        assert!(resp.is_empty());
        assert!(err.unwrap().contains("no stub registered"));
    }

    #[test]
    fn test_assert_called() {
        let mut env = CleatTest::new();
        env.register_call_stub("svc", "op", r#"{}"#);
        env.mock.cleat_call("svc", "op", r#"{}"#);
        assert!(env.assert_called("svc", "op"));
        assert!(env.assert_not_called("other", "op"));
    }

    #[test]
    fn test_signal_delivery() {
        let mut env = CleatTest::new();
        env.deliver_signal("order_placed", r#"{"orderId":"ord_1"}"#);

        let (payload, found, err) = env.mock.poll_signal("order_placed");
        assert!(found);
        assert!(err.is_none());
        assert_eq!(payload, r#"{"orderId":"ord_1"}"#);
    }

    #[test]
    fn test_await_signals() {
        let mut env = CleatTest::new();
        env.deliver_signal("payment_received", r#"{"amount":5000}"#);

        let (name, payload, timed_out, err) = env.mock.await_signals(&["payment_received"], 1000);
        assert!(!timed_out);
        assert!(err.is_none());
        assert_eq!(name, "payment_received");
        assert_eq!(payload, r#"{"amount":5000}"#);
    }

    #[test]
    fn test_await_signals_timeout() {
        let mut env = CleatTest::new();
        let (name, payload, timed_out, err) = env.mock.await_signals(&["nonexistent"], 100);
        assert!(timed_out);
        assert!(err.is_none());
        assert!(name.is_empty());
        assert!(payload.is_empty());
    }

    #[test]
    fn test_sleep_advances_time() {
        let mut env = CleatTest::new();
        let before = env.mock.now();
        env.mock.cleat_sleep(5000);
        let after = env.mock.now();
        assert_eq!(after - before, 5000);
    }

    #[test]
    fn test_random_sequence() {
        let mut env = CleatTest::new();
        env.set_random_seq(vec![42, 99, 7]);

        assert_eq!(env.mock.random(), 42);
        assert_eq!(env.mock.random(), 99);
        assert_eq!(env.mock.random(), 7);
        assert_eq!(env.mock.random(), 0); // exhausted
    }

    #[test]
    fn test_version() {
        let mut env = CleatTest::new();
        assert_eq!(env.mock.version(), 1);
        env.set_version(3);
        assert_eq!(env.mock.version(), 3);
    }

    #[test]
    fn test_promise_workflow() {
        let mut env = CleatTest::new();
        let (prom_id, err) = env.mock.create_promise("test-promise");
        assert!(err.is_none());
        assert!(prom_id.contains("test-promise"));

        // Promise is pending initially
        let (_val, timed_out, err) = env.mock.await_promise(&prom_id, 100);
        assert!(timed_out);
        assert!(err.is_none());

        // Resolve and await
        env.mock.resolve_promise(&prom_id, r#"{"status":"done"}"#).unwrap();
        let (val, timed_out, err) = env.mock.await_promise(&prom_id, 100);
        assert!(!timed_out);
        assert!(err.is_none());
        assert_eq!(val, r#"{"status":"done"}"#);
    }

    #[test]
    fn test_child_workflow() {
        let mut env = CleatTest::new();
        env.mock.register_child_workflow_stub("inventory_check", r#"{"available":true}"#, None);

        let (run_id, err) = env.mock.child_workflow("inventory_check", r#"{"sku":"s1"}"#);
        assert!(err.is_none());
        assert!(run_id.contains("inventory_check"));

        let (result, err) = env.mock.await_child(&run_id);
        assert!(err.is_none());
        assert_eq!(result, r#"{"available":true}"#);
    }

    #[test]
    fn test_retry_simulation() {
        let mut env = CleatTest::new();
        env.register_call_stub("payment", "charge", r#"{"id":"ch_123"}"#);
        env.mock.set_retry_simulation(2);

        // First two calls should fail
        let (resp1, err1) = env.mock.cleat_call("payment", "charge", r#"{"amount":5000}"#);
        assert!(resp1.is_empty());
        assert!(err1.unwrap().contains("simulated transient failure"));

        let (resp2, err2) = env.mock.cleat_call("payment", "charge", r#"{"amount":5000}"#);
        assert!(resp2.is_empty());
        assert!(err2.unwrap().contains("simulated transient failure"));

        // Third call should succeed
        let (resp3, err3) = env.mock.cleat_call("payment", "charge", r#"{"amount":5000}"#);
        assert!(err3.is_none());
        assert_eq!(resp3, r#"{"id":"ch_123"}"#);
    }

    #[test]
    fn test_plugin_call() {
        let mut env = CleatTest::new();
        env.mock.register_plugin_call_stub("blobstore", "get", r#"{"data":"hello"}"#);

        let (resp, err) = env.mock.plugin_call("blobstore", "get", r#"{"key":"test"}"#);
        assert!(err.is_none());
        assert_eq!(resp, r#"{"data":"hello"}"#);
    }

    #[test]
    fn test_workflow_state() {
        let mut env = CleatTest::new();
        env.mock.set_state("my_key", "my_value").unwrap();

        let val = env.mock.get_state("my_key").unwrap();
        assert_eq!(val, "my_value");
        assert!(env.mock.has_state("my_key"));
        assert!(!env.mock.has_state("nonexistent"));

        env.mock.delete_state("my_key").unwrap();
        assert!(!env.mock.has_state("my_key"));
    }

    #[test]
    fn test_signal_workflow_and_signal() {
        let mut env = CleatTest::new();
        env.mock.signal_workflow("target-run", "my_signal", r#"{"msg":"hello"}"#).unwrap();

        let (payload, found, _err) = env.mock.poll_signal("my_signal");
        assert!(found);
        assert_eq!(payload, r#"{"msg":"hello"}"#);
    }

    #[test]
    fn test_async_signals_with_quorum() {
        let mut env = CleatTest::new();
        env.deliver_signal("vote_1", r#"{"approved":true}"#);
        env.deliver_signal("vote_2", r#"{"approved":true}"#);

        let signals = &["vote_1", "vote_2", "vote_3"];
        let (name, _payload, timed_out, err) = env.mock.await_signals(signals, 5000);
        assert!(!timed_out);
        assert!(err.is_none());
        assert_eq!(name, "vote_1");
    }

    #[test]
    fn test_scope_and_state() {
        let mut env = CleatTest::new();
        let prev = env.mock.set_scope("counter", "user_42");
        assert!(prev.is_empty());

        env.mock.set_state("count", "10").unwrap();
        let val = env.mock.get_state("count").unwrap();
        assert_eq!(val, "10");

        // Verify scoped key
        let (obj_type, inst_key) = env.mock.get_scope();
        assert_eq!(obj_type, "counter");
        assert_eq!(inst_key, "user_42");

        env.mock.clear_scope();
        let (obj_type2, _) = env.mock.get_scope();
        assert!(obj_type2.is_empty());
    }

    #[test]
    fn test_send_signal_and_wait() {
        let mut env = CleatTest::new();

        // Send signal and wait — will reply via reply_to_signal
        let corr_id = "corr-target-test-1";
        env.mock.signal_reply_channels.insert(corr_id.to_string(), "__pending__".to_string());

        // Simulate the target replying
        env.mock.reply_to_signal(corr_id, r#"{"status":"ok"}"#).unwrap();

        // Now check the channel has the reply
        assert_eq!(
            env.mock.signal_reply_channels.get(corr_id).unwrap(),
            r#"{"status":"ok"}"#
        );
    }

    #[test]
    fn test_run_workflow_closure() {
        let mut env = CleatTest::new();
        env.register_call_stub("greeter", "hello", r#"{"greeting":"Hello, World!"}"#);

        let result = env.run_workflow(
            |host: &mut MockHostCalls, input: &str| -> String {
                let (resp, _err) = host.cleat_call("greeter", "hello", input);
                resp
            },
            r#"{"name":"test"}"#,
        );

        assert_eq!(result, r#"{"greeting":"Hello, World!"}"#);
    }

    #[test]
    fn test_assert_state() {
        let mut env = CleatTest::new();
        env.mock.set_state("key1", "value1").unwrap();

        assert!(env.assert_state("key1", "value1"));
        assert!(!env.assert_state("key1", "wrong"));
        assert!(!env.assert_state("nonexistent", "anything"));
    }

    #[test]
    fn test_reset() {
        let mut env = CleatTest::new();
        env.register_call_stub("svc", "op", r#"{}"#);
        env.mock.cleat_call("svc", "op", r#"{}"#);
        assert_eq!(env.call_count("svc", "op"), 1);

        env.reset();
        assert_eq!(env.call_count("svc", "op"), 0);
    }

    #[test]
    fn test_cleat_send_recording() {
        let mut env = CleatTest::new();
        env.mock.cleat_send("notification", "email", r#"{"to":"user@example.com"}"#).unwrap();

        assert_eq!(env.call_count("notification", "email"), 1);
    }

    #[test]
    fn test_incr_state() {
        let mut env = CleatTest::new();

        let val = env.mock.incr_state("counter", 5).unwrap();
        assert_eq!(val, 5);

        let val2 = env.mock.incr_state("counter", 3).unwrap();
        assert_eq!(val2, 8);
    }

    #[test]
    fn test_list_state() {
        let mut env = CleatTest::new();
        env.mock.set_state("a:1", "v1").unwrap();
        env.mock.set_state("a:2", "v2").unwrap();
        env.mock.set_state("b:1", "v3").unwrap();

        let keys = env.mock.list_state("a:").unwrap();
        assert_eq!(keys.len(), 2);
    }

    #[test]
    fn test_continue_as_new() {
        let mut env = CleatTest::new();
        let err = env.mock.continue_as_new(r#"{"restart":true}"#);
        assert!(err.is_none());
        assert!(env.mock.continue_as_new_called);
    }

    #[test]
    fn test_assert_signal_delivered() {
        let mut env = CleatTest::new();
        env.mock.signal_workflow("target", "my_sig", r#"{}"#).unwrap();

        assert!(env.assert_signal_delivered("my_sig"));
        assert!(!env.assert_signal_delivered("other_sig"));
    }

    #[test]
    fn test_set_workflow_id() {
        let mut env = CleatTest::new();
        env.mock.set_workflow_id("custom-wf");
        assert_eq!(env.mock.workflow_id(), "custom-wf");
    }
}
