//! Test environment for cleat workflows — mock HostCalls for deterministic testing
//! without WASM compilation.
//!
//! # Quick start
//!
//! ```rust,ignore
//! use cleat_test::TestEnv;
//!
//! #[cleat_test]
//! fn test_my_workflow() {
//!     let env = TestEnv::new();
//!
//!     // Stub a durable call
//!     env.expect_call("payment", "charge", r#"{"amount":5000}"#)
//!         .respond(r#"{"id":"ch_123","status":"succeeded"}"#);
//!
//!     // Inject a signal
//!     env.inject_signal("order_confirmed", r#"{"orderId":"ord_1"}"#);
//!
//!     // Run the workflow
//!     let result = env.execute(my_workflow_fn, r#"{"userId":"u1"}"#);
//!
//!     // Assert calls were made
//!     assert!(env.assert_called("payment", "charge"));
//!     assert_eq!(env.call_count("payment", "charge"), 1);
//! }
//!
//! fn my_workflow_fn(env: &TestEnv, input: &str) -> String {
//!     let (resp, err) = env.cleat_call("payment", "charge", input);
//!     if let Some(e) = err { return format!("{{ \"error\": \"{}\" }}", e); }
//!     resp
//! }
//! ```
//!
//! # Integration with `#[cleat_test]`
//!
//! The `#[cleat_test]` proc macro (re-exported from `cleat_macro`) wraps the test
//! body in `std::panic::catch_unwind`. If a host method inside the workflow panics
//! with [`SuspendSentinel`] (e.g. because `await_child` or `await_signals` would
//! have suspended on a fresh execution), the macro catches it and returns
//! `Ok(())` / `()` so the test does not crash.
//!
//! In the `cleat-test` crate the [`SuspendSentinel`] panic is never actually
//! thrown — the mock `await_signals` / `await_child` / etc. return immediately
//! (or after advancing simulated time) instead of panicking. However, if you
//! write a workflow helper that calls `HostCalls` methods directly and those
//! methods are real WASM imports (not mocked), the sentinel-catching wrapper
//! from `#[cleat_test]` prevents a confusing crash.
//!
//! # Re-exports
//!
//! For convenience this crate re-exports `#[cleat_test]` from `cleat_macro`.
//! You can either:
//!
//! ```rust,ignore
//! use cleat_test::cleat_test;
//!
//! #[cleat_test]
//! fn test() { ... }
//! ```
//!
//! or import it from `cleat_macro`:
//!
//! ```rust,ignore
//! use cleat_macro::cleat_test;
//! ```

pub use cleat_macro::cleat_test;

use std::cell::RefCell;
use std::collections::HashMap;
use std::time::Duration;

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

/// A single recorded invocation through the test environment.
#[derive(Debug, Clone)]
pub struct RecordedCall {
    pub service: String,
    pub operation: String,
    pub request: String,
    pub response: String,
    pub error: Option<String>,
}

/// Result of an `await_signals` call.
#[derive(Debug, Clone)]
pub struct SignalResult {
    pub name: String,
    pub payload: String,
    pub timed_out: bool,
    pub error: Option<String>,
}

/// Result from a simulated HTTP fetch.
#[derive(Debug, Clone)]
pub struct FetchResult {
    pub status: u16,
    pub headers: HashMap<String, String>,
    pub body: String,
}

// ---------------------------------------------------------------------------
// Internal types
// ---------------------------------------------------------------------------

#[derive(Clone)]
enum RequestMatcher {
    Any,
    Exact(String),
}

#[derive(Clone)]
struct CallStub {
    service: String,
    operation: String,
    request_matcher: RequestMatcher,
    response: String,
    error_msg: Option<String>,
}

#[derive(Clone)]
struct PluginCallStub {
    plugin: String,
    function: String,
    response: String,
    error: Option<String>,
}

#[derive(Clone)]
struct PendingSignal {
    name: String,
    payload: String,
    deliver_at_ms: i64,
}

#[derive(Clone)]
struct ChildWorkflowStub {
    result: String,
    error: Option<String>,
}

#[derive(Clone)]
enum PromiseState {
    Pending,
    Resolved(String),
    Rejected(String),
}

// ---------------------------------------------------------------------------
// ExpectCallBuilder — builder-pattern stub registration
// ---------------------------------------------------------------------------

/// Builder returned by [`TestEnv::expect_call`].
///
/// Configure the mock response with [`respond`](ExpectCallBuilder::respond)
/// or [`respond_error`](ExpectCallBuilder::respond_error).
pub struct ExpectCallBuilder<'a> {
    env: &'a TestEnv,
    service: String,
    operation: String,
    request_matcher: RequestMatcher,
}

impl<'a> ExpectCallBuilder<'a> {
    /// Register a successful response for this call stub.
    ///
    /// The returned reference allows chaining further `expect_call` calls on
    /// the same `TestEnv`.
    pub fn respond(self, response: &str) -> &'a TestEnv {
        self.env.inner.borrow_mut().call_stubs.push(CallStub {
            service: self.service,
            operation: self.operation,
            request_matcher: self.request_matcher,
            response: response.to_string(),
            error_msg: None,
        });
        self.env
    }

    /// Register an error response for this call stub.
    pub fn respond_error(self, error: &str) -> &'a TestEnv {
        self.env.inner.borrow_mut().call_stubs.push(CallStub {
            service: self.service,
            operation: self.operation,
            request_matcher: self.request_matcher,
            response: String::new(),
            error_msg: Some(error.to_string()),
        });
        self.env
    }
}

/// Builder returned by [`TestEnv::expect_plugin_call`].
pub struct ExpectPluginCallBuilder<'a> {
    env: &'a TestEnv,
    plugin: String,
    function: String,
}

impl<'a> ExpectPluginCallBuilder<'a> {
    /// Register a successful response for this plugin call stub.
    pub fn respond(self, response: &str) -> &'a TestEnv {
        self.env.inner.borrow_mut().plugin_call_stubs.push(PluginCallStub {
            plugin: self.plugin,
            function: self.function,
            response: response.to_string(),
            error: None,
        });
        self.env
    }

    /// Register an error response for this plugin call stub.
    pub fn respond_error(self, error: &str) -> &'a TestEnv {
        self.env.inner.borrow_mut().plugin_call_stubs.push(PluginCallStub {
            plugin: self.plugin,
            function: self.function,
            response: String::new(),
            error: Some(error.to_string()),
        });
        self.env
    }
}

// ---------------------------------------------------------------------------
// TestEnv state
// ---------------------------------------------------------------------------

struct TestEnvInner {
    // Call recording
    call_history: Vec<RecordedCall>,
    call_stubs: Vec<CallStub>,
    plugin_call_stubs: Vec<PluginCallStub>,

    // Signals
    pending_signals: Vec<PendingSignal>,

    // Child workflows
    child_workflow_stubs: HashMap<String, ChildWorkflowStub>,
    child_results: HashMap<String, String>,
    child_errors: HashMap<String, String>,
    child_run_id_counter: u64,

    // Simulated time
    now_ms: i64,

    // Version
    version_val: i32,
    min_version_val: i32,

    // Random
    random_seq: Vec<i64>,
    random_idx: usize,

    // State
    query_state: HashMap<String, String>,
    workflow_state: HashMap<String, String>,
    scope_prefix: String,

    // Cancellation
    cancelled: bool,
    cancel_reason: String,

    // Defer
    defer_counter: u64,

    // Continue-as-new
    continue_as_new_called: bool,

    // Workflow metadata
    workflow_id: String,
    run_id: String,

    // Promises
    promises: HashMap<String, PromiseState>,

    // Signal reply (SendSignalAndWait / ReplyToSignal)
    signal_reply_channels: HashMap<String, String>,
    signal_reply_corr_counter: u64,

    // Sent signals (for assertion)
    sent_signals: Vec<String>,

    // Retry simulation
    retry_sim_count: u32,
    retry_sim_attempts: HashMap<String, u32>,

    // Plugin call recording
    plugin_call_history: Vec<RecordedCall>,

    // Scheduling
    scheduled_invocations: Vec<String>,
}

// ---------------------------------------------------------------------------
// TestEnv
// ---------------------------------------------------------------------------

