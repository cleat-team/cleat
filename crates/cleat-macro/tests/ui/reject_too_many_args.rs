use cleat_macro::cleat_entry;
use cleat_sdk::HostCalls;

#[cleat_entry]
fn too_many(_h: &HostCalls, a: String, b: String) -> Result<String, String> {
    Ok(format!("{} {}", a, b))
}

fn main() {}
