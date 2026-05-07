// HostCalls: Rust wrapper around the 18 WASM host function imports
// matching the cleat host runtime ABI from internal/host/imports.go.

use crate::memory;

/// All 18 host function imports from the "env" WASM module.
/// Each returns i64 with a bit-packed result. Strings cross as (ptr, len) pairs.
mod imports {
    #[link(wasm_import_module = "env")]
    extern "C" {
        // durablesleep - single i64 param
        pub fn durable_sleep(duration_ms: i64) -> i64;

        // durablenow - no params
        pub fn durable_now() -> i64;

        // durablerandom - no params
        pub fn durable_random() -> i64;

        // durablelog - one string in
        pub fn durable_log(msg_ptr: *const u8, msg_len: u32) -> i64;

        // durableversion - no params
        pub fn durable_version() -> i64;

        // durablemin_version - no params
        pub fn durable_min_version() -> i64;

        // durabledefer - one string in, one string out
        pub fn durable_defer(
            desc_ptr: *const u8, desc_len: u32,
            defer_id_ptr: *mut u8, defer_id_max_len: u32,
        ) -> i64;

        // durablepoll_cancellation - one string out
        pub fn durable_poll_cancellation(
            reason_ptr: *mut u8, reason_max_len: u32,
        ) -> i64;

        // durablepoll_signal - one string in, one string out
        pub fn durable_poll_signal(
            name_ptr: *const u8, name_len: u32,
            payload_ptr: *mut u8, payload_max_len: u32,
        ) -> i64;

        // durablecontinue_as_new - one string in
        pub fn durable_continue_as_new(
            input_ptr: *const u8, input_len: u32,
        ) -> i64;

        // durablechild_workflow - two strings in, one string out
        pub fn durable_child_workflow(
            name_ptr: *const u8, name_len: u32,
            input_ptr: *const u8, input_len: u32,
            run_id_ptr: *mut u8, run_id_max_len: u32,
        ) -> i64;

        // durableawait_child - one string in, one string out
        pub fn durable_await_child(
            run_id_ptr: *const u8, run_id_len: u32,
            result_ptr: *mut u8, result_max_len: u32,
        ) -> i64;

        // durableawait_signals - strings + i64 mixed
        pub fn durable_await_signals(
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
        pub fn durable_call(
            svc_ptr: *const u8, svc_len: u32,
            op_ptr: *const u8, op_len: u32,
            req_ptr: *const u8, req_len: u32,
            resp_ptr: *mut u8, resp_max_len: u32,
        ) -> i64;

        // durablecreate_promise - one string in, one string out (ABI 2.20)
        pub fn durable_create_promise(
            name_ptr: *const u8, name_len: u32,
            id_out_ptr: *mut u8, id_out_max: u32,
        ) -> i64;

        // durableawait_promise - one string in, i64 timeout, one string out (ABI 2.21)
        pub fn durable_await_promise(
            id_ptr: *const u8, id_len: u32,
            timeout_ms: i64,
            result_out_ptr: *mut u8, result_out_max: u32,
        ) -> i64;

        // durableregister_update_handler - one string in, void return (ABI 2.22)
        pub fn durable_register_update_handler(
            name_ptr: *const u8, name_len: u32,
        ) -> i64;

        // durable_send_signal_and_wait - 3 strings in, i64 timeout, 1 string out (ABI 2.23)
        pub fn durable_send_signal_and_wait(
            target_ptr: *const u8, target_len: u32,
            signal_ptr: *const u8, signal_len: u32,
            payload_ptr: *const u8, payload_len: u32,
            timeout_ms: i64,
            response_ptr: *mut u8, response_max_len: u32,
        ) -> i64;

        // durable_reply_to_signal - 2 strings in (ABI 2.24)
        pub fn durable_reply_to_signal(
            correlation_ptr: *const u8, correlation_len: u32,
            response_ptr: *const u8, response_len: u32,
        ) -> i64;

        // durable_signal_workflow - 3 strings in (ABI 2.25)
        pub fn durable_signal_workflow(
            target_ptr: *const u8, target_len: u32,
            signal_ptr: *const u8, signal_len: u32,
            payload_ptr: *const u8, payload_len: u32,
        ) -> i64;

        // durable_set_scope - 2 strings in, 1 string out (ABI 2.26)
        pub fn durable_set_scope(
            obj_type_ptr: *const u8, obj_type_len: u32,
            inst_key_ptr: *const u8, inst_key_len: u32,
            prev_scope_ptr: *mut u8, prev_scope_max_len: u32,
        ) -> i64;

        // durable_get_scope - 2 string buffers out (ABI 2.27)
        pub fn durable_get_scope(
            obj_type_ptr: *mut u8, obj_type_max_len: u32,
            inst_key_ptr: *mut u8, inst_key_max_len: u32,
        ) -> i64;

        // durable_uuid - 1 string in, 1 string out (ABI 2.28)
        pub fn durable_uuid(
            seed_ptr: *const u8, seed_len: u32,
            uuid_ptr: *mut u8, uuid_max_len: u32,
        ) -> i64;

        // plugin_call - ABI 2.19 (WASM import name is "plugin_call")
        #[link_name = "plugin_call"]
        pub fn durable_plugin_call(
            plugin_name_ptr: *const u8, plugin_name_len: u32,
            function_name_ptr: *const u8, function_name_len: u32,
            input_ptr: *const u8, input_len: u32,
            response_ptr: *mut u8, response_max_len: u32,
        ) -> i64;
    }
}

/// High-level Rust wrapper around the WASM host function imports.
/// Mirrors the Go `durable.HostCalls` interface.
pub struct HostCalls;

impl HostCalls {
    /// Make a durable API call. Mirrors Go's DurableCall.
    /// Returns (response_json, error_message).
    pub fn durable_call(&self, service: &str, operation: &str, request_json: &str) -> (String, Option<String>) {
        let mut resp_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::durable_call(
                service.as_ptr(), service.len() as u32,
                operation.as_ptr(), operation.len() as u32,
                request_json.as_ptr(), request_json.len() as u32,
                resp_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (response_len, _call_error_code, err_code) = memory::decode_durable_call_result(result);
        if err_code != 0 {
            let err_msg = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
            return (String::new(), Some(err_msg));
        }
        let resp = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
        (resp, None)
    }

    /// Suspend execution for a duration. Mirrors Go's DurableSleep.
    ///
    /// On a fresh execution the host returns status = 1 (bits 56-63) or the
    /// direct `SUSPEND_SENTINEL` value. In either case we panic with
    /// [`SuspendSentinel`] so the export wrapper can propagate the sentinel
    /// back to the engine. On replay the call returns status = 0 and we return
    /// `false` (no suspend needed).
    pub fn durable_sleep(&self, duration_ms: i64) -> bool {
        let result = unsafe { imports::durable_sleep(duration_ms) };
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
        unsafe { imports::durable_now() }
    }

    /// Get deterministic random value. Mirrors Go's Random().
    pub fn random(&self) -> i64 {
        unsafe { imports::durable_random() }
    }

    /// Log a message. Mirrors Go's DurableLog.
    pub fn durable_log(&self, message: &str) {
        unsafe {
            imports::durable_log(message.as_ptr(), message.len() as u32);
        }
    }

    /// Get workflow definition version. Mirrors Go's Version().
    pub fn version(&self) -> i32 {
        (unsafe { imports::durable_version() }) as i32
    }

    /// Get minimum supported version. Mirrors Go's MinVersion().
    pub fn min_version(&self) -> i32 {
        (unsafe { imports::durable_min_version() }) as i32
    }

    /// Register cleanup to run on exit. Mirrors Go's DurableDefer.
    pub fn durable_defer(&self, description: &str) -> (String, Option<String>) {
        let mut id_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::durable_defer(
                description.as_ptr(), description.len() as u32,
                id_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (id_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return (String::new(), Some(format!("defer error code: {}", err_code)));
        }
        let id = unsafe { memory::read_string(id_buf.as_ptr(), id_len) };
        (id, None)
    }

    /// Check for cancellation. Mirrors Go's PollCancellation.
    pub fn poll_cancellation(&self) -> (bool, String) {
        let mut reason_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::durable_poll_cancellation(
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
            imports::durable_poll_signal(
                name.as_ptr(), name.len() as u32,
                payload_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (payload_len, found, err_code) = memory::decode_poll_signal_result(result);
        if err_code != 0 {
            return (String::new(), false, Some(format!("signal error code: {}", err_code)));
        }
        let payload = if found && payload_len > 0 {
            unsafe { memory::read_string(payload_buf.as_ptr(), payload_len) }
        } else {
            String::new()
        };
        (payload, found, None)
    }

    /// Continue as new. Mirrors Go's ContinueAsNew.
    pub fn continue_as_new(&self, input_json: &str) -> Option<String> {
        let result = unsafe {
            imports::durable_continue_as_new(
                input_json.as_ptr(), input_json.len() as u32,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Some(format!("continue_as_new error code: {}", err_code));
        }
        None
    }

    /// Start a child workflow. Mirrors Go's ChildWorkflow.
    pub fn child_workflow(&self, name: &str, input_json: &str) -> (String, Option<String>) {
        let mut run_id_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::durable_child_workflow(
                name.as_ptr(), name.len() as u32,
                input_json.as_ptr(), input_json.len() as u32,
                run_id_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (run_id_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return (String::new(), Some(format!("child_workflow error code: {}", err_code)));
        }
        let run_id = unsafe { memory::read_string(run_id_buf.as_ptr(), run_id_len) };
        (run_id, None)
    }

    /// Await child workflow completion. Mirrors Go's AwaitChild.
    pub fn await_child(&self, run_id: &str) -> (String, Option<String>) {
        let mut result_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let r = unsafe {
            imports::durable_await_child(
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
            return (String::new(), Some(format!("await_child error code: {}", err_code)));
        }
        let result = unsafe { memory::read_string(result_buf.as_ptr(), result_len) };
        (result, None)
    }

    /// Await external signals. Mirrors Go's AwaitSignals.
    /// Returns (signal_name, payload, timed_out, error).
    pub fn await_signals(&self, signal_names: &[&str], timeout_ms: i64) -> (String, String, bool, Option<String>) {
        // JSON-marshal the signal names array, matching Go's adapter.go behavior.
        let names_json = serde_json::to_string(signal_names).unwrap_or_else(|_| "[]".to_string());
        let mut sig_name_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let mut payload_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::durable_await_signals(
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
            return (String::new(), String::new(), false, Some(format!("await_signals error code: {}", err_code)));
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
            imports::durable_create_promise(
                name.as_ptr(), name.len() as u32,
                id_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (id_len, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return (String::new(), Some(format!("create_promise error code: {}", err_code)));
        }
        let id = unsafe { memory::read_string(id_buf.as_ptr(), id_len) };
        (id, None)
    }

    /// Await a durable promise. Mirrors Go's AwaitPromise (ABI 2.21).
    /// Returns (result, timed_out, error).
    pub fn await_promise(&self, promise_id: &str, timeout_ms: i64) -> (String, bool, Option<String>) {
        let mut result_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::durable_await_promise(
                promise_id.as_ptr(), promise_id.len() as u32,
                timeout_ms,
                result_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (result_len, timed_out, err_code) = memory::decode_await_promise_result(result);
        if err_code != 0 {
            return (String::new(), timed_out, Some(format!("await_promise error code: {}", err_code)));
        }
        let result = if result_len > 0 {
            unsafe { memory::read_string(result_buf.as_ptr(), result_len) }
        } else {
            String::new()
        };
        (result, timed_out, None)
    }

    /// Register an update handler. Mirrors Go's RegisterUpdateHandler (ABI 2.22).
    pub fn register_update_handler(&self, name: &str) {
        unsafe {
            imports::durable_register_update_handler(
                name.as_ptr(), name.len() as u32,
            );
        }
    }

    /// Call a plugin host function. Mirrors Go's PluginCall (ABI 2.19).
    /// Returns (response_json, error_message).
    pub fn plugin_call(&self, plugin_name: &str, function_name: &str, input_json: &str) -> (String, Option<String>) {
        let mut resp_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::durable_plugin_call(
                plugin_name.as_ptr(), plugin_name.len() as u32,
                function_name.as_ptr(), function_name.len() as u32,
                input_json.as_ptr(), input_json.len() as u32,
                resp_buf.as_mut_ptr(), memory::OUT_BUF_SIZE,
            )
        };
        let (response_len, _call_error_code, err_code) = memory::decode_durable_call_result(result);
        if err_code != 0 {
            let err_msg = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
            return (String::new(), Some(err_msg));
        }
        let resp = unsafe { memory::read_string(resp_buf.as_ptr(), response_len) };
        (resp, None)
    }

    /// Send a signal to a target workflow and wait for a response.
    /// Mirrors Go's SendSignalAndWait.
    pub fn send_signal_and_wait(&self, target_run_id: &str, signal_name: &str, payload: &str, timeout_ms: i64) -> Result<String, String> {
        let mut resp_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::durable_send_signal_and_wait(
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
            imports::durable_reply_to_signal(
                correlation_id.as_ptr(), correlation_id.len() as u32,
                response.as_ptr(), response.len() as u32,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("reply_to_signal error code: {}", err_code));
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
            let (name, payload, timed_out, err) = self.await_signals(&remaining_names, remaining.as_millis() as i64);
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
            imports::durable_signal_workflow(
                target_run_id.as_ptr(), target_run_id.len() as u32,
                signal_name.as_ptr(), signal_name.len() as u32,
                payload.as_ptr(), payload.len() as u32,
            )
        };
        let (_extra, err_code) = memory::decode_simple_result(result);
        if err_code != 0 {
            return Err(format!("signal_workflow error code: {}", err_code));
        }
        Ok(())
    }

    /// Set the virtual object scope. Returns the previous scope.
    /// Mirrors Go's SetScope.
    pub fn set_scope(&self, object_type: &str, instance_key: &str) -> String {
        let mut prev_buf = vec![0u8; memory::OUT_BUF_SIZE as usize];
        let result = unsafe {
            imports::durable_set_scope(
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
            imports::durable_get_scope(
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
            imports::durable_set_scope(
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
            imports::durable_uuid(
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
}

/// Result of an await_signals call.
#[derive(Debug, Clone)]
pub struct SignalResult {
    pub name: String,
    pub payload: String,
    pub timed_out: bool,
}
