//! DETERMINISTIC BY CONSTRUCTION, and it names forbidden spellings on purpose.
//!
//! cleat#1782: the Rust determinism scan is `strings.Contains` over each line,
//! so before rustCodeOnly a comment mentioning a pattern WAS a violation of it
//! -- and since #1784 wired the scan into `cleat build --target rust`, prose
//! like this failed a build rather than only a lint.
//!
//! Every spelling below is quoted deliberately. This crate calls none of them:
//!
//!   use std::fs            std::fs::
//!   use std::net           std::net::
//!   use std::process       std::process::Command
//!   use rand               rand::
//!   std::time::SystemTime::now    std::time::Instant::now
//!   use std::thread        std::thread::
//!   use std::sync          std::sync::
//!
//! The sibling fixture known_limit_grouped_use had to be written the OTHER way
//! round -- its header says "read the list there rather than here" because
//! quoting it produced seven errors out of its own explanation. That workaround
//! is what this fixture exists to retire; if the two ever disagree, this one is
//! the statement of intent.

/* A nested block comment, because Rust allows them and a scanner that does not
   count depth stops at the first close it meets rather than the matching one:
   /* use std::sync::Mutex */ and after the inner close, still comment:
   use std::fs::File */

// The line above deliberately does NOT write the close delimiter in prose. The
// first draft did -- inside backticks, as an example -- and the comment ended
// there, because backticks mean nothing to a lexer and `*` followed by `/` is a
// close wherever it appears. Everything after it read as code and the scan
// reported two R001s from a sentence explaining comment handling. The fixture
// walked into its own subject; that is recorded here rather than smoothed away.

#[no_mangle]
pub fn workflow() -> i32 {
    // A string literal is prose too: it cannot execute, so naming a pattern
    // inside one is not a use of it either.
    let banned = "use std::process::Command";
    let raw = r#"rand::thread_rng() and std::net::TcpStream"#;
    let _ = (banned.len(), raw.len());

    // A lifetime is not a character literal; a scanner that confuses them
    // swallows the rest of the line and stops seeing real code.
    fn first<'a>(xs: &'a [i32]) -> &'a i32 { &xs[0] }
    let xs = [7, 8, 9];

    *first(&xs) + (banned.len() as i32) + (raw.len() as i32)
}
