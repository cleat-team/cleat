//! The `utilities` group of host-call bindings (cleat#3827 group J): `cleat_fetch` and `cleat_json_parse`.
//!
//! Both are stateless request/response and neither suspends. `cleat_fetch` still goes through `guarded`, because a defer
//! segment refuses new work and an outbound HTTP request is the most externally visible kind of it; `cleat_json_parse`
//! only validates and re-serialises what it is handed, records nothing, and reaches no host, so it is not guarded -- see
//! the conventions in `packages/cleat-ts/groups/README.md`, rule 3.

use cleat_sdk::HostCalls;
use javy_plugin_api::javy::quickjs::{Object, prelude::Func};

use crate::{guarded, pair};

pub(crate) fn install(g: &Object<'_>) {
    // The host answers {status, headers, body} and the SDK decodes it into FetchResult; re-serialising that struct is
    // what keeps the guest's shape identical to the SDK's rather than an ad-hoc one built here.
    g.set(
        "__cleat_fetch",
        Func::from(
            |method: String, url: String, headers: String, body: String| -> String {
                guarded(
                    || match HostCalls.cleat_fetch(&method, &url, &headers, &body) {
                        Ok(r) => pair((
                            serde_json::to_string(&r).unwrap_or_else(|_| "{}".to_string()),
                            None,
                        )),
                        Err(e) => pair((String::new(), Some(e))),
                    },
                )
            },
        ),
    )
    .unwrap();

    // Not guarded: this one reads its argument, parses it and returns the host's canonical form. It reaches no host,
    // waits for nothing and records nothing, so there is no event for a defer segment to refuse.
    //
    // A refusal is the host telling us the text is not JSON. It is reported as an error rather than as an empty string,
    // because "" is a valid JSON text and a caller could not tell the two apart.
    g.set(
        "__cleat_json_parse",
        Func::from(|json: String| -> String {
            match HostCalls.json_parse(&json) {
                Some(v) => pair((v, None)).to_string(),
                None => pair((
                    String::new(),
                    Some(format!(
                        "cleat_json_parse refused the input as JSON: {}",
                        truncate_for_message(&json)
                    )),
                ))
                .to_string(),
            }
        }),
    )
    .unwrap();
}

/// A short, quoted excerpt for an error message. The input can be arbitrarily large and the message travels back through
/// the ABI, so it is bounded here rather than at whatever the buffer happens to be.
fn truncate_for_message(s: &str) -> String {
    const MAX: usize = 120;
    if s.chars().count() <= MAX {
        return format!("{s:?}");
    }
    let head: String = s.chars().take(MAX).collect();
    format!("{head:?}... ({} chars)", s.chars().count())
}
