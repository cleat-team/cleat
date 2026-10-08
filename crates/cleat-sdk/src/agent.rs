//! `run_agent` -- start the shipped agent workflow as a child and await it.
//!
//! The agent loop is itself a WORKFLOW (`cleat/agentworkflow` in the Go SDK),
//! so this module is the whole per-language surface: the LLM turns, the tool
//! dispatch, the step budget and the durability all live in the workflow, and
//! a Rust workflow gets every one of them by starting it under its name.
//! Before cleat#1983 the Go SDK carried a Go-only ReAct loop with no
//! importer, and the two agent templates carried hand-written copies of the
//! same loop that nothing tested. cleat#2978 is the Rust/Java/AssemblyScript
//! follow-up.
//!
//! Each LLM turn and each tool call is a durable step inside the workflow, so
//! an agent started this way survives a crash mid-conversation and resumes
//! without asking the model again for turns it already completed.
//!
//! Mirrors `python-sdk/cleat_sdk/agent.py`'s field scope exactly, not the
//! full Go `agentworkflow.Input` -- `arg_transforms`/`static_args`
//! (cleat#3169) are not yet exposed by any SDK wrapper, Python's included.
//!
//! ```ignore
//! use cleat_sdk::agent::{AgentConfig, Tool, run_agent};
//!
//! let result = run_agent(h, AgentConfig {
//!     tools: vec![Tool { name: "lookup".into(), kind: "service".into(),
//!         service: "weather".into(), operation: "get".into(), ..Default::default() }],
//!     ..Default::default()
//! }, "How warm is Tokyo?")?;
//! ```

use crate::HostCalls;
use serde::{Deserialize, Serialize};

/// The workflow name the agent must be DEPLOYED under, because a child is
/// resolved by name against a deployed `workflow_defs` row.
pub const AGENT_WORKFLOW_NAME: &str = "agent";

fn is_zero_i64(v: &i64) -> bool {
    *v == 0
}

fn is_zero_f64(v: &f64) -> bool {
    *v == 0.0
}

/// One tool the model may call, and how to call it.
///
/// `name`, `description` and `parameters` are what the model sees; the rest
/// is dispatch, and it is deliberately opaque to the model -- the same tool
/// list reaches a durable call, a plugin or a child workflow without the
/// model being told which.
///
/// `kind` is one of `"service"` (`service` + `operation`), `"plugin"`
/// (`plugin` + `function`), `"workflow"` (`workflow`) or `"approval"`
/// (`plugin` + `function`, polled until the claim reports found).
#[derive(Debug, Clone, Default, Serialize)]
pub struct Tool {
    pub name: String,
    pub kind: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub description: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub parameters: Option<serde_json::Value>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub service: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub operation: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub plugin: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub function: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub workflow: String,

    /// An `approval` tool's wait. 0 takes the workflow's own default.
    #[serde(skip_serializing_if = "is_zero_i64", rename = "poll_interval_seconds")]
    pub poll_interval_seconds: i64,
    #[serde(skip_serializing_if = "is_zero_i64", rename = "max_polls")]
    pub max_polls: i64,
}

/// The agent's configuration. Mirrors `python-sdk/cleat_sdk/agent.py`'s
/// `AgentConfig`, which itself mirrors `cleat/agentworkflow`'s `Input`.
///
/// `message` is set by [`run_agent`], not by the caller: a caller populates
/// every OTHER field and leaves this one at its default, matching
/// `agentworkflow.RunAsChild`/`run_agent`'s own shape, where the message is
/// a separate argument rather than a config field to populate. It is `pub`
/// (not hidden, as a first attempt made it) because Rust's
/// `S { ..Default::default() }` struct-update syntax requires every field
/// to be visible at the call site, even ones the caller never names --
/// a private field here would make `AgentConfig { tools, ..Default::default() }`
/// refuse to compile from outside this crate, which is the shape every
/// caller actually uses.
#[derive(Debug, Clone, Default, Serialize)]
pub struct AgentConfig {
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<Tool>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub system_prompt: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub provider: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(skip_serializing_if = "is_zero_i64")]
    pub max_steps: i64,
    #[serde(skip_serializing_if = "is_zero_f64")]
    pub temperature: f64,

    /// The run's spend ceiling in dollars, against the `cost` the llm plugin
    /// reports. 0.0 (the default) means unbounded. The workflow stops before
    /// starting a turn once the ceiling is REACHED, and reports
    /// `status == "budget_exceeded"` in the result.
    #[serde(skip_serializing_if = "is_zero_f64")]
    pub budget: f64,

    /// Attribution, echoed back in the result and never required by the
    /// workflow.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub tenant_id: String,

    /// When set, the workflow writes the finished answer to the bundled
    /// blobstore plugin under this key and echoes it in the result.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub artifact_key: String,

    /// What a failed tool -- or a tool the model invented -- does. `""` is
    /// the workflow's default (`"inject"`: the error goes back to the model
    /// as the tool's result). `"fail"` ends the run instead. This is a
    /// CONTRACT difference, not a preference, so it is stated rather than
    /// assumed.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub tool_error_mode: String,

    /// Set by [`run_agent`]; a caller leaves this at its default. See the
    /// struct's own doc comment for why it is `pub` rather than hidden.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub message: String,
}

