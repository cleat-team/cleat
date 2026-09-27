// A Cargo bench target. Same reasoning as tests/.
use std::thread;
use std::time::Instant;

fn main() {
    let t = Instant::now();
    thread::yield_now();
    let _ = t.elapsed();
}
