// HostCalls: Rust wrapper around the 18 WASM host function imports
// matching the cleat host runtime ABI from internal/host/imports.go.

use crate::memory;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::time::Duration;

/// All host function imports from the "env" WASM module.
/// Each returns i64 with a bit-packed result. Strings cross as (ptr, len) pairs.
mod imports {
    #[link(wasm_import_module = "env")]
    extern "C" {
        // cleatsleep - single i64 param
        pub fn cleat_sleep(duration_ms: i64) -> i64;

        // cleatnow - no params
        pub fn cleat_now() -> i64;

        // cleatrandom - no params
        pub fn cleat_random() -> i64;

        // cleatlog - one string in
        pub fn cleat_log(msg_ptr: *const u8, msg_len: u32) -> i64;

        // cleatversion - no params
        pub fn cleat_version() -> i64;

        // cleatmin_version - no params
        pub fn cleat_min_version() -> i64;

        // cleatdefer - one string in, one string out
        pub fn cleat_defer(
            desc_ptr: *const u8, desc_len: u32,
            defer_id_ptr: *mut u8, defer_id_max_len: u32,
        ) -> i64;

        // cleatpoll_cancellation - one string out
        pub fn cleat_poll_cancellation(
            reason_ptr: *mut u8, reason_max_len: u32,
        ) -> i64;

        // cleatpoll_signal - one string in, one string out
        pub fn cleat_poll_signal(
            name_ptr: *const u8, name_len: u32,
            payload_ptr: *mut u8, payload_max_len: u32,
        ) -> i64;

        // cleatcontinue_as_new - one string in
        pub fn cleat_continue_as_new(
            input_ptr: *const u8, input_len: u32,
        ) -> i64;

        // cleatchild_workflow - two strings in, one string out
        pub fn cleat_child_workflow(
            name_ptr: *const u8, name_len: u32,
            input_ptr: *const u8, input_len: u32,
            run_id_ptr: *mut u8, run_id_max_len: u32,
        ) -> i64;

        // cleat_child_workflow_with_options - two strings, i64 version, i64 priority, parentClosePolicy string, one string out
        pub fn cleat_child_workflow_with_options(
            name_ptr: *const u8, name_len: u32,
            input_ptr: *const u8, input_len: u32,
            version: i64,
            priority: i64,
            policy_ptr: *const u8, policy_len: u32,
            run_id_ptr: *mut u8, run_id_max_len: u32,
        ) -> i64;


        // cleatawait_child - one string in, one string out
        pub fn cleat_await_child(
            run_id_ptr: *const u8, run_id_len: u32,
            result_ptr: *mut u8, result_max_len: u32,
        ) -> i64;

        // durableawait_signals - strings + i64 mixed
        pub fn cleat_await_signals(
            names_ptr: *const u8, names_len: u32,
            timeout_ms: i64,
            sig_name_ptr: *mut u8, sig_name_max_len: u32,
            payload_ptr: *mut u8, payload_max_len: u32,
        ) -> i64;

        // setquerystate - two strings in
        pub fn set_query_state(
            key_ptr: *const u8, key_len: u32,
            val_ptr: *const u8, val_len: u32,
        ) -> i64;

        // durablecall - 4 strings in, 1 string out
        pub fn cleat_call(
            svc_ptr: *const u8, svc_len: u32,
            op_ptr: *const u8, op_len: u32,
            req_ptr: *const u8, req_len: u32,
            resp_ptr: *mut u8, resp_max_len: u32,
        ) -> i64;

        // durablecreate_promise - one string in, one string out (ABI 2.20)
        pub fn cleat_create_promise(
            name_ptr: *const u8, name_len: u32,
            id_out_ptr: *mut u8, id_out_max: u32,
        ) -> i64;

        // durableawait_promise - one string in, i64 timeout, one string out (ABI 2.21)
        pub fn cleat_await_promise(
            id_ptr: *const u8, id_len: u32,
            timeout_ms: i64,
            result_out_ptr: *mut u8, result_out_max: u32,
        ) -> i64;

        // durableregister_update_handler - one string in, void return (ABI 2.22)
        pub fn cleat_register_update_handler(
            name_ptr: *const u8, name_len: u32,
        ) -> i64;

        // cleat_send_signal_and_wait - 3 strings in, i64 timeout, 1 string out (ABI 2.23)
        pub fn cleat_send_signal_and_wait(
            target_ptr: *const u8, target_len: u32,
            signal_ptr: *const u8, signal_len: u32,
            payload_ptr: *const u8, payload_len: u32,
            timeout_ms: i64,
            response_ptr: *mut u8, response_max_len: u32,
        ) -> i64;

        // cleat_reply_to_signal - 2 strings in (ABI 2.24)
        pub fn cleat_reply_to_signal(
            correlation_ptr: *const u8, correlation_len: u32,
            response_ptr: *const u8, response_len: u32,
        ) -> i64;

        // cleat_signal_workflow - 3 strings in (ABI 2.25)
        pub fn cleat_signal_workflow(
            target_ptr: *const u8, target_len: u32,
            signal_ptr: *const u8, signal_len: u32,
            payload_ptr: *const u8, payload_len: u32,
        ) -> i64;

        // cleat_set_scope - 2 strings in, 1 string out (ABI 2.26)
        pub fn cleat_set_scope(
            obj_type_ptr: *const u8, obj_type_len: u32,
            inst_key_ptr: *const u8, inst_key_len: u32,
            prev_scope_ptr: *mut u8, prev_scope_max_len: u32,
        ) -> i64;

        // cleat_get_scope - 2 string buffers out (ABI 2.27)
        pub fn cleat_get_scope(
            obj_type_ptr: *mut u8, obj_type_max_len: u32,
            inst_key_ptr: *mut u8, inst_key_max_len: u32,
        ) -> i64;

        // cleat_uuid - 1 string in, 1 string out (ABI 2.28)
        pub fn cleat_uuid(
            seed_ptr: *const u8, seed_len: u32,
            uuid_ptr: *mut u8, uuid_max_len: u32,
        ) -> i64;

        // plugin_call - ABI 2.19 (WASM import name is "plugin_call")
        #[link_name = "plugin_call"]
        pub fn cleat_plugin_call(
            plugin_name_ptr: *const u8, plugin_name_len: u32,
            function_name_ptr: *const u8, function_name_len: u32,
            input_ptr: *const u8, input_len: u32,
            response_ptr: *mut u8, response_max_len: u32,
        ) -> i64;

        // cleat_workflow_id - ABI 2.29, no params, one string out
        pub fn cleat_workflow_id(id_ptr: *mut u8, id_max_len: u32) -> i64;

        // cleat_run_id - ABI 2.30, no params, one string out
        pub fn cleat_run_id(id_ptr: *mut u8, id_max_len: u32) -> i64;

        // cleat_resolve_promise - ABI 2.31, two strings in
        pub fn cleat_resolve_promise(id_ptr: *const u8, id_len: u32, value_ptr: *const u8, value_len: u32) -> i64;

        // cleat_reject_promise - ABI 2.32, two strings in
        pub fn cleat_reject_promise(id_ptr: *const u8, id_len: u32, error_ptr: *const u8, error_len: u32) -> i64;

        // cleat_send - ABI 2.33, three strings in
        pub fn cleat_send(svc_ptr: *const u8, svc_len: u32, op_ptr: *const u8, op_len: u32, req_ptr: *const u8, req_len: u32) -> i64;

        // schedule_invoke - ABI 2.34, three strings in, i64 delay
        pub fn schedule_invoke(svc_ptr: *const u8, svc_len: u32, op_ptr: *const u8, op_len: u32, req_ptr: *const u8, req_len: u32, delay_ms: i64) -> i64;

        // cleat_run_detached - ABI 2.36, two strings in
        pub fn cleat_run_detached(name_ptr: *const u8, name_len: u32, input_ptr: *const u8, input_len: u32) -> i64;

        // cleat_set_state - ABI 2.37, two strings in
        pub fn cleat_set_state(key_ptr: *const u8, key_len: u32, val_ptr: *const u8, val_len: u32) -> i64;

        // cleat_get_state - ABI 2.38, one string in, one string out
        pub fn cleat_get_state(key_ptr: *const u8, key_len: u32, out_ptr: *mut u8, max_len: u32) -> i64;

        // cleat_delete_state - ABI 2.39, one string in
        pub fn cleat_delete_state(key_ptr: *const u8, key_len: u32) -> i64;

        // cleat_incr_state - ABI 2.40, one string in, i64 delta, i64 out
        pub fn cleat_incr_state(key_ptr: *const u8, key_len: u32, delta: i64) -> i64;

        // cleat_has_state - ABI 2.41, one string in, i64 boolean out
        pub fn cleat_has_state(key_ptr: *const u8, key_len: u32) -> i64;

        // cleat_list_state - ABI 2.42, one string in (prefix), one string out
        pub fn cleat_list_state(prefix_ptr: *const u8, prefix_len: u32, out_ptr: *mut u8, max_len: u32) -> i64;

        // cleat_await_all_children - ABI 2.43, one string in (JSON run_ids), one string out
        pub fn cleat_await_all_children(run_ids_ptr: *const u8, run_ids_len: u32, out_ptr: *mut u8, max_len: u32) -> i64;

        // cleat_poll_child - ABI 2.44, one string in (run_id), one string out, non-blocking
        pub fn cleat_poll_child(
            run_id_ptr: *const u8, run_id_len: u32,
            result_ptr: *mut u8, result_max_len: u32,
        ) -> i64;

        // cleat_await_any_child - ABI 2.45, one string in (JSON run_ids), one string out, blocking
        pub fn cleat_await_any_child(
            run_ids_ptr: *const u8, run_ids_len: u32,
            result_ptr: *mut u8, result_max_len: u32,
        ) -> i64;

        // cleat_call_retry - 3 string pairs, 4 i64, 1 string pair (nonRetryableErrorsJSON), 1 string out
        #[link_name = "cleat_call_retry"]
        pub fn cleat_call_with_retry(
            svc_ptr: *const u8, svc_len: u32,
            op_ptr: *const u8, op_len: u32,
            req_ptr: *const u8, req_len: u32,
            max_attempts: i64,
            initial_interval_ms: i64,
            backoff_coefficient_100x: i64,
            max_interval_ms: i64,
            non_retryable_ptr: *const u8, non_retryable_len: u32,
            resp_ptr: *mut u8, resp_max_len: u32,
        ) -> i64;

        // cleat_fetch - ABI 2.45, 4 strings in, 1 string out
        pub fn cleat_fetch(
            method_ptr: *const u8, method_len: u32,
            url_ptr: *const u8, url_len: u32,
            headers_ptr: *const u8, headers_len: u32,
            body_ptr: *const u8, body_len: u32,
            resp_ptr: *mut u8, resp_max_len: u32,
        ) -> i64;

        // cleat_acquire_lock - ABI 2.46, one string in, i64 ttl
        pub fn cleat_acquire_lock(key_ptr: *const u8, key_len: u32, ttl_ms: i64) -> i64;

        // cleat_release_lock - ABI 2.47, one string in
        pub fn cleat_release_lock(key_ptr: *const u8, key_len: u32) -> i64;

        // cleat_continue_as_new_versioned - (ptr,len,i32) -> i64
        pub fn cleat_continue_as_new_versioned(
            input_ptr: *const u8, input_len: u32,
            new_version: i32,
        ) -> i64;

        // cleat_call_heartbeat - (ptr,len x3, i64, ptr,maxLen) -> i64
        pub fn cleat_call_heartbeat(
            svc_ptr: *const u8, svc_len: u32,
            op_ptr: *const u8, op_len: u32,
            req_ptr: *const u8, req_len: u32,
            heartbeat_interval_ms: i64,
            resp_ptr: *mut u8, resp_max_len: u32,
        ) -> i64;

        // cleat_side_effect - (ptr,len, ptr,maxLen) -> i64
        pub fn cleat_side_effect(
            result_ptr: *const u8, result_len: u32,
            out_ptr: *mut u8, out_max_len: u32,
        ) -> i64;

        // plugin_call_streaming - (ptr,len x4, ptr,maxLen) -> i64
        #[link_name = "plugin_call_streaming"]
        pub fn cleat_plugin_call_streaming(
            plugin_name_ptr: *const u8, plugin_name_len: u32,
            function_name_ptr: *const u8, function_name_len: u32,
            input_ptr: *const u8, input_len: u32,
            response_ptr: *mut u8, response_max_len: u32,
        ) -> i64;

        // cleat_json_parse - (ptr,len, ptr,maxLen) -> i64 (ABI 2.51)
        // Validates and canonicalizes JSON via the host's encoding/json.
        #[cfg(target_arch = "wasm32")]
        pub fn cleat_json_parse(
            json_ptr: *const u8, json_len: u32,
            out_ptr: *mut u8, out_max_len: u32,
        ) -> i64;

        // cleat_json_stringify - (ptr,len, ptr,maxLen) -> i64 (ABI 2.52)
        // Validates and re-serializes JSON via the host's encoding/json.
        // Identical behavior to cleat_json_parse; provided as a semantic alias.
        #[cfg(target_arch = "wasm32")]
        pub fn cleat_json_stringify(
            ptr: *const u8, len: u32,
            out_ptr: *mut u8, out_max_len: u32,
        ) -> i64;
    }
}