/// One tool call the agent made, as reported back in [`AgentResult`].
///
/// Carries [`Serialize`] as well as [`Deserialize`]: [`AgentResult`] needs
/// both for the same reason it does -- see its own doc comment.
#[derive(Debug, Clone, Default, Deserialize, Serialize)]
pub struct ToolCallRecord {
    pub step: i64,
    pub name: String,
    #[serde(default)]
    pub arguments: String,
    #[serde(default)]
    pub result: String,
    #[serde(default)]
    pub error: String,
}

/// The agent workflow's result.
///
/// Carries [`Serialize`] as well as [`Deserialize`]: [`run_agent`] only
/// decodes one of these (from the child's output), but a workflow that
/// calls [`run_agent`] and forwards the result as its OWN output needs to
/// re-encode it, exactly as `testdata/agentclientrust` does for
/// `tests/crash/agent_resume_test.go`.
#[derive(Debug, Clone, Default, Deserialize, Serialize)]
pub struct AgentResult {
    /// `"done"` when the model answered, or `"budget_exceeded"` when the
    /// spend ceiling stopped it first -- see the workflow's own
    /// `StatusDone`/`StatusBudgetExceeded`.
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub answer: String,
    #[serde(default)]
    pub steps: i64,
    #[serde(default)]
    pub tool_calls: Vec<ToolCallRecord>,
    #[serde(default)]
    pub model: String,
    #[serde(default)]
    pub total_tokens: i64,
    #[serde(default)]
    pub cost: f64,
    #[serde(default)]
    pub tenant_id: String,
    #[serde(default)]
    pub artifact_key: String,
}

/// Starts the shipped agent workflow as a child and awaits its result.
///
/// THIS IS THE WHOLE PER-LANGUAGE SURFACE -- everything else (the loop, the
/// tool dispatch, the step budget, the durability) lives in the workflow,
/// which is why the same agent works from Go, Python, Rust, Java or
/// AssemblyScript without any of them implementing it. See
/// `cleat/agentworkflow`'s own `RunAsChild` doc comment, which this mirrors
/// exactly.
///
/// The caller's workflow needs the agent deployed under
/// [`AGENT_WORKFLOW_NAME`]. A deployment under a different name means
/// calling `h.child_workflow`/`h.await_child` directly with the marshalled
/// config -- two calls, and the reason this is a convenience rather than a
/// mechanism.
pub fn run_agent(
    h: &HostCalls,
    mut config: AgentConfig,
    message: impl Into<String>,
) -> Result<AgentResult, String> {
    config.message = message.into();
    let run_id = h.child_workflow_typed(AGENT_WORKFLOW_NAME, &config)?;
    h.await_child_typed(&run_id)
}

#[cfg(test)]
mod tests {
    use super::*;

    // run_agent itself calls the real `cleat_child_workflow`/`cleat_await_child`
    // host imports, which have no native stub (unlike cleat_json_stringify/parse
    // in lib.rs's native_stubs) -- they only resolve inside a WASM guest. So
    // these tests check the wire shape directly, the same way Go's
    // `agentworkflow` tests and Python's `Tool.to_json()` tests do, rather than
    // calling run_agent against a HostCalls value.

