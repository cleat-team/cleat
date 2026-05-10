use cleat_macro::cleat_entry;
use cleat_sdk::HostCalls;

#[cleat_entry]
fn bad_return(_h: &HostCalls) -> String {
    "not a result".to_string()
}

fn main() {}
