// DETERMINISTIC BY CONSTRUCTION -- the negative control for R008 (cleat#1864).
//
// R008 is about ORDER, not about the type: constructing a HashMap, inserting
// into it, and looking a key up in it are all deterministic regardless of the
// hasher's seed. Only enumerating its contents in an unspecified order is the
// hazard. This crate exercises the deterministic operations and none of the
// order-producing ones (.iter/.iter_mut/.keys/.values/.values_mut/.into_iter/
// .drain, or `for x in a_map`), and must vet clean.
use std::collections::HashMap;

pub fn build() -> HashMap<String, u64> {
    let mut m = HashMap::new();
    m.insert("a".to_string(), 1);
    m.insert("b".to_string(), 2);
    m
}

pub fn lookup(m: &HashMap<String, u64>, key: &str) -> Option<u64> {
    if !m.contains_key(key) {
        return None;
    }
    m.get(key).copied()
}

pub fn remove_and_count(m: &mut HashMap<String, u64>, key: &str) -> usize {
    m.remove(key);
    m.len()
}