    #[test]
    fn a_default_tool_omits_every_optional_field() {
        let tool = Tool {
            name: "lookup".into(),
            kind: "service".into(),
            ..Default::default()
        };
        let v = serde_json::to_value(&tool).unwrap();
        assert_eq!(v, serde_json::json!({"name": "lookup", "kind": "service"}));
    }

    #[test]
    fn an_approval_tools_poll_fields_round_trip() {
        let tool = Tool {
            name: "wait-for-sign-off".into(),
            kind: "approval".into(),
            plugin: "approvals".into(),
            function: "claim".into(),
            poll_interval_seconds: 5,
            max_polls: 3,
            ..Default::default()
        };
        let v = serde_json::to_value(&tool).unwrap();
        assert_eq!(v["poll_interval_seconds"], 5);
        assert_eq!(v["max_polls"], 3);
    }

    #[test]
    fn a_default_config_sends_only_the_message() {
        // run_agent is the only thing that sets `message`; this test sets it
        // directly, since `message` is a private field by construction.
        let mut config = AgentConfig::default();
        config.message = "hello".into();
        let v = serde_json::to_value(&config).unwrap();
        assert_eq!(v, serde_json::json!({"message": "hello"}));
    }

    #[test]
    fn a_full_config_serializes_every_declared_field() {
        let mut config = AgentConfig {
            system_prompt: "be terse".into(),
            provider: "openai".into(),
            model: "gpt-4o-mini".into(),
            max_steps: 4,
            temperature: 0.2,
            budget: 1.5,
            tenant_id: "tenant-1".into(),
            artifact_key: "answer.txt".into(),
            tool_error_mode: "fail".into(),
            tools: vec![Tool {
                name: "lookup".into(),
                kind: "service".into(),
                service: "weather".into(),
                operation: "get".into(),
                ..Default::default()
            }],
            ..Default::default()
        };
        config.message = "How warm is Tokyo?".into();
        let v = serde_json::to_value(&config).unwrap();
        assert_eq!(v["message"], "How warm is Tokyo?");
        assert_eq!(v["system_prompt"], "be terse");
        assert_eq!(v["provider"], "openai");
        assert_eq!(v["model"], "gpt-4o-mini");
        assert_eq!(v["max_steps"], 4);
        assert_eq!(v["temperature"], 0.2);
        assert_eq!(v["budget"], 1.5);
        assert_eq!(v["tenant_id"], "tenant-1");
        assert_eq!(v["artifact_key"], "answer.txt");
        assert_eq!(v["tool_error_mode"], "fail");
        assert_eq!(v["tools"][0]["service"], "weather");
        assert_eq!(v["tools"][0]["operation"], "get");
    }

    #[test]
    fn a_result_decodes_from_the_workflows_own_json_shape() {
        // This is agentworkflow.Result's own JSON shape, byte for byte --
        // confirming AgentResult can decode what the workflow actually sends.
        let raw = r#"{
            "status": "done",
            "answer": "It's 28C in Tokyo.",
            "steps": 2,
            "tool_calls": [
                {"step": 0, "name": "lookup", "arguments": "{\"city\":\"Tokyo\"}", "result": "28C"}
            ],
            "model": "gpt-4o-mini",
            "total_tokens": 123,
            "cost": 0.0021
        }"#;
        let res: AgentResult = serde_json::from_str(raw).unwrap();
        assert_eq!(res.status, "done");
        assert_eq!(res.answer, "It's 28C in Tokyo.");
        assert_eq!(res.steps, 2);
        assert_eq!(res.tool_calls.len(), 1);
        assert_eq!(res.tool_calls[0].name, "lookup");
        assert_eq!(res.total_tokens, 123);
        assert!((res.cost - 0.0021).abs() < f64::EPSILON);
    }

    #[test]
    fn a_result_with_only_status_decodes_cleanly() {
        // Every field but status is #[serde(default)], so a minimal result
        // (the budget_exceeded shape, which may omit tool_calls/answer when
        // nothing ran yet) still decodes rather than erroring.
        let res: AgentResult = serde_json::from_str(r#"{"status":"budget_exceeded"}"#).unwrap();
        assert_eq!(res.status, "budget_exceeded");
        assert_eq!(res.answer, "");
        assert!(res.tool_calls.is_empty());
    }
}
