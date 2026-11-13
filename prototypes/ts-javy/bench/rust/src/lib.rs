// Phase 4: the Rust twin of workflows/bench (n recorded side-effect host calls, then an optional sleep).
use cleat_macro::cleat_entry;
use cleat_sdk::HostCalls;
use serde::{Deserialize, Serialize};

#[derive(Deserialize)]
struct StepsInput {
    n: i64,
    sleep_ms: i64,
}

#[derive(Serialize)]
struct Out {
    sum: i64,
}

#[cleat_entry]
fn bench_steps(h: &HostCalls, input: StepsInput) -> Result<Out, String> {
    for i in 0..input.n {
        h.side_effect(&i.to_string())?;
    }
    if input.sleep_ms > 0 {
        h.cleat_sleep_ms(input.sleep_ms)?;
    }
    Ok(Out { sum: input.n })
}
