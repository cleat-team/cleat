use cleat_macro::cleat_entry;

#[cleat_entry]
fn no_hostcalls(input: String) -> Result<String, String> {
    Ok(input)
}

fn main() {}
