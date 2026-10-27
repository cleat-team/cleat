//! The Rust half of cleat#2978's acceptance test: a workflow that runs the
//! shipped agent as a CHILD, handed the same config the Go and Python
//! clients are handed.
//!
//! THE CONFIG IS INPUT, NOT CODE, and that is deliberate -- the acceptance is
//! that each client runs the SAME agent config through the shipped agent
//! workflow, so the config has to come from the run's own input, not a
//! per-client literal that could silently drift from the others.
//!
//! This file is also the Rust SDK's surface under test: `cleat_sdk::agent::run_agent`
//! does the start-and-await, so the fixture measures that call rather than
//! hand-rolling the two host calls it is made of.
//!
//! WHY `AgentInput` MIRRORS `agent::AgentConfig` RATHER THAN BEING IT.
//! `agent::AgentConfig` has no public `message` field -- `run_agent` takes
//! the message as its own argument -- so a config struct carrying it would
//! have two places to look for it. This mirrors the Go client's shape
//! (`agentworkflow.Input`, which does carry `message`) and converts one to
//! the other at the boundary, the same conversion Python's client makes for
//! the same reason.
//!
//! `#[cleat_entry]` requires exactly one user parameter beyond `&HostCalls`:
//! a WASM export receives one JSON payload. So the whole input JSON IS the
//! agent config, exactly as the Go client receives it (a single string
//! parameter bound to the whole payload) -- unlike the Python client, which
//! must wrap the config under a key because a Python entry point binds
//! parameters by name.

use cleat_macro::cleat_entry;
use cleat_sdk::agent::{self, AgentConfig, AgentResult, Tool};
use serde::Deserialize;

#[derive(Debug, Deserialize)]
struct ToolInput {
    name: String,
    kind: String,
    #[serde(default)]
    description: String,
    #[serde(default)]
    parameters: Option<serde_json::Value>,
    #[serde(default)]
    service: String,
    #[serde(default)]
    operation: String,
    #[serde(default)]
    plugin: String,
    #[serde(default)]
    function: String,
    #[serde(default)]
    workflow: String,
}

/// The agent's configuration, as it arrives in the run's input. Field names
/// are the JSON keys of the config, and they match `cleat/agentworkflow`'s
/// `Input` exactly -- that is what lets one JSON document be every client's
/// configuration.
#[derive(Debug, Deserialize)]
struct AgentInput {
    message: String,
    #[serde(default)]
    tools: Vec<ToolInput>,
    #[serde(default)]
    system_prompt: String,
    #[serde(default)]
    provider: String,
    #[serde(default)]
    model: String,
    #[serde(default)]
    max_steps: i64,
    #[serde(default)]
    temperature: f64,
}

#[cleat_entry]
fn run_agent_client(h: &cleat_sdk::HostCalls, input: AgentInput) -> Result<AgentResult, String> {
    let tools = input
        .tools
        .into_iter()
        .map(|t| Tool {
            name: t.name,
            kind: t.kind,
            description: t.description,
            parameters: t.parameters,
            service: t.service,
            operation: t.operation,
            plugin: t.plugin,
            function: t.function,
            workflow: t.workflow,
            ..Default::default()
        })
        .collect();

    let config = AgentConfig {
        tools,
        system_prompt: input.system_prompt,
        provider: input.provider,
        model: input.model,
        max_steps: input.max_steps,
        temperature: input.temperature,
        ..Default::default()
    };

    agent::run_agent(h, config, input.message)
}
