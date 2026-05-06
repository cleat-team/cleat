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
    /// Returns true if this should suspend the workflow.
    pub fn durable_sleep(&self, duration_ms: i64) -> bool {
        let result = unsafe { imports::durable_sleep(duration_ms) };
        let (status, _) = memory::decode_sleep_result(result);
        status == memory::SLEEP_STATUS_SUSPEND
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
}