/// Options for starting a child workflow with version control.
/// `version = 0` means default resolution (parent's version or latest).
#[derive(Default)]
pub struct ChildWorkflowOptions {
    /// Explicit workflow definition version to use.
    /// 0 = default resolution.
    pub version: i64,
    /// Parent close policy for the child workflow (e.g. "abandon", "terminate", "request_cancel").
    pub parent_close_policy: String,
    /// Priority for scheduling child workflow execution. 0 = highest priority; lower numbers are picked first.
    pub priority: i32,
}


/// High-level Rust wrapper around the WASM host function imports.
/// Mirrors the Go `durable.HostCalls` interface.
pub struct HostCalls;

impl HostCalls {
    /// Make a durable API call. Primary name: `call`.
    /// Mirrors Go's Call. This is the preferred name.
    /// `cleat_call` is retained for backward compatibility.
    pub fn call(&self, service: &str, operation: &str, request_json: &str) -> (String, Option<String>) {
        self.cleat_call(service, operation, request_json)
    }

    /// Make a durable API call. Mirrors Go's DurableCall.
    /// Returns (response_json, error_message).
    ///
    /// Deprecated: Use `call` instead.
    pub fn cleat_call(&self, service: &str, operation: &str, request_json: &str) -> (String, Option<String>) {
        let mut resp_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_call(
                service.as_ptr(), service.len() as u32,
                operation.as_ptr(), operation.len() as u32,
                request_json.as_ptr(), request_json.len() as u32,
                resp_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (response_len, _call_error_code, err_code) = memory::decode_cleat_call_result(result);
        if err_code != 0 {
            let err_msg = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
            return (String::new(), Some(err_msg));
        }
        let resp = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
        (resp, None)
    }

    /// Sleep for a duration. Preferred over cleat_sleep_ms.
    pub fn cleat_sleep(&self, d: Duration) -> bool {
        self.cleat_sleep_ms(d.as_millis() as i64)
    }

    /// Suspend execution for a duration in milliseconds. Mirrors Go's DurableSleep.
    ///
    /// On a fresh execution the host returns status = 1 (bits 56-63) or the
    /// direct `SUSPEND_SENTINEL` value. In either case we panic with
    /// [`SuspendSentinel`] so the export wrapper can propagate the sentinel
    /// back to the engine. On replay the call returns status = 0 and we return
    /// `false` (no suspend needed).
    pub fn cleat_sleep_ms(&self, ms: i64) -> bool {
        let result = unsafe { imports::cleat_sleep(ms) };
        // Some host runtimes return SUSPEND_SENTINEL directly.
        if result == memory::SUSPEND_SENTINEL {
            std::panic::panic_any(crate::SuspendSentinel);
        }
        let (status, _) = memory::decode_sleep_result(result);
        if status == memory::SLEEP_STATUS_SUSPEND {
            std::panic::panic_any(crate::SuspendSentinel);
        }
        false
    }

    /// Get current time in milliseconds since epoch. Mirrors Go's Now().
    pub fn now(&self) -> i64 {
        unsafe { imports::cleat_now() }
    }

    /// Get deterministic random value. Mirrors Go's Random().
    pub fn random(&self) -> i64 {
        unsafe { imports::cleat_random() }
    }

    /// Log a message. Mirrors Go's DurableLog.
    pub fn cleat_log(&self, message: &str) {
        unsafe {
            imports::cleat_log(message.as_ptr(), message.len() as u32);
        }
    }

    /// Get workflow definition version. Mirrors Go's Version().
    pub fn version(&self) -> i32 {
        (unsafe { imports::cleat_version() }) as i32
    }

    /// Get minimum supported version. Mirrors Go's MinVersion().
    pub fn min_version(&self) -> i32 {
        (unsafe { imports::cleat_min_version() }) as i32
    }

    /// Register cleanup to run on exit. Mirrors Go's DurableDefer.
    pub fn cleat_defer(&self, description: &str) -> (String, Option<String>) {
        let mut id_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_defer(
                description.as_ptr(), description.len() as u32,
                id_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (id_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return (String::new(), Some(format!("defer(description=\"{}\") failed: host error code {}. Check that the defer description is valid.", description, err_code)));
        }
        let id = unsafe { memory::read_string(id_buf.as_ptr(), id_len) };
        (id, None)
    }

    /// Register cleanup WITH A BODY, to run when the workflow finishes or when
    /// the host kills it. Mirrors Go's DurableDeferFunc.
    ///
    /// `cleat_defer` above registers only a description: the host records that
    /// a defer exists, and nothing anywhere can run it. That is the whole of
    /// IMPROVEMENT-PLAN §3.73 -- the SDK documented cleanup that ran in LIFO
    /// order "analogous to Go's defer", and no mechanism existed to run it.
    /// This is the one with a closure attached.
    ///
    /// The returned ID is the host's, and is the key the body is stored under,
    /// so the two sides agree about which defer is which.
    pub fn defer_func<F: FnOnce() + 'static>(&self, f: F) -> (String, Option<String>) {
        // Refused BEFORE the host call -- IMPROVEMENT-PLAN 3.35 phase 4.
        // Registering here used to mint a real defer ID and write a durable
        // `defer` event that nothing could ever run, because run_deferred
        // drains the table before the first body starts.
        if crate::defer::in_defer_phase() {
            return (
                String::new(),
                Some(crate::defer::defer_phase_refusal("defer_func")),
            );
        }
        let (id, err) = self.cleat_defer("deferred function");
        if err.is_some() {
            return (id, err);
        }
        crate::defer::register_defer(id.clone(), Box::new(f));
        (id, None)
    }

    /// Check for cancellation. Mirrors Go's PollCancellation.
    pub fn poll_cancellation(&self) -> (bool, String) {
        let mut reason_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_poll_cancellation(
                reason_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (reason_len, cancelled) = memory::decode_poll_cancellation_result(result);
        let reason = if cancelled && reason_len > 0 {
            unsafe { memory::read_string(reason_buf.as_ptr(), reason_len) }
        } else {
            String::new()
        };
        (cancelled, reason)
    }

    /// Poll for a specific signal. Mirrors Go's PollSignal.
    pub fn poll_signal(&self, name: &str) -> (String, bool, Option<String>) {
        let mut payload_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_poll_signal(
                name.as_ptr(), name.len() as u32,
                payload_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (payload_len, found, err_code) = memory::decode_poll_signal_result(result);
        if err_code != 0 {
            return (String::new(), false, Some(format!("poll_signal(name=\"{}\") failed: host error code {}. Check that the signal name is valid.", name, err_code)));
        }
        let payload = if found && payload_len > 0 {
            unsafe { memory::read_string(payload_buf.as_ptr(), payload_len) }
        } else {
            String::new()
        };
        (payload, found, None)
    }

    /// Continue as new. Mirrors Go's ContinueAsNew.
    pub fn continue_as_new(&self, input_json: &str) -> Result<(), String> {
        // IMPROVEMENT-PLAN 3.35 phase 4. Before the host call: the workflow's
        // result is already decided by the time defers run, so a recorded
        // continuation is one the worker will never take.
        if crate::defer::in_defer_phase() {
            return Err(crate::defer::defer_phase_refusal("continue_as_new"));
        }
        let result = unsafe {
            imports::cleat_continue_as_new(
                input_json.as_ptr(), input_json.len() as u32,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("continue_as_new(...) failed: host error code {}. Check that the input JSON is valid and continue-as-new is available.", err_code));
        }
        Ok(())
    }

    /// Start a child workflow. Mirrors Go's ChildWorkflow.
    pub fn child_workflow(&self, name: &str, input_json: &str) -> (String, Option<String>) {
        let mut run_id_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_child_workflow(
                name.as_ptr(), name.len() as u32,
                input_json.as_ptr(), input_json.len() as u32,
                run_id_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (run_id_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return (String::new(), Some(format!("child_workflow(name=\"{}\") failed: host error code {}. Check that the child workflow name is correct and the workflow definition exists.", name, err_code)));
        }
        let run_id = unsafe { memory::read_string(run_id_buf.as_ptr(), run_id_len) };
        (run_id, None)
    }

    /// Start a child workflow with options. Mirrors Go's ChildWorkflowWithOptions.
    pub fn child_workflow_with_options(
        &self, name: &str, input_json: &str, opts: &ChildWorkflowOptions,
    ) -> (String, Option<String>) {
        let mut run_id_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_child_workflow_with_options(
                name.as_ptr(), name.len() as u32,
                input_json.as_ptr(), input_json.len() as u32,
                opts.version,
                opts.priority as i64,
                opts.parent_close_policy.as_ptr(), opts.parent_close_policy.len() as u32,
                run_id_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (run_id_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return (String::new(), Some(format!("child_workflow_with_options(name=\"{}\", version={}) failed: host error code {}. Check that the child workflow name is correct.", name, opts.version, err_code)));
        }
        let run_id = unsafe { memory::read_string(run_id_buf.as_ptr(), run_id_len) };
        (run_id, None)
    }


    /// Await child workflow completion. Mirrors Go's AwaitChild.
    pub fn await_child(&self, run_id: &str) -> (String, Option<String>) {
        let mut result_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let r = unsafe {
            imports::cleat_await_child(
                run_id.as_ptr(), run_id.len() as u32,
                result_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        // The host returns SUSPEND_SENTINEL when the child has not completed
        // yet — the workflow must suspend.
        if r == memory::SUSPEND_SENTINEL {
            std::panic::panic_any(crate::SuspendSentinel);
        }
        let (result_len, err_code) = memory::decode_simple_result(r);
        if err_code != 0 {
            return (String::new(), Some(format!("await_child(run_id=\"{}\") failed: host error code {}. Check that the run ID is valid.", run_id, err_code)));
        }
        let result = unsafe { memory::read_string(result_buf.as_ptr(), result_len) };
        (result, None)
    }

    /// Await external signals for a duration. Preferred over await_signals_ms.
    pub fn await_signals(&self, signal_names: &[&str], timeout: Duration) -> (String, String, bool, Option<String>) {
        self.await_signals_ms(signal_names, timeout.as_millis() as i64)
    }

    /// Await external signals in milliseconds. Mirrors Go's AwaitSignals.
    /// Returns (signal_name, payload, timed_out, error).
    pub fn await_signals_ms(&self, signal_names: &[&str], timeout_ms: i64) -> (String, String, bool, Option<String>) {
        // JSON-marshal the signal names array, matching Go's adapter.go behavior.
        let names_json = serde_json::to_string(signal_names).unwrap_or_else(|e| { eprintln!("warning: failed to serialize signal names: {}", e); "[]".to_string() });
        let mut sig_name_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let mut payload_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_await_signals(
                names_json.as_ptr(), names_json.len() as u32,
                timeout_ms,
                sig_name_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
                payload_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        // The host returns SUSPEND_SENTINEL when no signal is available and a
        // non-zero timeout has been specified — the workflow must suspend.
        if result == memory::SUSPEND_SENTINEL {
            std::panic::panic_any(crate::SuspendSentinel);
        }
        let (sig_name_len, payload_len, timed_out, err_code) = memory::decode_await_signals_result(result);
        if err_code != 0 {
            return (String::new(), String::new(), false, Some(format!("await_signals(names={}, timeout_ms={}) failed: host error code {}. Check that the signal names are valid.", names_json, timeout_ms, err_code)));
        }
        let sig_name = unsafe { memory::read_string(sig_name_buf.as_ptr(), sig_name_len as u32) };
        let payload = if !timed_out && payload_len > 0 {
            unsafe { memory::read_string(payload_buf.as_ptr(), payload_len as u32) }
        } else {
            String::new()
        };
        (sig_name, payload, timed_out, None)
    }

    /// Set query state. Mirrors Go's SetQueryState.
    pub fn set_query_state(&self, key: &str, value: &str) {
        unsafe {
            imports::set_query_state(
                key.as_ptr(), key.len() as u32,
                value.as_ptr(), value.len() as u32,
            );
        }
    }

    /// Create a durable promise. Mirrors Go's CreatePromise (ABI 2.20).
    /// Returns (promise_id, error).
    pub fn create_promise(&self, name: &str) -> (String, Option<String>) {
        let mut id_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_create_promise(
                name.as_ptr(), name.len() as u32,
                id_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (id_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return (String::new(), Some(format!("create_promise(name=\"{}\") failed: host error code {}. Check that the promise name is valid.", name, err_code)));
        }
        let id = unsafe { memory::read_string(id_buf.as_ptr(), id_len) };
        (id, None)
    }

    /// Await a durable promise for a duration. Preferred over await_promise_ms.
    pub fn await_promise(&self, promise_id: &str, timeout: Duration) -> (String, bool, Option<String>) {
        self.await_promise_ms(promise_id, timeout.as_millis() as i64)
    }

    /// Await a durable promise in milliseconds. Mirrors Go's AwaitPromise (ABI 2.21).
    /// Returns (result, timed_out, error).
    pub fn await_promise_ms(&self, promise_id: &str, timeout_ms: i64) -> (String, bool, Option<String>) {
        let mut result_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_await_promise(
                promise_id.as_ptr(), promise_id.len() as u32,
                timeout_ms,
                result_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (result_len, timed_out, err_code) = memory::decode_await_promise_result(result);
        if err_code != 0 {
            return (String::new(), timed_out, Some(format!("await_promise(promise_id=\"{}\") failed: host error code {}. Check that the promise ID is valid.", promise_id, err_code)));
        }
        let result = if result_len > 0 {
            unsafe { memory::read_string(result_buf.as_ptr(), result_len) }
        } else {
            String::new()
        };
        (result, timed_out, None)
    }

    // There is no register_query_handler here (removed 2026-08-09, previously
    // ABI 2.35 / cleat_register_query_handler). It recorded a handler name
    // with the host but nothing ever routed an external query to it -- see
    // docs/determinism.md, "Why there is no RegisterQueryHandler". Use
    // set_query_state instead, which any caller can read via
    // GET /api/workflows/:id/query?key=X regardless of whether a worker
    // currently has the workflow loaded.

    /// Register an update handler. Mirrors Go's RegisterUpdateHandler (ABI 2.22).
    pub fn register_update_handler(&self, name: &str) {
        unsafe {
            imports::cleat_register_update_handler(
                name.as_ptr(), name.len() as u32,
            );
        }
    }

    /// Call a plugin host function. Mirrors Go's PluginCall (ABI 2.19).
    /// Returns (response_json, error_message).
    ///
    /// Plugin calls target Go host plugins rather than external services.
    /// They are journaled for deterministic replay, same as [`call`](Self::call).
    pub fn plugin_call(&self, plugin_name: &str, function_name: &str, input_json: &str) -> (String, Option<String>) {
        let mut resp_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_plugin_call(
                plugin_name.as_ptr(), plugin_name.len() as u32,
                function_name.as_ptr(), function_name.len() as u32,
                input_json.as_ptr(), input_json.len() as u32,
                resp_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (response_len, _call_error_code, err_code) = memory::decode_cleat_call_result(result);
        if err_code != 0 {
            let err_msg = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
            return (String::new(), Some(err_msg));
        }
        let resp = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
        (resp, None)
    }

    /// Typed variant of [`plugin_call`](Self::plugin_call).
    /// Deserialises the JSON response into `T`.
    pub fn plugin_call_typed<T: serde::de::DeserializeOwned>(
        &self,
        plugin_name: &str,
        function_name: &str,
        input_json: &str,
    ) -> Result<T, String> {
        let (resp, err) = self.plugin_call(plugin_name, function_name, input_json);
        if let Some(e) = err {
            return Err(e);
        }
        serde_json::from_str(&resp).map_err(|e| format!("deserializing plugin response: {}", e))
    }

    /// Send a signal to a target workflow and wait for a response with a timeout duration. Preferred over send_signal_and_wait_ms.
    pub fn send_signal_and_wait(&self, target_run_id: &str, signal_name: &str, payload: &str, timeout: Duration) -> Result<String, String> {
        self.send_signal_and_wait_ms(target_run_id, signal_name, payload, timeout.as_millis() as i64)
    }

    /// Send a signal to a target workflow and wait for a response in milliseconds. Mirrors Go's SendSignalAndWait.
    pub fn send_signal_and_wait_ms(&self, target_run_id: &str, signal_name: &str, payload: &str, timeout_ms: i64) -> Result<String, String> {
        let mut resp_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_send_signal_and_wait(
                target_run_id.as_ptr(), target_run_id.len() as u32,
                signal_name.as_ptr(), signal_name.len() as u32,
                payload.as_ptr(), payload.len() as u32,
                timeout_ms,
                resp_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        // The host may return SUSPEND_SENTINEL when the target has not responded yet.
        if result == memory::SUSPEND_SENTINEL {
            std::panic::panic_any(crate::SuspendSentinel);
        }
        let (response_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            let err_msg = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
            return Err(err_msg);
        }
        let resp = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
        Ok(resp)
    }

    /// Reply to a signal, sending a response back to the sender.
    /// Mirrors Go's ReplyToSignal.
    pub fn reply_to_signal(&self, correlation_id: &str, response: &str) -> Result<(), String> {
        let result = unsafe {
            imports::cleat_reply_to_signal(
                correlation_id.as_ptr(), correlation_id.len() as u32,
                response.as_ptr(), response.len() as u32,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("reply_to_signal(correlation_id=\"{}\") failed: host error code {}. Check that the correlation ID is valid.", correlation_id, err_code));
        }
        Ok(())
    }

    /// Wait for at least min_count signals from the named set, with rejection tracking.
    /// Mirrors Go's AwaitSignalsWithQuorum.
    /// Returns Ok(Vec<SignalResult>) on success, or Err(String) on error.
    pub fn await_signals_with_quorum(&self, signal_names: &[String], min_count: i32, max_rejections: i32, timeout_ms: i64) -> Result<Vec<SignalResult>, String> {
        let deadline = std::time::Instant::now() + std::time::Duration::from_millis(timeout_ms as u64);
        let mut results: Vec<SignalResult> = Vec::new();
        let mut rejection_count = 0;

        while (results.len() as i32) < min_count {
            let remaining = deadline.saturating_duration_since(std::time::Instant::now());
            if remaining.as_millis() == 0 {
                return Err(format!("quorum timeout after {}ms: got {}/{} signals", timeout_ms, results.len(), min_count));
            }

            // Gather signal names not yet received as a &[&str].
            let remaining_names: Vec<&str> = signal_names.iter().map(|s| s.as_str()).collect();
            let (name, payload, timed_out, err) = self.await_signals_ms(&remaining_names, remaining.as_millis() as i64);
            if let Some(e) = err {
                return Err(format!("quorum signal error: {}", e));
            }
            if timed_out {
                return Err(format!("quorum timeout after {}ms: got {}/{} signals", timeout_ms, results.len(), min_count));
            }

            results.push(SignalResult {
                name,
                payload: payload.clone(),
                timed_out: false,
            });

            // Check for rejection if max_rejections >= 0.
            if max_rejections >= 0 && !payload.is_empty() {
                if let Ok(payload_map) = serde_json::from_str::<serde_json::Value>(&payload) {
                    if let Some(rejected) = payload_map.get("rejected").and_then(|v| v.as_bool()) {
                        if rejected {
                            rejection_count += 1;
                            if rejection_count > max_rejections {
                                return Err(format!("quorum exceeded max rejections ({})", max_rejections));
                            }
                        }
                    }
                }
            }
        }

        Ok(results)
    }

    /// Send a signal to a target workflow (fire-and-forget).
    /// Mirrors Go's SignalWorkflow.
    pub fn signal_workflow(&self, target_run_id: &str, signal_name: &str, payload: &str) -> Result<(), String> {
        let result = unsafe {
            imports::cleat_signal_workflow(
                target_run_id.as_ptr(), target_run_id.len() as u32,
                signal_name.as_ptr(), signal_name.len() as u32,
                payload.as_ptr(), payload.len() as u32,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("signal_workflow(target_run_id=\"{}\", signal_name=\"{}\") failed: host error code {}. Check that the target run ID and signal name are valid.", target_run_id, signal_name, err_code));
        }
        Ok(())
    }

    /// Set the virtual object scope. Returns the previous scope.
    /// Mirrors Go's SetScope.
    pub fn set_scope(&self, object_type: &str, instance_key: &str) -> String {
        let mut prev_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_set_scope(
                object_type.as_ptr(), object_type.len() as u32,
                instance_key.as_ptr(), instance_key.len() as u32,
                prev_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (prev_len, _err_code) = memory::decode_simple_result(result);
        if prev_len > 0 {
            unsafe { memory::read_string(prev_buf.as_ptr(), prev_len) }
        } else {
            String::new()
        }
    }

    /// Get the current virtual object scope.
    /// Mirrors Go's GetScope.
    pub fn get_scope(&self) -> (String, String) {
        let mut obj_type_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let mut inst_key_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_get_scope(
                obj_type_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
                inst_key_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (obj_type_len, inst_key_len) = memory::decode_get_scope_result(result);
        let obj_type = if obj_type_len > 0 {
            unsafe { memory::read_string(obj_type_buf.as_ptr(), obj_type_len) }
        } else {
            String::new()
        };
        let inst_key = if inst_key_len > 0 {
            unsafe { memory::read_string(inst_key_buf.as_ptr(), inst_key_len) }
        } else {
            String::new()
        };
        (obj_type, inst_key)
    }

    /// Clear the virtual object scope. Returns the previous scope.
    /// Mirrors Go's ClearScope.
    pub fn clear_scope(&self) -> String {
        // Clear scope by setting it to empty strings.
        let mut prev_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_set_scope(
                std::ptr::null(), 0u32,
                std::ptr::null(), 0u32,
                prev_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (prev_len, _err_code) = memory::decode_simple_result(result);
        if prev_len > 0 {
            unsafe { memory::read_string(prev_buf.as_ptr(), prev_len) }
        } else {
            String::new()
        }
    }

    /// Generate a deterministic UUID from a seed.
    /// Mirrors Go's UUID.
    pub fn uuid(&self, seed: &str) -> String {
        let mut uuid_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_uuid(
                seed.as_ptr(), seed.len() as u32,
                uuid_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (uuid_len, _err_code) = memory::decode_simple_result(result);
        if uuid_len > 0 {
            unsafe { memory::read_string(uuid_buf.as_ptr(), uuid_len) }
        } else {
            String::new()
        }
    }

    /// Get the current workflow ID.
    pub fn workflow_id(&self) -> String {
        let mut buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_workflow_id(buf.as_mut_ptr(), memory::OUT_BUF_SIZE)
        };
        let (id_len, _err_code) = memory::decode_simple_result(result);
        if id_len > 0 {
            unsafe { memory::read_string(buf.as_ptr(), id_len) }
        } else {
            String::new()
        }
    }

    /// Get the current run ID.
    pub fn run_id(&self) -> String {
        let mut buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_run_id(buf.as_mut_ptr(), memory::OUT_BUF_SIZE)
        };
        let (id_len, _err_code) = memory::decode_simple_result(result);
        if id_len > 0 {
            unsafe { memory::read_string(buf.as_ptr(), id_len) }
        } else {
            String::new()
        }
    }

    /// Resolve a promise with a value. Mirrors Go's ResolvePromise.
    pub fn resolve_promise(&self, promise_id: &str, value: &str) -> Result<(), String> {
        let result = unsafe {
            imports::cleat_resolve_promise(
                promise_id.as_ptr(), promise_id.len() as u32,
                value.as_ptr(), value.len() as u32,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("resolve_promise(promise_id=\"{}\") failed: host error code {}. Check that the promise ID is valid.", promise_id, err_code));
        }
        Ok(())
    }

    /// Reject a promise with an error. Mirrors Go's RejectPromise.
    pub fn reject_promise(&self, promise_id: &str, error: &str) -> Result<(), String> {
        let result = unsafe {
            imports::cleat_reject_promise(
                promise_id.as_ptr(), promise_id.len() as u32,
                error.as_ptr(), error.len() as u32,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("reject_promise(promise_id=\"{}\") failed: host error code {}. Check that the promise ID is valid.", promise_id, err_code));
        }
        Ok(())
    }

    /// Fire-and-forget durable call. Mirrors Go's DurableSend.
    pub fn cleat_send(&self, service: &str, operation: &str, request_json: &str) -> Result<(), String> {
        let result = unsafe {
            imports::cleat_send(
                service.as_ptr(), service.len() as u32,
                operation.as_ptr(), operation.len() as u32,
                request_json.as_ptr(), request_json.len() as u32,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("cleat_send(service=\"{}\", operation=\"{}\") failed: host error code {}. Check that the service and operation names are valid.", service, operation, err_code));
        }
        Ok(())
    }

    /// Schedule an invoke with a delay duration. Preferred over schedule_invoke_ms.
    pub fn schedule_invoke(&self, service: &str, operation: &str, request_json: &str, delay: Duration) -> Result<(), String> {
        self.schedule_invoke_ms(service, operation, request_json, delay.as_millis() as i64)
    }

    /// Schedule an invoke with a delay in milliseconds. Mirrors Go's ScheduleInvoke.
    pub fn schedule_invoke_ms(&self, service: &str, operation: &str, request_json: &str, delay_ms: i64) -> Result<(), String> {
        let result = unsafe {
            imports::schedule_invoke(
                service.as_ptr(), service.len() as u32,
                operation.as_ptr(), operation.len() as u32,
                request_json.as_ptr(), request_json.len() as u32,
                delay_ms,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("schedule_invoke(service=\"{}\", operation=\"{}\") failed: host error code {}. Check that the service and operation are valid.", service, operation, err_code));
        }
        Ok(())
    }

    /// Run a child workflow detached (fire-and-forget). Mirrors Go's RunDetached.
    pub fn run_detached(&self, name: &str, input_json: &str) -> Result<(), String> {
        let result = unsafe {
            imports::cleat_run_detached(
                name.as_ptr(), name.len() as u32,
                input_json.as_ptr(), input_json.len() as u32,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("run_detached(name=\"{}\") failed: host error code {}. Check that the workflow name is correct.", name, err_code));
        }
        Ok(())
    }

    /// Set a state value by key. Mirrors Go's SetState.
    pub fn set_state(&self, key: &str, value: &str) -> Result<(), String> {
        let result = unsafe {
            imports::cleat_set_state(
                key.as_ptr(), key.len() as u32,
                value.as_ptr(), value.len() as u32,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("set_state(key=\"{}\", ...) failed: host error code {}. Check that the key is valid and state operations are available.", key, err_code));
        }
        Ok(())
    }

    /// Get a state value by key. Mirrors Go's GetState.
    pub fn get_state(&self, key: &str) -> Result<String, String> {
        let mut buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_get_state(
                key.as_ptr(), key.len() as u32,
                buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (val_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("get_state(key=\"{}\") failed: host error code {}. Check that the key exists and state operations are available.", key, err_code));
        }
        let val = unsafe { memory::read_string(buf.as_ptr(), val_len) };
        Ok(val)
    }

    /// Delete a state key. Mirrors Go's DeleteState.
    pub fn delete_state(&self, key: &str) -> Result<(), String> {
        let result = unsafe {
            imports::cleat_delete_state(
                key.as_ptr(), key.len() as u32,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("delete_state(key=\"{}\") failed: host error code {}. Check that the key is valid and state operations are available.", key, err_code));
        }
        Ok(())
    }

    /// Atomically increment a state counter by delta. Returns the new value.
    pub fn incr_state(&self, key: &str, delta: i64) -> Result<i64, String> {
        let result = unsafe {
            imports::cleat_incr_state(
                key.as_ptr(), key.len() as u32,
                delta,
            )
        };
        let (new_value, err_code) = memory::decode_incr_state_result(result);
        if err_code != 0 {
            return Err(format!("incr_state(key=\"{}\", delta={}) failed: host error code {}. Check that the key is valid for numeric operations.", key, delta, err_code));
        }
        Ok(new_value)
    }

    /// Check if a state key exists.
    pub fn has_state(&self, key: &str) -> bool {
        let result = unsafe {
            imports::cleat_has_state(
                key.as_ptr(), key.len() as u32,
            )
        };
        memory::decode_has_state_result(result)
    }

    /// List state keys with a given prefix. Returns deserialized JSON array of key names.
    pub fn list_state(&self, prefix: &str) -> Result<Vec<String>, String> {
        let mut buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_list_state(
                prefix.as_ptr(), prefix.len() as u32,
                buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (data_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("list_state(prefix=\"{}\") failed: host error code {}. Check that state operations are available.", prefix, err_code));
        }
        let json_str = unsafe { memory::read_string(buf.as_ptr(), data_len) };
        serde_json::from_str(&json_str).map_err(|e| format!("list_state parse error: {}", e))
    }

    /// Await all children workflows. Returns aggregated JSON results.
    pub fn await_all_children(&self, run_ids: &[&str]) -> Result<String, String> {
        let run_ids_json = serde_json::to_string(run_ids).map_err(|e| format!("serialize run_ids: {}", e))?;
        let mut buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_await_all_children(
                run_ids_json.as_ptr(), run_ids_json.len() as u32,
                buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        if result == memory::SUSPEND_SENTINEL {
            std::panic::panic_any(crate::SuspendSentinel);
        }
        let (result_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("await_all_children(run_ids={}) failed: host error code {}. Check that the run IDs are valid.", run_ids_json, err_code));
        }
        let resp = unsafe { memory::read_string(buf.as_ptr(), result_len) };
        Ok(resp)
    }

    /// Poll a single child workflow for completion without suspending. Returns (result, error).
    pub fn poll_child(&self, run_id: &str) -> (String, Option<String>) {
        let mut result_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let r = unsafe {
            imports::cleat_poll_child(
                run_id.as_ptr(), run_id.len() as u32,
                result_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (result_len, err_code) = memory::decode_simple_result(r);
        if err_code != 0 {
            return (String::new(), Some(format!("poll_child(run_id=\"{}\") failed: host error code {}. Check that the run ID is valid.", run_id, err_code)));
        }
        let result = unsafe { memory::read_string(result_buf.as_ptr(), result_len) };
        (result, None)
    }

    /// Await any of the given child workflows to complete. Returns the result JSON.
    pub fn await_any_child(&self, run_ids: &[&str]) -> Result<String, String> {
        let run_ids_json = serde_json::to_string(run_ids).map_err(|e| format!("serialize run_ids: {}", e))?;
        let mut buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_await_any_child(
                run_ids_json.as_ptr(), run_ids_json.len() as u32,
                buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        if result == memory::SUSPEND_SENTINEL {
            std::panic::panic_any(crate::SuspendSentinel);
        }
        let (result_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("await_any_child(run_ids={}) failed: host error code {}. Check that the run IDs are valid.", run_ids_json, err_code));
        }
        let resp = unsafe { memory::read_string(buf.as_ptr(), result_len) };
        Ok(resp)
    }

    /// Typed version of child_workflow using serde for serialization.
    pub fn child_workflow_typed<T: serde::Serialize>(&self, name: &str, input: &T) -> Result<String, String> {
        let input_json = serde_json::to_string(input).map_err(|e| format!("serialize input: {}", e))?;
        let (run_id, err) = self.child_workflow(name, &input_json);
        if let Some(e) = err {
            return Err(e);
        }
        Ok(run_id)
    }

    /// Typed version of await_child using serde for deserialization.
    pub fn await_child_typed<T: serde::de::DeserializeOwned>(&self, run_id: &str) -> Result<T, String> {
        let (result_json, err) = self.await_child(run_id);
        if let Some(e) = err {
            return Err(e);
        }
        serde_json::from_str(&result_json).map_err(|e| format!("deserialize result: {}", e))
    }

    /// Typed version of cleat_call using serde for serialization/deserialization.
    pub fn cleat_call_typed<T: serde::Serialize, R: serde::de::DeserializeOwned>(
        &self, service: &str, operation: &str, request: &T,
    ) -> Result<R, String> {
        let request_json = serde_json::to_string(request).map_err(|e| format!("serialize request: {}", e))?;
        let (resp_json, err) = self.cleat_call(service, operation, &request_json);
        if let Some(e) = err {
            return Err(e);
        }
        serde_json::from_str(&resp_json).map_err(|e| format!("deserialize response: {}", e))
    }

    /// Durable call with a retry policy.
    pub fn cleat_call_with_retry<T: serde::Serialize, R: serde::de::DeserializeOwned>(
        &self, service: &str, operation: &str, request: &T, retry_policy: &RetryPolicy,
    ) -> Result<R, String> {
        let request_json = serde_json::to_string(request).map_err(|e| format!("serialize request: {}", e))?;
        let non_retryable_json = serde_json::to_string(&retry_policy.non_retryable_errors)
            .map_err(|e| format!("serialize non-retryable errors: {}", e))?;
        let backoff_coefficient_100x = (retry_policy.backoff_multiplier * 100.0) as i64;
        let mut resp_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_call_with_retry(
                service.as_ptr(), service.len() as u32,
                operation.as_ptr(), operation.len() as u32,
                request_json.as_ptr(), request_json.len() as u32,
                retry_policy.max_attempts as i64,
                retry_policy.initial_interval_ms as i64,
                backoff_coefficient_100x,
                retry_policy.maximum_interval_ms as i64,
                non_retryable_json.as_ptr(), non_retryable_json.len() as u32,
                resp_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (response_len, _call_error_code, err_code) = memory::decode_cleat_call_result(result);
        if err_code != 0 {
            let err_msg = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
            return Err(err_msg);
        }
        let resp = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
        serde_json::from_str(&resp).map_err(|e| format!("deserialize response: {}", e))
    }

    /// Make an HTTP fetch request to an external endpoint.
    /// The headers parameter is a JSON object of string key-value pairs.
    /// Returns a FetchResult with status, headers, and body.
    pub fn cleat_fetch(
        &self, method: &str, url: &str, headers: &str, body: &str,
    ) -> Result<FetchResult, String> {
        let mut resp_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_fetch(
                method.as_ptr(), method.len() as u32,
                url.as_ptr(), url.len() as u32,
                headers.as_ptr(), headers.len() as u32,
                body.as_ptr(), body.len() as u32,
                resp_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (response_len, _call_error_code, err_code) = memory::decode_cleat_call_result(result);
        if err_code != 0 {
            let err_msg = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
            return Err(err_msg);
        }
        let resp = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
        serde_json::from_str(&resp).map_err(|e| format!("parse fetch response: {}", e))
    }

    /// Convenience method for HTTP GET requests.
    pub fn fetch_get(&self, url: &str) -> Result<FetchResult, String> {
        self.cleat_fetch("GET", url, "{}", "")
    }

    /// Acquire a concurrency lock with a TTL duration. Preferred over acquire_lock_ms.
    pub fn acquire_lock(&self, key: &str, ttl: Duration) -> (bool, Option<String>) {
        self.acquire_lock_ms(key, ttl.as_millis() as i64)
    }

    /// Acquire a concurrency lock in milliseconds. Mirrors Go's AcquireLockMs.
    /// Returns (acquired, error_message).
    pub fn acquire_lock_ms(&self, key: &str, ttl_ms: i64) -> (bool, Option<String>) {
        let result = unsafe {
            imports::cleat_acquire_lock(
                key.as_ptr(), key.len() as u32,
                ttl_ms,
            )
        };
        let err_code = (result as u64 & 0xFF) as u8;
        let acquired = ((result as u64 >> 8) & 0x1) != 0;
        if err_code != 0 {
            return (false, Some(format!("acquire_lock(key=\"{}\", ttl_ms={}) failed: host error code {}. Check that the lock key is valid.", key, ttl_ms, err_code)));
        }
        (acquired, None)
    }

    /// Release a concurrency lock previously acquired by this workflow.
    /// Mirrors Go's ReleaseLock.
    /// Returns None on success, or Some(error_message) on failure.
    pub fn release_lock(&self, key: &str) -> Option<String> {
        let result = unsafe {
            imports::cleat_release_lock(
                key.as_ptr(), key.len() as u32,
            )
        };
        let err_code = (result as u64 & 0xFF) as u8;
        if err_code != 0 {
            return Some(format!("release_lock(key=\"{}\") failed: host error code {}. Check that the lock key is valid and the lock is held.", key, err_code));
        }
        None
    }

    // ── R1.1: continue_as_new_versioned ───────────────────────────────────

    /// Continue as new with an explicit version. Mirrors Go's ContinueAsNewWithVersion.
    pub fn continue_as_new_versioned(&self, input_json: &str, new_version: i32) -> Result<(), String> {
        // IMPROVEMENT-PLAN 3.35 phase 4. Before the host call: the workflow's
        // result is already decided by the time defers run, so a recorded
        // continuation is one the worker will never take.
        if crate::defer::in_defer_phase() {
            return Err(crate::defer::defer_phase_refusal("continue_as_new_versioned"));
        }
        let result = unsafe {
            imports::cleat_continue_as_new_versioned(
                input_json.as_ptr(), input_json.len() as u32,
                new_version,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("continue_as_new_versioned(...) failed: host error code {}. Check that the input JSON is valid.", err_code));
        }
        Ok(())
    }

    // ── R1.2: cleat_call_heartbeat ────────────────────────────────────────

    /// Durable call with heartbeat. Mirrors Go's DurableCallWithHeartbeat.
    /// Returns (response_json, error_message).
    pub fn cleat_call_heartbeat(
        &self, service: &str, operation: &str, request_json: &str, heartbeat_interval_ms: i64,
    ) -> (String, Option<String>) {
        let mut resp_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_call_heartbeat(
                service.as_ptr(), service.len() as u32,
                operation.as_ptr(), operation.len() as u32,
                request_json.as_ptr(), request_json.len() as u32,
                heartbeat_interval_ms,
                resp_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (response_len, _call_error_code, err_code) = memory::decode_cleat_call_result(result);
        if err_code != 0 {
            let err_msg = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
            return (String::new(), Some(err_msg));
        }
        let resp = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
        (resp, None)
    }

    /// Typed version of cleat_call_heartbeat using serde for serialization/deserialization.
    pub fn cleat_call_heartbeat_typed<T: serde::Serialize, R: serde::de::DeserializeOwned>(
        &self, service: &str, operation: &str, request: &T, heartbeat_interval_ms: i64,
    ) -> Result<R, String> {
        let request_json = serde_json::to_string(request).map_err(|e| format!("serialize request: {}", e))?;
        let (resp_json, err) = self.cleat_call_heartbeat(service, operation, &request_json, heartbeat_interval_ms);
        if let Some(e) = err {
            return Err(e);
        }
        serde_json::from_str(&resp_json).map_err(|e| format!("deserialize response: {}", e))
    }

    // ── R1.3: cleat_side_effect ──────────────────────────────────────────

    /// Record a non-deterministic computation result in event history on first
    /// execution, returning the cached result on replay. Mirrors Go's SideEffect.
    pub fn side_effect(&self, computed_result: &str) -> Result<String, String> {
        let mut out_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_side_effect(
                computed_result.as_ptr(), computed_result.len() as u32,
                out_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (out_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("side_effect(...) failed: host error code {}. Check that the input is valid.", err_code));
        }
        let out = unsafe { memory::read_string(out_buf.as_ptr(), out_len) };
        Ok(out)
    }

    /// Typed version of side_effect using serde for serialization/deserialization.
    pub fn side_effect_typed<T: serde::Serialize, R: serde::de::DeserializeOwned>(
        &self, computed: &T,
    ) -> Result<R, String> {
        let input_json = serde_json::to_string(computed).map_err(|e| format!("serialize input: {}", e))?;
        let result_json = self.side_effect(&input_json)?;
        serde_json::from_str(&result_json).map_err(|e| format!("deserialize result: {}", e))
    }

    // ── R1.4: plugin_call_streaming ──────────────────────────────────────

    /// Call a plugin function with streaming support. Mirrors Go's PluginCallStreaming.
    /// Returns (response_json, error_message).
    pub fn plugin_call_streaming(
        &self, plugin_name: &str, function_name: &str, input_json: &str,
    ) -> (String, Option<String>) {
        let mut resp_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_plugin_call_streaming(
                plugin_name.as_ptr(), plugin_name.len() as u32,
                function_name.as_ptr(), function_name.len() as u32,
                input_json.as_ptr(), input_json.len() as u32,
                resp_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (response_len, _call_error_code, err_code) = memory::decode_cleat_call_result(result);
        if err_code != 0 {
            let err_msg = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
            return (String::new(), Some(err_msg));
        }
        let resp = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
        (resp, None)
    }

    /// Typed version of plugin_call_streaming using serde for serialization/deserialization.
    pub fn plugin_call_streaming_typed<T: serde::Serialize, R: serde::de::DeserializeOwned>(
        &self, plugin_name: &str, function_name: &str, request: &T,
    ) -> Result<R, String> {
        let request_json = serde_json::to_string(request).map_err(|e| format!("serialize request: {}", e))?;
        let (resp_json, err) = self.plugin_call_streaming(plugin_name, function_name, &request_json);
        if let Some(e) = err {
            return Err(e);
        }
        serde_json::from_str(&resp_json).map_err(|e| format!("deserialize response: {}", e))
    }

    /// Validate and canonicalize a JSON string via the host's encoding/json.
    ///
    /// Parses the input JSON, then re-serializes it with sorted keys and
    /// canonical formatting. Returns `Some(normalized_json)` on success,
    /// `None` if the input is not valid JSON or exceeds the 65536-byte
    /// output buffer.
    #[cfg(target_arch = "wasm32")]
    pub fn json_parse(&self, json: &str) -> Option<String> {
        let mut out_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_json_parse(
                json.as_ptr(), json.len() as u32,
                out_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (written, err_code) = memory::decode_simple_result(result);
        if err_code != 0 || written == 0 {
            return None;
        }
        Some(unsafe { memory::read_string(out_buf.as_ptr(), written) })
    }

    /// Non-WASM stub for `json_parse`.
    /// Returns the input unchanged (no host normalization available on native targets).
    #[cfg(not(target_arch = "wasm32"))]
    pub fn json_parse(&self, json: &str) -> Option<String> {
        Some(json.to_string())
    }

    /// Validate and re-serialize a JSON value via the host's encoding/json.
    ///
    /// Identical behavior to `json_parse` — both parse then re-serialize
    /// through the host's `encoding/json`. Provided as a semantic alias for
    /// SDK ergonomics. Returns `Some(normalized_json)` on success,
    /// `None` if the input is not valid JSON or exceeds the 65536-byte
    /// output buffer.
    #[cfg(target_arch = "wasm32")]
    pub fn json_stringify(&self, value: &str) -> Option<String> {
        let mut out_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::cleat_json_stringify(
                value.as_ptr(), value.len() as u32,
                out_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (written, err_code) = memory::decode_simple_result(result);
        if err_code != 0 || written == 0 {
            return None;
        }
        Some(unsafe { memory::read_string(out_buf.as_ptr(), written) })
    }

    /// Non-WASM stub for `json_stringify`.
    /// Returns the input unchanged (no host normalization available on native targets).
    #[cfg(not(target_arch = "wasm32"))]
    pub fn json_stringify(&self, value: &str) -> Option<String> {
        Some(value.to_string())
    }
}

/// Result of an await_signals call.
#[derive(Debug, Clone)]
pub struct SignalResult {
    pub name: String,
    pub payload: String,
    pub timed_out: bool,
}

/// Retry policy for cleat_call_with_retry.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RetryPolicy {
    pub max_attempts: u32,
    pub initial_interval_ms: u64,
    pub backoff_multiplier: f64,
    pub maximum_interval_ms: u64,
    /// List of error messages/substrings that should not be retried.
    /// Serialized as JSON array when passed to the host.
    #[serde(default)]
    pub non_retryable_errors: Vec<String>,
}

/// Result from an HTTP fetch request.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FetchResult {
    pub status: u16,
    #[serde(default)]
    pub headers: HashMap<String, String>,
    #[serde(default)]
    pub body: String,
}