/// Mock test environment for cleat workflows.
///
/// Wraps the WASM host import ABI in a deterministic, in-memory mock that can
/// be used with `cargo test` — no WASM compilation required.
///
/// All public methods take `&self` (interior mutability via `RefCell`), so the
/// environment can be shared by the test function *and* passed into the workflow
/// closure without `&mut` plumbing.
///
/// # Usage
///
/// ```rust,ignore
/// use cleat_test::TestEnv;
///
/// #[cleat_test]
/// fn test_durable_call() {
///     let env = TestEnv::new();
///
///     env.expect_call("inventory", "reserve", r#"{"sku":"S42"}"#)
///        .respond(r#"{"reserved":true}"#);
///
///     let output = env.execute(|h, input: &str| -> String {
///         let (resp, err) = h.cleat_call("inventory", "reserve", input);
///         resp
///     }, r#"{"sku":"S42"}"#);
///
///     assert_eq!(output, r#"{"reserved":true}"#);
///     assert!(env.assert_called("inventory", "reserve"));
/// }
/// ```
pub struct TestEnv {
    inner: RefCell<TestEnvInner>,
}

impl TestEnv {
    /// Create a new `TestEnv` with clean state.
    ///
    /// Simulated clock starts at `2024-01-01T00:00:00Z`.
    pub fn new() -> Self {
        Self {
            inner: RefCell::new(TestEnvInner {
                call_history: Vec::new(),
                call_stubs: Vec::new(),
                plugin_call_stubs: Vec::new(),
                pending_signals: Vec::new(),
                child_workflow_stubs: HashMap::new(),
                child_results: HashMap::new(),
                child_errors: HashMap::new(),
                child_run_id_counter: 0,
                now_ms: 1704067200000, // 2024-01-01T00:00:00Z
                version_val: 1,
                min_version_val: 1,
                random_seq: Vec::new(),
                random_idx: 0,
                query_state: HashMap::new(),
                workflow_state: HashMap::new(),
                scope_prefix: String::new(),
                cancelled: false,
                cancel_reason: String::new(),
                defer_counter: 0,
                continue_as_new_called: false,
                workflow_id: "test-workflow".to_string(),
                run_id: "test-run-001".to_string(),
                promises: HashMap::new(),
                signal_reply_channels: HashMap::new(),
                signal_reply_corr_counter: 0,
                sent_signals: Vec::new(),
                retry_sim_count: 0,
                retry_sim_attempts: HashMap::new(),
                plugin_call_history: Vec::new(),
                scheduled_invocations: Vec::new(),
            }),
        }
    }

    // -----------------------------------------------------------------------
    // Stub registration (builder pattern)
    // -----------------------------------------------------------------------

