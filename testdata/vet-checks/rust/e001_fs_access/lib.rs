use std::fs;

#[no_mangle]
pub fn workflow() {
    let _ = fs::read_to_string("data.txt");
}
