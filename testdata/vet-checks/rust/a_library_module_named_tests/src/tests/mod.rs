use std::fs;

pub fn helper() -> i32 {
    let _ = fs::read_to_string("data.txt");
    1
}
