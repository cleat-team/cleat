// An ALIASED import of the wall clock. cleat#1811.
//
// The literal-spelling table looked for `std::time::SystemTime::now`. That text
// never appears here: the import renames the type and the call site uses the
// new name, so the forbidden spelling is not in the file at all. `as` is
// ordinary Rust, used to shorten a long path or to disambiguate two types with
// the same name, and nothing about this is evasion.
//
// The resolver reads the declaration into ST -> std::time::SystemTime, resolves
// the call's head through that map, and matches the RESOLVED path. The finding
// names both forms, because "R005 at ST::now" would send the reader looking for
// a spelling that is not in any table:
//
//   R005  (written "ST::now", which resolves to std::time::SystemTime::now)
//
// std::time::Duration is imported here on purpose and is NOT a finding. It is
// what h.DurableSleep() takes, and it is not under either forbidden prefix --
// the old table needed an explicit allow row to say so, and the prefix rule
// says it by construction.
use std::time::Duration;
use std::time::SystemTime as ST;

#[no_mangle]
pub fn workflow() -> u64 {
    let d = Duration::from_secs(1);
    let t = ST::now();
    let _ = d;
    t.duration_since(ST::UNIX_EPOCH).map(|x| x.as_secs()).unwrap_or(0)
}
