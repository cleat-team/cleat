use cleat_macro::cleat_entry;
use cleat_sdk::HostCalls;

#[cleat_entry]
async fn async_workflow(_h: &HostCalls) -> Result<String, String> {
    Ok("bad".to_string())
}

fn main() {}