    /// Register a stub for a `cleat_call`.
    ///
    /// `request` is the *exact* JSON request string that must be matched.
    /// Use [`expect_any_call`](TestEnv::expect_any_call) to match any request.
    ///
    /// Returns an [`ExpectCallBuilder`] — call `.respond()` or `.respond_error()`
    /// to set the mock response.
    pub fn expect_call(&self, service: &str, operation: &str, request: &str) -> ExpectCallBuilder<'_> {
        ExpectCallBuilder {
            env: self,
            service: service.to_string(),
            operation: operation.to_string(),
            request_matcher: RequestMatcher::Exact(request.to_string()),
        }
    }

    /// Register a stub for a `cleat_call` that matches *any* request payload.
    ///
    /// Useful when the request payload is non-deterministic or you want a
    /// catch-all stub.
    pub fn expect_any_call(&self, service: &str, operation: &str) -> ExpectCallBuilder<'_> {
        ExpectCallBuilder {
            env: self,
            service: service.to_string(),
            operation: operation.to_string(),
            request_matcher: RequestMatcher::Any,
        }
    }

    /// Register a stub for a `plugin_call`.
    ///
    /// Returns an [`ExpectPluginCallBuilder`] — call `.respond()` or
    /// `.respond_error()` to set the mock response.
    pub fn expect_plugin_call(&self, plugin: &str, function: &str) -> ExpectPluginCallBuilder<'_> {
        ExpectPluginCallBuilder {
            env: self,
            plugin: plugin.to_string(),
            function: function.to_string(),
        }
    }

    // -----------------------------------------------------------------------
    // Signal injection
    // -----------------------------------------------------------------------

    /// Inject a signal immediately (at the current simulated time).
    pub fn inject_signal(&self, name: &str, payload: &str) {
        let now = {
            let inner = self.inner.borrow();
            inner.now_ms
        };
        self.inner.borrow_mut().pending_signals.push(PendingSignal {
            name: name.to_string(),
            payload: payload.to_string(),
            deliver_at_ms: now,
        });
    }

    /// Inject a signal that will become available after the given delay
    /// (from the current simulated time).
    pub fn inject_signal_delayed(&self, name: &str, payload: &str, delay: Duration) {
        let deliver_at = {
            let inner = self.inner.borrow();
            inner.now_ms + delay.as_millis() as i64
        };
        self.inner.borrow_mut().pending_signals.push(PendingSignal {
            name: name.to_string(),
            payload: payload.to_string(),
            deliver_at_ms: deliver_at,
        });
    }

    // -----------------------------------------------------------------------
    // Workflow execution
    // -----------------------------------------------------------------------

    /// Run a workflow function with this test environment.
    ///
    /// The workflow function receives `&TestEnv` and the input string, and
    /// returns whatever type `R` the caller specifies.
    pub fn execute<F, R>(&self, workflow_fn: F, input: &str) -> R
    where
        F: FnOnce(&TestEnv, &str) -> R,
    {
        workflow_fn(self, input)
    }

    // ===================================================================
    // HostCalls-compatible API — for use inside workflow closures
    // ===================================================================

    // -----------------------------------------------------------------------
    // Durable calls
    // -----------------------------------------------------------------------

    /// Make a durable API call. Returns `(response_json, error_message)`.
    pub fn cleat_call(&self, service: &str, operation: &str, request_json: &str) -> (String, Option<String>) {
        let sim_count = self.inner.borrow().retry_sim_count;

        // Retry simulation
        if sim_count > 0 {
            let key = format!("{}/{}", service, operation);
            let mut inner = self.inner.borrow_mut();
            let attempt = inner.retry_sim_attempts.entry(key.clone()).or_insert(0);
            if *attempt < sim_count {
                *attempt += 1;
                let err = format!(
                    "simulated transient failure for {} (attempt {}/{})",
                    key, *attempt, sim_count
                );
                inner.call_history.push(RecordedCall {
                    service: service.to_string(),
                    operation: operation.to_string(),
                    request: request_json.to_string(),
                    response: String::new(),
                    error: Some(err.clone()),
                });
                return (String::new(), Some(err));
            }
        }

        let mut inner = self.inner.borrow_mut();

        // Find first matching stub and consume it
        for i in 0..inner.call_stubs.len() {
            let matches = {
                let stub = &inner.call_stubs[i];
                stub.service == service
                    && stub.operation == operation
                    && match &stub.request_matcher {
                        RequestMatcher::Any => true,
                        RequestMatcher::Exact(expected) => expected == request_json,
                    }
            };
            if matches {
                let stub = inner.call_stubs.remove(i);
                let (resp, err) = if let Some(ref msg) = stub.error_msg {
                    (String::new(), Some(msg.clone()))
                } else {
                    (stub.response.clone(), None)
                };
                inner.call_history.push(RecordedCall {
                    service: service.to_string(),
                    operation: operation.to_string(),
                    request: request_json.to_string(),
                    response: resp.clone(),
                    error: err.clone(),
                });
                return (resp, err);
            }
        }

        // No stub matched
        let err = format!("cleat_test: no stub registered for {}.{} (request: {})", service, operation, request_json);
        inner.call_history.push(RecordedCall {
            service: service.to_string(),
            operation: operation.to_string(),
            request: request_json.to_string(),
            response: String::new(),
            error: Some(err.clone()),
        });
        (String::new(), Some(err))
    }

    /// Durable call with retry policy. Delegates to `cleat_call` in mock mode.
    pub fn cleat_call_with_retry(&self, _retry_policy: &RetryPolicy, service: &str, operation: &str, request_json: &str) -> (String, Option<String>) {
        self.cleat_call(service, operation, request_json)
    }

    /// Durable call with heartbeat. Delegates to `cleat_call` in mock mode.
    pub fn cleat_call_heartbeat(&self, service: &str, operation: &str, request_json: &str, _heartbeat_interval_ms: i64) -> (String, Option<String>) {
        self.cleat_call(service, operation, request_json)
    }

    /// Fire-and-forget durable send.
    pub fn cleat_send(&self, service: &str, operation: &str, request_json: &str) -> Result<(), String> {
        self.inner.borrow_mut().call_history.push(RecordedCall {
            service: service.to_string(),
            operation: operation.to_string(),
            request: request_json.to_string(),
            response: String::new(),
            error: None,
        });
        Ok(())
    }

    /// Schedule a delayed invocation.
    pub fn schedule_invoke(&self, service: &str, operation: &str, request_json: &str, delay: Duration) -> Result<(), String> {
        self.inner.borrow_mut().scheduled_invocations.push(
            format!("{}.{}:{}ms", service, operation, delay.as_millis())
        );
        let _ = request_json; // recorded for traceability
        Ok(())
    }

    // -----------------------------------------------------------------------
    // Plugin calls
    // -----------------------------------------------------------------------

    /// Call a plugin function. Returns `(response_json, error_message)`.
    pub fn plugin_call(&self, plugin_name: &str, function_name: &str, input_json: &str) -> (String, Option<String>) {
        let mut inner = self.inner.borrow_mut();

        for i in 0..inner.plugin_call_stubs.len() {
            let matches = {
                let stub = &inner.plugin_call_stubs[i];
                stub.plugin == plugin_name && stub.function == function_name
            };
            if matches {
                let stub = inner.plugin_call_stubs.remove(i);
                let err = stub.error.clone();
                let resp = stub.response.clone();
                inner.plugin_call_history.push(RecordedCall {
                    service: plugin_name.to_string(),
                    operation: function_name.to_string(),
                    request: input_json.to_string(),
                    response: resp.clone(),
                    error: err.clone(),
                });
                return if let Some(e) = err {
                    (String::new(), Some(e))
                } else {
                    (resp, None)
                };
            }
        }

        let err = format!("cleat_test: no stub registered for PluginCall({}, {})", plugin_name, function_name);
        inner.plugin_call_history.push(RecordedCall {
            service: plugin_name.to_string(),
            operation: function_name.to_string(),
            request: input_json.to_string(),
            response: String::new(),
            error: Some(err.clone()),
        });
        (String::new(), Some(err))
    }

    /// Streaming plugin call. Delegates to `plugin_call` in mock mode.
    pub fn plugin_call_streaming(&self, plugin_name: &str, function_name: &str, input_json: &str) -> (String, Option<String>) {
        self.plugin_call(plugin_name, function_name, input_json)
    }

    // -----------------------------------------------------------------------
    // Sleep / Time / Random
    // -----------------------------------------------------------------------

    /// Simulate workflow suspension for a duration.
    ///
    /// Advances the simulated clock by `d`. Returns `false` (no real suspend
    /// needed in test mode).
    pub fn cleat_sleep(&self, d: Duration) -> bool {
        self.inner.borrow_mut().now_ms += d.as_millis() as i64;
        false
    }

    /// Get the current simulated wall-clock time.
    pub fn now(&self) -> i64 {
        self.inner.borrow().now_ms
    }

    /// Get a deterministic random value from the pre-configured sequence.
    /// After the sequence is exhausted, returns 0.
    pub fn random(&self) -> i64 {
        let mut inner = self.inner.borrow_mut();
        if inner.random_idx < inner.random_seq.len() {
            let val = inner.random_seq[inner.random_idx];
            inner.random_idx += 1;
            val
        } else {
            0
        }
    }

    /// Log a message (no-op in test mode).
    pub fn cleat_log(&self, _message: &str) {}

    // -----------------------------------------------------------------------
    // Version
    // -----------------------------------------------------------------------

    /// Get the workflow definition version.
    pub fn version(&self) -> i32 {
        self.inner.borrow().version_val
    }

    /// Get the minimum supported version.
    pub fn min_version(&self) -> i32 {
        self.inner.borrow().min_version_val
    }

    // -----------------------------------------------------------------------
    // Signals
    // -----------------------------------------------------------------------

    /// Wait for one or more external signals with a timeout.
    ///
    /// In test mode this checks pending signals that are due by the current
    /// simulated time (or advances time on timeout). It never panics with
    /// [`SuspendSentinel`].
    pub fn await_signals(&self, signal_names: &[&str], timeout: Duration) -> SignalResult {
        let mut inner = self.inner.borrow_mut();

        // Try to find a matching pending signal that is due.
        for i in 0..inner.pending_signals.len() {
            if inner.pending_signals[i].deliver_at_ms <= inner.now_ms
                && signal_names.contains(&inner.pending_signals[i].name.as_str())
            {
                let sig = inner.pending_signals.remove(i);
                return SignalResult {
                    name: sig.name,
                    payload: sig.payload,
                    timed_out: false,
                    error: None,
                };
            }
        }

        if timeout.as_millis() == 0 {
            // Zero timeout = poll only
            return SignalResult {
                name: String::new(),
                payload: String::new(),
                timed_out: true,
                error: None,
            };
        }

        // No matching signal — advance time and check again.
        inner.now_ms += timeout.as_millis() as i64;

        for i in 0..inner.pending_signals.len() {
            if inner.pending_signals[i].deliver_at_ms <= inner.now_ms
                && signal_names.contains(&inner.pending_signals[i].name.as_str())
            {
                let sig = inner.pending_signals.remove(i);
                return SignalResult {
                    name: sig.name,
                    payload: sig.payload,
                    timed_out: false,
                    error: None,
                };
            }
        }

        SignalResult {
            name: String::new(),
            payload: String::new(),
            timed_out: true,
            error: None,
        }
    }

    /// Poll for a specific pending signal. Returns (payload, found, error).
    pub fn poll_signal(&self, name: &str) -> (String, bool, Option<String>) {
        let mut inner = self.inner.borrow_mut();
        for i in 0..inner.pending_signals.len() {
            if inner.pending_signals[i].name == name && inner.pending_signals[i].deliver_at_ms <= inner.now_ms {
                let sig = inner.pending_signals.remove(i);
                return (sig.payload, true, None);
            }
        }
        (String::new(), false, None)
    }

    /// Send a signal to a target workflow (fire-and-forget). In test mode,
    /// the signal is added to the local pending signal queue.
    pub fn signal_workflow(&self, _target_run_id: &str, signal_name: &str, payload: &str) -> Result<(), String> {
        let now = self.inner.borrow().now_ms;
        let mut inner = self.inner.borrow_mut();
        inner.sent_signals.push(format!("{}:{}", _target_run_id, signal_name));
        inner.pending_signals.push(PendingSignal {
            name: signal_name.to_string(),
            payload: payload.to_string(),
            deliver_at_ms: now,
        });
        Ok(())
    }

    /// Send a signal to a target workflow and wait for a reply.
    ///
    /// In test mode, the signal is sent and the reply is checked immediately
    /// (if pre-registered), otherwise a timeout error is returned.
    pub fn send_signal_and_wait(&self, target_run_id: &str, signal_name: &str, payload: &str, timeout: Duration) -> Result<String, String> {
        // Register reply channel (drop borrow before signal_workflow which needs its own borrow)
        let reply_corr_id;
        {
            let mut inner = self.inner.borrow_mut();
            inner.signal_reply_corr_counter += 1;
            reply_corr_id = format!("corr-{}-{}-{}", target_run_id, signal_name, inner.signal_reply_corr_counter);
            inner.signal_reply_channels.insert(reply_corr_id.clone(), "__pending__".to_string());
        }

        // Send the signal (needs its own borrow)
        let _ = self.signal_workflow(target_run_id, signal_name, payload);

        // Re-borrow to check reply
        let mut inner = self.inner.borrow_mut();
        if let Some(reply) = inner.signal_reply_channels.get(&reply_corr_id) {
            if reply != "__pending__" {
                let resp = reply.clone();
                inner.signal_reply_channels.remove(&reply_corr_id);
                return Ok(resp);
            }
        }

        // Simulate timeout
        inner.now_ms += timeout.as_millis() as i64;
        Err(format!("SendSignalAndWait(target={}, signal={}) timed out after {}ms", target_run_id, signal_name, timeout.as_millis()))
    }

    /// Reply to a signal, sending a response back to the sender.
    pub fn reply_to_signal(&self, correlation_id: &str, response: &str) -> Result<(), String> {
        let mut inner = self.inner.borrow_mut();
        if let Some(ch) = inner.signal_reply_channels.get_mut(correlation_id) {
            *ch = response.to_string();
            Ok(())
        } else {
            Err(format!("cleat_test: no pending signal for correlation_id={}", correlation_id))
        }
    }

    // -----------------------------------------------------------------------
    // Query state
    // -----------------------------------------------------------------------

    /// Set a key-value pair in query state.
    pub fn set_query_state(&self, key: &str, value: &str) {
        let prefix = self.inner.borrow().scope_prefix.clone();
        let mut inner = self.inner.borrow_mut();
        inner.query_state.insert(
            if prefix.is_empty() { key.to_string() } else { format!("{}{}", prefix, key) },
            value.to_string(),
        );
    }

    // -----------------------------------------------------------------------
    // Child workflows
    // -----------------------------------------------------------------------

    /// Start a child workflow.
    pub fn child_workflow(&self, name: &str, _input_json: &str) -> (String, Option<String>) {
        let mut inner = self.inner.borrow_mut();
        inner.child_run_id_counter += 1;
        let run_id = format!("child-{}-{}", name, inner.child_run_id_counter);

        let stub = inner.child_workflow_stubs.get(name).cloned();
        if let Some(stub) = stub {
            if let Some(err) = stub.error {
                inner.child_errors.insert(run_id.clone(), err);
            } else {
                inner.child_results.insert(run_id.clone(), stub.result);
            }
        } else {
            inner.child_results.insert(run_id.clone(), r#"{"status":"completed"}"#.to_string());
        }

        (run_id, None)
    }

    /// Start a child workflow with version options (delegates to `child_workflow`).
    pub fn child_workflow_with_options(&self, name: &str, input_json: &str, _version: i64) -> (String, Option<String>) {
        self.child_workflow(name, input_json)
    }


    /// Await a child workflow completion.
    pub fn await_child(&self, run_id: &str) -> (String, Option<String>) {
        let inner = self.inner.borrow();
        if let Some(err) = inner.child_errors.get(run_id) {
            return (String::new(), Some(err.clone()));
        }
        if let Some(result) = inner.child_results.get(run_id) {
            return (result.clone(), None);
        }
        (r#"{"status":"completed"}"#.to_string(), None)
    }

    /// Await all children workflows.
    pub fn await_all_children(&self, run_ids: &[&str]) -> Result<String, String> {
        let mut results: Vec<String> = Vec::new();
        for run_id in run_ids {
            let (result, err) = self.await_child(run_id);
            if let Some(e) = err {
                results.push(format!(r#"{{"runId":"{}","error":"{}"}}"#, run_id, e));
            } else {
                results.push(format!(r#"{{"runId":"{}","result":{}}}"#, run_id, result));
            }
        }
        Ok(format!("[{}]", results.join(",")))
    }

    /// Register a stub for a child workflow.
    pub fn register_child_workflow_stub(&self, name: &str, result: &str) {
        self.inner.borrow_mut().child_workflow_stubs.insert(
            name.to_string(),
            ChildWorkflowStub {
                result: result.to_string(),
                error: None,
            },
        );
    }

    // -----------------------------------------------------------------------
    // Promises
    // -----------------------------------------------------------------------

    /// Create a durable promise.
    pub fn create_promise(&self, name: &str) -> (String, Option<String>) {
        let mut inner = self.inner.borrow_mut();
        inner.defer_counter += 1;
        let promise_id = format!("prom-{}-{}", name, inner.defer_counter);
        inner.promises.insert(promise_id.clone(), PromiseState::Pending);
        (promise_id, None)
    }

    /// Await a durable promise.
    pub fn await_promise(&self, promise_id: &str, timeout: Duration) -> (String, bool, Option<String>) {
        let mut inner = self.inner.borrow_mut();
        match inner.promises.get(promise_id) {
            None => (String::new(), false, Some(format!("promise not found: {}", promise_id))),
            Some(PromiseState::Resolved(val)) => (val.clone(), false, None),
            Some(PromiseState::Rejected(err)) => (String::new(), false, Some(err.clone())),
            Some(PromiseState::Pending) => {
                inner.now_ms += timeout.as_millis() as i64;
                (String::new(), true, None)
            }
        }
    }

    /// Resolve a promise externally (for test orchestration).
    pub fn resolve_promise(&self, promise_id: &str, value: &str) -> Result<(), String> {
        let mut inner = self.inner.borrow_mut();
        match inner.promises.get_mut(promise_id) {
            None => Err(format!("promise not found: {}", promise_id)),
            Some(state) => {
                *state = PromiseState::Resolved(value.to_string());
                Ok(())
            }
        }
    }

    /// Reject a promise externally (for test orchestration).
    pub fn reject_promise(&self, promise_id: &str, error: &str) -> Result<(), String> {
        let mut inner = self.inner.borrow_mut();
        match inner.promises.get_mut(promise_id) {
            None => Err(format!("promise not found: {}", promise_id)),
            Some(state) => {
                *state = PromiseState::Rejected(error.to_string());
                Ok(())
            }
        }
    }

    /// Register an update handler.
    pub fn register_update_handler(&self, name: &str) {
        let _ = name; // no-op in test mode
    }

    // There is no register_query_handler here (removed 2026-08-09). It
    // recorded a handler name with the host but nothing ever routed an
    // external query to it -- see docs/determinism.md, "Why there is no
    // RegisterQueryHandler". Use set_query_state instead.

    // -----------------------------------------------------------------------
    // State (workflow-persisted key-value)
    // -----------------------------------------------------------------------

    /// Set a workflow state value.
    pub fn set_state(&self, key: &str, value: &str) -> Result<(), String> {
        let prefix = self.inner.borrow().scope_prefix.clone();
        let mut inner = self.inner.borrow_mut();
        let scoped = if prefix.is_empty() { key.to_string() } else { format!("{}{}", prefix, key) };
        inner.workflow_state.insert(scoped, value.to_string());
        Ok(())
    }

    /// Get a workflow state value.
    pub fn get_state(&self, key: &str) -> Result<String, String> {
        let inner = self.inner.borrow();
        let scoped = if inner.scope_prefix.is_empty() { key.to_string() } else { format!("{}{}", inner.scope_prefix, key) };
        inner.workflow_state
            .get(&scoped)
            .cloned()
            .ok_or_else(|| format!("key not found: {}", key))
    }

    /// Delete a workflow state key.
    pub fn delete_state(&self, key: &str) -> Result<(), String> {
        let prefix = self.inner.borrow().scope_prefix.clone();
        let mut inner = self.inner.borrow_mut();
        inner.workflow_state.remove(
            &(if prefix.is_empty() { key.to_string() } else { format!("{}{}", prefix, key) })
        );
        Ok(())
    }

    /// Atomically increment a state counter.
    pub fn incr_state(&self, key: &str, delta: i64) -> Result<i64, String> {
        let prefix = self.inner.borrow().scope_prefix.clone();
        let mut inner = self.inner.borrow_mut();
        let scoped = if prefix.is_empty() { key.to_string() } else { format!("{}{}", prefix, key) };
        let current = inner.workflow_state
            .get(&scoped)
            .and_then(|v| v.parse::<i64>().ok())
            .unwrap_or(0);
        let new_val = current + delta;
        inner.workflow_state.insert(scoped, new_val.to_string());
        Ok(new_val)
    }

    /// Check if a state key exists.
    pub fn has_state(&self, key: &str) -> bool {
        let inner = self.inner.borrow();
        let scoped = if inner.scope_prefix.is_empty() { key.to_string() } else { format!("{}{}", inner.scope_prefix, key) };
        inner.workflow_state.contains_key(&scoped)
    }

    /// List state keys with a given prefix.
    pub fn list_state(&self, prefix: &str) -> Result<Vec<String>, String> {
        let inner = self.inner.borrow();
        let scoped = if inner.scope_prefix.is_empty() { prefix.to_string() } else { format!("{}{}", inner.scope_prefix, prefix) };
        Ok(inner.workflow_state
            .keys()
            .filter(|k| k.starts_with(&scoped))
            .cloned()
            .collect())
    }

    // -----------------------------------------------------------------------
    // Scope (virtual objects)
    // -----------------------------------------------------------------------

    /// Set the virtual object scope. Returns the previous scope value.
    pub fn set_scope(&self, object_type: &str, instance_key: &str) -> String {
        let mut inner = self.inner.borrow_mut();
        let prev = inner.scope_prefix.clone();
        inner.scope_prefix = if !object_type.is_empty() && !instance_key.is_empty() {
            format!("vo:{}:{}:", object_type, instance_key)
        } else {
            String::new()
        };
        prev
    }

    /// Get the current virtual object scope.
    pub fn get_scope(&self) -> (String, String) {
        let inner = self.inner.borrow();
        if inner.scope_prefix.is_empty() {
            return (String::new(), String::new());
        }
        let trimmed = inner.scope_prefix.trim_end_matches(':');
        let parts: Vec<&str> = trimmed.splitn(3, ':').collect();
        if parts.len() == 3 && parts[0] == "vo" {
            (parts[1].to_string(), parts[2].to_string())
        } else {
            (String::new(), String::new())
        }
    }

    /// Clear the current scope. Returns the previous scope value.
    pub fn clear_scope(&self) -> String {
        let mut inner = self.inner.borrow_mut();
        let prev = inner.scope_prefix.clone();
        inner.scope_prefix = String::new();
        prev
    }

    // -----------------------------------------------------------------------
    // Defer / Cancellation
    // -----------------------------------------------------------------------

    /// Register a deferred cleanup action.
    pub fn cleat_defer(&self, _description: &str) -> (String, Option<String>) {
        let mut inner = self.inner.borrow_mut();
        inner.defer_counter += 1;
        let defer_id = format!("defer-{}", inner.defer_counter);
        (defer_id, None)
    }

    /// Check whether cancellation has been requested.
    pub fn poll_cancellation(&self) -> (bool, String) {
        let inner = self.inner.borrow();
        (inner.cancelled, inner.cancel_reason.clone())
    }

    // -----------------------------------------------------------------------
    // Continue-as-new
    // -----------------------------------------------------------------------

    /// Simulate continue-as-new.
    pub fn continue_as_new(&self, _input_json: &str) -> Option<String> {
        self.inner.borrow_mut().continue_as_new_called = true;
        None
    }

    /// Continue as new with an explicit version.
    pub fn continue_as_new_versioned(&self, input_json: &str, _new_version: i32) -> Result<(), String> {
        self.inner.borrow_mut().continue_as_new_called = true;
        let _ = input_json;
        Ok(())
    }

    // -----------------------------------------------------------------------
    // Workflow metadata
    // -----------------------------------------------------------------------

    /// Get the current workflow ID.
    pub fn workflow_id(&self) -> String {
        self.inner.borrow().workflow_id.clone()
    }

    /// Get the current run ID.
    pub fn run_id(&self) -> String {
        self.inner.borrow().run_id.clone()
    }

    /// Generate a deterministic UUID from a seed.
    pub fn uuid(&self, seed: &str) -> String {
        format!("mock-uuid-{}", seed)
    }

    // -----------------------------------------------------------------------
    // HTTP fetch
    // -----------------------------------------------------------------------

    /// Make an HTTP fetch request.
    pub fn cleat_fetch(&self, _method: &str, _url: &str, _headers: &str, _body: &str) -> Result<FetchResult, String> {
        Ok(FetchResult {
            status: 200,
            headers: HashMap::new(),
            body: String::new(),
        })
    }

    // -----------------------------------------------------------------------
    // Side effect
    // -----------------------------------------------------------------------

    /// Record a non-deterministic computation result (returns the input in mock mode).
    pub fn side_effect(&self, computed_result: &str) -> Result<String, String> {
        Ok(computed_result.to_string())
    }

    // -----------------------------------------------------------------------
    // Locks
    // -----------------------------------------------------------------------

    /// Acquire a concurrency lock (always succeeds in mock mode).
    pub fn acquire_lock(&self, _key: &str, _ttl: Duration) -> (bool, Option<String>) {
        (true, None)
    }

    /// Release a concurrency lock (always succeeds in mock mode).
    pub fn release_lock(&self, _key: &str) -> Option<String> {
        None
    }

    // -----------------------------------------------------------------------
    // Run detached
    // -----------------------------------------------------------------------

    /// Run a child workflow in detached mode.
    pub fn run_detached(&self, _name: &str, _input_json: &str) -> Result<(), String> {
        Ok(())
    }

    // -----------------------------------------------------------------------
    // Await condition
    // -----------------------------------------------------------------------

    /// Wait for a condition (invokes predicate in a loop in mock mode).
    pub fn await_condition(&self, predicate: impl Fn() -> bool, poll_interval: Duration, timeout: Duration) -> (bool, Option<String>) {
        let deadline = {
            let inner = self.inner.borrow();
            inner.now_ms + timeout.as_millis() as i64
        };
        loop {
            if predicate() {
                return (true, None);
            }
            {
                let mut inner = self.inner.borrow_mut();
                if inner.now_ms >= deadline {
                    return (false, Some("condition timed out".to_string()));
                }
                inner.now_ms += poll_interval.as_millis() as i64;
            }
        }
    }

    // ===================================================================
    // Test configuration helpers
    // ===================================================================

    /// Set the workflow version returned by `version()`.
    pub fn set_version(&self, v: i32) {
        self.inner.borrow_mut().version_val = v;
    }

    /// Set the minimum workflow version.
    pub fn set_min_version(&self, v: i32) {
        self.inner.borrow_mut().min_version_val = v;
    }

    /// Configure the sequence of values returned by `random()`.
    pub fn set_random_seq(&self, seq: Vec<i64>) {
        let mut inner = self.inner.borrow_mut();
        inner.random_seq = seq;
        inner.random_idx = 0;
    }

    /// Advance the simulated clock by `ms` milliseconds.
    pub fn advance_time(&self, ms: i64) {
        self.inner.borrow_mut().now_ms += ms;
    }

    /// Set the simulated clock to a specific time (ms since epoch).
    pub fn set_time(&self, ms: i64) {
        self.inner.borrow_mut().now_ms = ms;
    }

    /// Configure the cancellation state.
    pub fn set_cancelled(&self, cancelled: bool, reason: &str) {
        let mut inner = self.inner.borrow_mut();
        inner.cancelled = cancelled;
        inner.cancel_reason = reason.to_string();
    }

    /// Set the workflow ID returned by `workflow_id()`.
    pub fn set_workflow_id(&self, id: &str) {
        self.inner.borrow_mut().workflow_id = id.to_string();
    }

    /// Set the run ID returned by `run_id()`.
    pub fn set_run_id(&self, id: &str) {
        self.inner.borrow_mut().run_id = id.to_string();
    }

    /// Set retry simulation: fail the first `n` calls per (service, operation).
    pub fn set_retry_simulation(&self, n: u32) {
        let mut inner = self.inner.borrow_mut();
        inner.retry_sim_count = n;
        inner.retry_sim_attempts.clear();
    }

    // ===================================================================
    // Call recording / assertion helpers
    // ===================================================================

    /// Get a copy of all recorded `cleat_call` invocations.
    pub fn call_history(&self) -> Vec<RecordedCall> {
        self.inner.borrow().call_history.clone()
    }

    /// Get a copy of all recorded `plugin_call` invocations.
    pub fn plugin_call_history(&self) -> Vec<RecordedCall> {
        self.inner.borrow().plugin_call_history.clone()
    }

    /// Check if a call to the given `service.operation` was recorded.
    pub fn assert_called(&self, service: &str, operation: &str) -> bool {
        self.call_count(service, operation) > 0
    }

    /// Check that no call to `service.operation` was recorded.
    pub fn assert_not_called(&self, service: &str, operation: &str) -> bool {
        self.call_count(service, operation) == 0
    }

    /// Count how many times `service.operation` was called.
    pub fn call_count(&self, service: &str, operation: &str) -> usize {
        let inner = self.inner.borrow();
        inner.call_history
            .iter()
            .filter(|rec| rec.service == service && rec.operation == operation)
            .count()
    }

    /// Read a query state value previously set via `set_query_state`.
    pub fn read_query_state(&self, key: &str) -> Option<String> {
        self.inner.borrow().query_state.get(key).cloned()
    }

    /// Check whether `continue_as_new` was called.
    pub fn continue_as_new_was_called(&self) -> bool {
        self.inner.borrow().continue_as_new_called
    }

    /// Reset the entire test environment to its initial state.
    pub fn reset(&self) {
        let mut inner = self.inner.borrow_mut();
        inner.call_history.clear();
        inner.call_stubs.clear();
        inner.plugin_call_stubs.clear();
        inner.pending_signals.clear();
        inner.child_workflow_stubs.clear();
        inner.child_results.clear();
        inner.child_errors.clear();
        inner.child_run_id_counter = 0;
        inner.now_ms = 1704067200000;
        inner.version_val = 1;
        inner.min_version_val = 1;
        inner.random_seq.clear();
        inner.random_idx = 0;
        inner.query_state.clear();
        inner.workflow_state.clear();
        inner.scope_prefix.clear();
        inner.cancelled = false;
        inner.cancel_reason.clear();
        inner.defer_counter = 0;
        inner.continue_as_new_called = false;
        inner.workflow_id = "test-workflow".to_string();
        inner.run_id = "test-run-001".to_string();
        inner.promises.clear();
        inner.signal_reply_channels.clear();
        inner.signal_reply_corr_counter = 0;
        inner.sent_signals.clear();
        inner.retry_sim_count = 0;
        inner.retry_sim_attempts.clear();
        inner.plugin_call_history.clear();
        inner.scheduled_invocations.clear();
    }

}

impl Default for TestEnv {
    fn default() -> Self {
        Self::new()
    }
}

// ---------------------------------------------------------------------------
// Supporting types
// ---------------------------------------------------------------------------

/// Retry policy (mirrors the structure from `cleat_sdk::host_calls::RetryPolicy`).
#[derive(Debug, Clone)]
pub struct RetryPolicy {
    pub max_attempts: u32,
    pub initial_interval_ms: u64,
    pub backoff_multiplier: f64,
    pub maximum_interval_ms: u64,
    pub non_retryable_errors: Vec<String>,
}

// ===================================================================
// Tests
// ===================================================================

#[cfg(test)]
mod tests {
    use super::*;

    // -----------------------------------------------------------------------
    // Basic call stubbing & recording
    // -----------------------------------------------------------------------

    #[test]
    fn test_basic_call_stub_and_recording() {
        let env = TestEnv::new();
        env.expect_call("payment", "charge", r#"{"amount":5000}"#)
            .respond(r#"{"id":"ch_123","status":"succeeded"}"#);

        let (resp, err) = env.cleat_call("payment", "charge", r#"{"amount":5000}"#);

        assert!(err.is_none());
        assert_eq!(resp, r#"{"id":"ch_123","status":"succeeded"}"#);
        assert_eq!(env.call_count("payment", "charge"), 1);
        assert!(env.assert_called("payment", "charge"));
    }

    #[test]
    fn test_call_error_stub() {
        let env = TestEnv::new();
        env.expect_call("payment", "charge", r#"{"amount":5000}"#)
            .respond_error("insufficient funds");

        let (resp, err) = env.cleat_call("payment", "charge", r#"{"amount":5000}"#);
        assert!(resp.is_empty());
        assert_eq!(err.unwrap(), "insufficient funds");
    }

    #[test]
    fn test_no_stub_returns_error() {
        let env = TestEnv::new();
        let (resp, err) = env.cleat_call("unknown", "op", r#"{}"#);
        assert!(resp.is_empty());
        assert!(err.unwrap().contains("no stub registered"));
    }

    #[test]
    fn test_any_request_matcher() {
        let env = TestEnv::new();
        env.expect_any_call("service", "op")
            .respond(r#"{"ok":true}"#);

        let (resp, err) = env.cleat_call("service", "op", r#"{"any":"payload"}"#);
        assert!(err.is_none());
        assert_eq!(resp, r#"{"ok":true}"#);
    }

    #[test]
    fn test_stub_consumed_on_match() {
        let env = TestEnv::new();
        env.expect_call("svc", "op", r#"{"v":1}"#).respond(r#"{"r":"first"}"#);

        let (resp1, _) = env.cleat_call("svc", "op", r#"{"v":1}"#);
        assert_eq!(resp1, r#"{"r":"first"}"#);

        // Second call with same args has no stub left.
        let (_resp2, err2) = env.cleat_call("svc", "op", r#"{"v":1}"#);
        assert!(err2.is_some());
    }

    // -----------------------------------------------------------------------
    // Plugin calls
    // -----------------------------------------------------------------------

    #[test]
    fn test_plugin_call() {
        let env = TestEnv::new();
        env.expect_plugin_call("llm", "chat")
            .respond(r#"{"severity":"critical"}"#);

        let (resp, err) = env.plugin_call("llm", "chat", r#"{"prompt":"test"}"#);
        assert!(err.is_none());
        assert_eq!(resp, r#"{"severity":"critical"}"#);
    }

    #[test]
    fn test_plugin_call_error() {
        let env = TestEnv::new();
        env.expect_plugin_call("blobstore", "get")
            .respond_error("not found");

        let (resp, err) = env.plugin_call("blobstore", "get", r#"{"key":"test"}"#);
        assert!(resp.is_empty());
        assert_eq!(err.unwrap(), "not found");
    }

    #[test]
    fn test_plugin_call_no_stub() {
        let env = TestEnv::new();
        let (resp, err) = env.plugin_call("unknown", "fn", r#"{}"#);
        assert!(resp.is_empty());
        assert!(err.unwrap().contains("no stub registered"));
    }

    #[test]
    fn test_plugin_call_history() {
        let env = TestEnv::new();
        env.expect_plugin_call("llm", "chat").respond(r#"{"ok":true}"#);
        let _ = env.plugin_call("llm", "chat", r#"{}"#);

        let history = env.plugin_call_history();
        assert_eq!(history.len(), 1);
        assert_eq!(history[0].service, "llm");
        assert_eq!(history[0].operation, "chat");
    }

    // -----------------------------------------------------------------------
    // Signal injection
    // -----------------------------------------------------------------------

    #[test]
    fn test_inject_signal_and_poll() {
        let env = TestEnv::new();
        env.inject_signal("order_placed", r#"{"orderId":"ord_1"}"#);

        let (payload, found, err) = env.poll_signal("order_placed");
        assert!(found);
        assert!(err.is_none());
        assert_eq!(payload, r#"{"orderId":"ord_1"}"#);
    }

    #[test]
    fn test_await_signals() {
        let env = TestEnv::new();
        env.inject_signal("payment_received", r#"{"amount":5000}"#);

        let result = env.await_signals(&["payment_received"], Duration::from_secs(5));
        assert!(!result.timed_out);
        assert!(result.error.is_none());
        assert_eq!(result.name, "payment_received");
        assert_eq!(result.payload, r#"{"amount":5000}"#);
    }

    #[test]
    fn test_await_signals_timeout() {
        let env = TestEnv::new();
        let result = env.await_signals(&["nonexistent"], Duration::from_millis(100));
        assert!(result.timed_out);
        assert!(result.error.is_none());
        assert!(result.name.is_empty());
    }

    #[test]
    fn test_await_signals_zero_timeout() {
        let env = TestEnv::new();
        let result = env.await_signals(&["nonexistent"], Duration::from_millis(0));
        assert!(result.timed_out);
    }

    #[test]
    fn test_signal_workflow() {
        let env = TestEnv::new();
        env.signal_workflow("target-run", "my_signal", r#"{"msg":"hello"}"#).unwrap();

        let (payload, found, _err) = env.poll_signal("my_signal");
        assert!(found);
        assert_eq!(payload, r#"{"msg":"hello"}"#);
    }

    // -----------------------------------------------------------------------
    // Sleep/time
    // -----------------------------------------------------------------------

    #[test]
    fn test_sleep_advances_time() {
        let env = TestEnv::new();
        let before = env.now();
        env.cleat_sleep(Duration::from_secs(5));
        let after = env.now();
        assert_eq!(after - before, 5000);
    }

    #[test]
    fn test_random_sequence() {
        let env = TestEnv::new();
        env.set_random_seq(vec![42, 99, 7]);

        assert_eq!(env.random(), 42);
        assert_eq!(env.random(), 99);
        assert_eq!(env.random(), 7);
        assert_eq!(env.random(), 0); // exhausted
    }

    // -----------------------------------------------------------------------
    // Version
    // -----------------------------------------------------------------------

    #[test]
    fn test_version() {
        let env = TestEnv::new();
        assert_eq!(env.version(), 1);
        env.set_version(3);
        assert_eq!(env.version(), 3);
    }

    #[test]
    fn test_min_version() {
        let env = TestEnv::new();
        assert_eq!(env.min_version(), 1);
        env.set_min_version(2);
        assert_eq!(env.min_version(), 2);
    }

    // -----------------------------------------------------------------------
    // State management
    // -----------------------------------------------------------------------

    #[test]
    fn test_workflow_state() {
        let env = TestEnv::new();
        env.set_state("my_key", "my_value").unwrap();

        let val = env.get_state("my_key").unwrap();
        assert_eq!(val, "my_value");
        assert!(env.has_state("my_key"));
        assert!(!env.has_state("nonexistent"));

        env.delete_state("my_key").unwrap();
        assert!(!env.has_state("my_key"));
    }

    #[test]
    fn test_incr_state() {
        let env = TestEnv::new();

        let val = env.incr_state("counter", 5).unwrap();
        assert_eq!(val, 5);

        let val2 = env.incr_state("counter", 3).unwrap();
        assert_eq!(val2, 8);
    }

    #[test]
    fn test_list_state() {
        let env = TestEnv::new();
        env.set_state("a:1", "v1").unwrap();
        env.set_state("a:2", "v2").unwrap();
        env.set_state("b:1", "v3").unwrap();

        let keys = env.list_state("a:").unwrap();
        assert_eq!(keys.len(), 2);
    }

    #[test]
    fn test_get_missing_state() {
        let env = TestEnv::new();
        let result = env.get_state("nonexistent");
        assert!(result.is_err());
    }

    // -----------------------------------------------------------------------
    // Scope
    // -----------------------------------------------------------------------

    #[test]
    fn test_scope_and_state() {
        let env = TestEnv::new();
        let prev = env.set_scope("counter", "user_42");
        assert!(prev.is_empty());

        env.set_state("count", "10").unwrap();
        let val = env.get_state("count").unwrap();
        assert_eq!(val, "10");

        let (obj_type, inst_key) = env.get_scope();
        assert_eq!(obj_type, "counter");
        assert_eq!(inst_key, "user_42");

        env.clear_scope();
        let (obj_type2, _) = env.get_scope();
        assert!(obj_type2.is_empty());
    }

    // -----------------------------------------------------------------------
    // Child workflows
    // -----------------------------------------------------------------------

    #[test]
    fn test_child_workflow() {
        let env = TestEnv::new();
        env.register_child_workflow_stub("inventory_check", r#"{"available":true}"#);

        let (run_id, err) = env.child_workflow("inventory_check", r#"{"sku":"s1"}"#);
        assert!(err.is_none());
        assert!(run_id.contains("inventory_check"));

        let (result, err) = env.await_child(&run_id);
        assert!(err.is_none());
        assert_eq!(result, r#"{"available":true}"#);
    }

    #[test]
    fn test_child_workflow_default_result() {
        let env = TestEnv::new();
        let (run_id, err) = env.child_workflow("unknown_child", r#"{}"#);
        assert!(err.is_none());

        let (result, err) = env.await_child(&run_id);
        assert!(err.is_none());
        assert_eq!(result, r#"{"status":"completed"}"#);
    }

    #[test]
    fn test_await_all_children() {
        let env = TestEnv::new();
        env.register_child_workflow_stub("child_a", r#"{"status":"done"}"#);

        let (run_a, _) = env.child_workflow("child_a", r#"{}"#);
        let (run_b, _) = env.child_workflow("child_b", r#"{}"#);

        let result = env.await_all_children(&[&run_a, &run_b]).unwrap();
        // Just check it's valid JSON with two entries
        assert!(result.starts_with('['));
        assert!(result.ends_with(']'));
        assert!(result.contains("child_a"));
        assert!(result.contains("child_b"));
    }

    // -----------------------------------------------------------------------
    // Promises
    // -----------------------------------------------------------------------

    #[test]
    fn test_promise_workflow() {
        let env = TestEnv::new();
        let (prom_id, err) = env.create_promise("test-promise");
        assert!(err.is_none());
        assert!(prom_id.contains("test-promise"));

        // Promise is pending initially
        let (_val, timed_out, err) = env.await_promise(&prom_id, Duration::from_millis(100));
        assert!(timed_out);
        assert!(err.is_none());

        // Resolve and await
        env.resolve_promise(&prom_id, r#"{"status":"done"}"#).unwrap();
        let (val, timed_out, err) = env.await_promise(&prom_id, Duration::from_millis(100));
        assert!(!timed_out);
        assert!(err.is_none());
        assert_eq!(val, r#"{"status":"done"}"#);
    }

    #[test]
    fn test_reject_promise() {
        let env = TestEnv::new();
        let (prom_id, _) = env.create_promise("fail-promise");
        env.reject_promise(&prom_id, "something went wrong").unwrap();

        let (_val, _timed_out, err) = env.await_promise(&prom_id, Duration::from_millis(100));
        assert!(err.is_some());
        let msg = err.unwrap();
        assert!(msg.contains("rejected") || msg.contains("went wrong"));
    }

    // -----------------------------------------------------------------------
    // Defer / Cancellation
    // -----------------------------------------------------------------------

    #[test]
    fn test_defer() {
        let env = TestEnv::new();
        let (id, err) = env.cleat_defer("cleanup resources");
        assert!(err.is_none());
        assert!(id.contains("defer"));
    }

    #[test]
    fn test_cancellation() {
        let env = TestEnv::new();
        let (cancelled, reason) = env.poll_cancellation();
        assert!(!cancelled);
        assert!(reason.is_empty());

        env.set_cancelled(true, "timeout");
        let (cancelled, reason) = env.poll_cancellation();
        assert!(cancelled);
        assert_eq!(reason, "timeout");
    }

    // -----------------------------------------------------------------------
    // Continue-as-new
    // -----------------------------------------------------------------------

    #[test]
    fn test_continue_as_new() {
        let env = TestEnv::new();
        let err = env.continue_as_new(r#"{"restart":true}"#);
        assert!(err.is_none());
        assert!(env.continue_as_new_was_called());
    }

    // -----------------------------------------------------------------------
    // Fetch
    // -----------------------------------------------------------------------

    #[test]
    fn test_fetch() {
        let env = TestEnv::new();
        let result = env.cleat_fetch("GET", "https://example.com", "{}", "");
        assert!(result.is_ok());
        assert_eq!(result.unwrap().status, 200);
    }

    // -----------------------------------------------------------------------
    // Locks
    // -----------------------------------------------------------------------

    #[test]
    fn test_locks() {
        let env = TestEnv::new();
        let (acquired, err) = env.acquire_lock("lock-key", Duration::from_secs(30));
        assert!(acquired);
        assert!(err.is_none());

        let err = env.release_lock("lock-key");
        assert!(err.is_none());
    }

    // -----------------------------------------------------------------------
    // Side effect
    // -----------------------------------------------------------------------

    #[test]
    fn test_side_effect() {
        let env = TestEnv::new();
        let result = env.side_effect(r#"{"computed":42}"#).unwrap();
        assert_eq!(result, r#"{"computed":42}"#);
    }

    // -----------------------------------------------------------------------
    // Send / schedule
    // -----------------------------------------------------------------------

    #[test]
    fn test_cleat_send_recording() {
        let env = TestEnv::new();
        env.cleat_send("notification", "email", r#"{"to":"user@example.com"}"#).unwrap();

        assert_eq!(env.call_count("notification", "email"), 1);
    }

    #[test]
    fn test_schedule_invoke() {
        let env = TestEnv::new();
        env.schedule_invoke("timer", "remind", r#"{"id":"t1"}"#, Duration::from_secs(60)).unwrap();
    }

    // -----------------------------------------------------------------------
    // Workflow metadata
    // -----------------------------------------------------------------------

    #[test]
    fn test_workflow_id() {
        let env = TestEnv::new();
        assert_eq!(env.workflow_id(), "test-workflow");
        env.set_workflow_id("custom-wf");
        assert_eq!(env.workflow_id(), "custom-wf");
    }

    #[test]
    fn test_run_id() {
        let env = TestEnv::new();
        assert_eq!(env.run_id(), "test-run-001");
        env.set_run_id("custom-run");
        assert_eq!(env.run_id(), "custom-run");
    }

    #[test]
    fn test_uuid() {
        let env = TestEnv::new();
        let uuid = env.uuid("test-seed");
        assert_eq!(uuid, "mock-uuid-test-seed");
    }

    // -----------------------------------------------------------------------
    // Signal reply
    // -----------------------------------------------------------------------

    #[test]
    fn test_send_signal_and_wait_timeout() {
        let env = TestEnv::new();
        let result = env.send_signal_and_wait("target", "sig", r#"{}"#, Duration::from_millis(100));
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("timed out"));
    }

    #[test]
    fn test_reply_to_signal_unknown() {
        let env = TestEnv::new();
        let result = env.reply_to_signal("nonexistent", r#"{}"#);
        assert!(result.is_err());
    }

    // -----------------------------------------------------------------------
    // Time management
    // -----------------------------------------------------------------------

    #[test]
    fn test_advance_time() {
        let env = TestEnv::new();
        let start = env.now();
        env.advance_time(3600000); // 1 hour
        assert_eq!(env.now(), start + 3600000);
    }

    #[test]
    fn test_set_time() {
        let env = TestEnv::new();
        env.set_time(0);
        assert_eq!(env.now(), 0);
    }

    // -----------------------------------------------------------------------
    // Retry simulation
    // -----------------------------------------------------------------------

    #[test]
    fn test_retry_simulation() {
        let env = TestEnv::new();
        env.expect_call("payment", "charge", r#"{"amount":5000}"#)
            .respond(r#"{"id":"ch_123"}"#);
        env.set_retry_simulation(2);

        // First two calls fail
        let (resp1, err1) = env.cleat_call("payment", "charge", r#"{"amount":5000}"#);
        assert!(resp1.is_empty());
        assert!(err1.unwrap().contains("simulated transient failure"));

        let (resp2, err2) = env.cleat_call("payment", "charge", r#"{"amount":5000}"#);
        assert!(resp2.is_empty());
        assert!(err2.unwrap().contains("simulated transient failure"));

        // Third call succeeds
        let (resp3, err3) = env.cleat_call("payment", "charge", r#"{"amount":5000}"#);
        assert!(err3.is_none());
        assert_eq!(resp3, r#"{"id":"ch_123"}"#);
    }

    // -----------------------------------------------------------------------
    // Signal result struct
    // -----------------------------------------------------------------------

    #[test]
    fn test_signal_result_fields() {
        let env = TestEnv::new();
        env.inject_signal("greeting", r#"{"hello":"world"}"#);

        let sr = env.await_signals(&["greeting"], Duration::from_secs(1));
        assert_eq!(sr.name, "greeting");
        assert_eq!(sr.payload, r#"{"hello":"world"}"#);
        assert!(!sr.timed_out);
        assert!(sr.error.is_none());
    }

    // -----------------------------------------------------------------------
    // Reset
    // -----------------------------------------------------------------------

    #[test]
    fn test_reset() {
        let env = TestEnv::new();
        env.expect_call("svc", "op", r#"{}"#).respond(r#"{}"#);
        let _ = env.cleat_call("svc", "op", r#"{}"#);
        assert_eq!(env.call_count("svc", "op"), 1);

        env.reset();
        assert_eq!(env.call_count("svc", "op"), 0);
    }

    // -----------------------------------------------------------------------
    // Await condition
    // -----------------------------------------------------------------------

    #[test]
    fn test_await_condition_true() {
        let env = TestEnv::new();
        let called = std::cell::Cell::new(false);
        let (ok, err) = env.await_condition(
            || { called.set(true); true },
            Duration::from_millis(10),
            Duration::from_secs(1),
        );
        assert!(ok);
        assert!(err.is_none());
    }

    #[test]
    fn test_await_condition_timeout() {
        let env = TestEnv::new();
        let (ok, err) = env.await_condition(
            || false,
            Duration::from_millis(10),
            Duration::from_millis(50),
        );
        assert!(!ok);
        assert!(err.is_some());
    }

    // -----------------------------------------------------------------------
    // Builder chaining
    // -----------------------------------------------------------------------

    #[test]
    fn test_chained_expect_call() {
        let env = TestEnv::new();

        env.expect_call("svc", "op1", r#"{"a":1}"#).respond(r#"{"r1":"ok"}"#)
           .expect_call("svc", "op2", r#"{"b":2}"#).respond(r#"{"r2":"ok"}"#);

        let (r1, _) = env.cleat_call("svc", "op1", r#"{"a":1}"#);
        assert_eq!(r1, r#"{"r1":"ok"}"#);

        let (r2, _) = env.cleat_call("svc", "op2", r#"{"b":2}"#);
        assert_eq!(r2, r#"{"r2":"ok"}"#);
    }

    // -----------------------------------------------------------------------
    // Workflow execution
    // -----------------------------------------------------------------------

    #[test]
    fn test_execute_workflow() {
        let env = TestEnv::new();
        env.expect_call("greeter", "hello", r#"{"name":"test"}"#)
            .respond(r#"{"greeting":"Hello, World!"}"#);

        let result = env.execute(
            |h: &TestEnv, input: &str| -> String {
                let (resp, _err) = h.cleat_call("greeter", "hello", input);
                resp
            },
            r#"{"name":"test"}"#,
        );

        assert_eq!(result, r#"{"greeting":"Hello, World!"}"#);
        assert!(env.assert_called("greeter", "hello"));
    }

    // -----------------------------------------------------------------------
    // Query state
    // -----------------------------------------------------------------------

    #[test]
    fn test_query_state() {
        let env = TestEnv::new();
        env.set_query_state("my_query", r#"{"result":"data"}"#);

        let val = env.read_query_state("my_query");
        assert_eq!(val, Some(r#"{"result":"data"}"#.to_string()));
    }

    // -----------------------------------------------------------------------
    // Signal delayed
    // -----------------------------------------------------------------------

    #[test]
    fn test_inject_signal_delayed_not_yet_available() {
        let env = TestEnv::new();
        env.inject_signal_delayed("late_signal", r#"{"msg":"later"}"#, Duration::from_secs(60));

        // Signal not available yet
        let result = env.await_signals(&["late_signal"], Duration::from_millis(10));
        assert!(result.timed_out);
    }

    #[test]
    fn test_inject_signal_delayed_after_advance() {
        let env = TestEnv::new();
        env.inject_signal_delayed("late_signal", r#"{"msg":"later"}"#, Duration::from_secs(60));
        env.advance_time(120_000); // advance 2 min

        let result = env.await_signals(&["late_signal"], Duration::from_millis(10));
        assert!(!result.timed_out);
        assert_eq!(result.name, "late_signal");
    }

    // -----------------------------------------------------------------------
    // Plugin call streaming
    // -----------------------------------------------------------------------

    #[test]
    fn test_plugin_call_streaming() {
        let env = TestEnv::new();
        env.expect_plugin_call("streamer", "read")
            .respond(r#"{"chunk":"data"}"#);

        let (resp, err) = env.plugin_call_streaming("streamer", "read", r#"{}"#);
        assert!(err.is_none());
        assert_eq!(resp, r#"{"chunk":"data"}"#);
    }

    // -----------------------------------------------------------------------
    // Continue-as-new versioned
    // -----------------------------------------------------------------------

    #[test]
    fn test_continue_as_new_versioned() {
        let env = TestEnv::new();
        let result = env.continue_as_new_versioned(r#"{"v":2}"#, 2);
        assert!(result.is_ok());
        assert!(env.continue_as_new_was_called());
    }

    // -----------------------------------------------------------------------
    // Cleat call with retry
    // -----------------------------------------------------------------------

    #[test]
    fn test_cleat_call_with_retry() {
        let env = TestEnv::new();
        env.expect_call("svc", "op", r#"{"x":1}"#).respond(r#"{"y":2}"#);

        let policy = RetryPolicy {
            max_attempts: 3,
            initial_interval_ms: 100,
            backoff_multiplier: 2.0,
            maximum_interval_ms: 1000,
            non_retryable_errors: vec![],
        };
        let (resp, err) = env.cleat_call_with_retry(&policy, "svc", "op", r#"{"x":1}"#);
        assert!(err.is_none());
        assert_eq!(resp, r#"{"y":2}"#);
    }

    // -----------------------------------------------------------------------
    // Heartbeat
    // -----------------------------------------------------------------------

    #[test]
    fn test_cleat_call_heartbeat() {
        let env = TestEnv::new();
        env.expect_call("svc", "op", r#"{"x":1}"#).respond(r#"{"y":2}"#);

        let (resp, err) = env.cleat_call_heartbeat("svc", "op", r#"{"x":1}"#, 1000);
        assert!(err.is_none());
        assert_eq!(resp, r#"{"y":2}"#);
    }
}
