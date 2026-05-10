use cleat_macro::cleat_entry;
use cleat_sdk::HostCalls;

#[derive(serde::Deserialize)]
struct MyInput { x: i32, y: i32 }

#[cleat_entry]
fn destructured(_h: &HostCalls, MyInput { x, y }: MyInput) -> Result<String, String> {
    Ok(format!("{} {}", x, y))
}

fn main() {}
