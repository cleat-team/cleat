# The Rust determinism checker: a path resolver, not a parser and not a toolchain

**Status: design decision, 2026-09-17. cleat#1811.**
Written before the implementation, so that the approach and what it gives up are a decision on the
record rather than something inferred afterwards from the code that happened to get written.

---

## The problem

> **Implemented in cleat#1811.** This note is left in the tense it was written in — it describes the
> state the decision was made against, not the state of the tree. Rewriting it to match what was
> built would destroy the only record of what the alternatives were.

`cmd/cleat/vet_rust.go` matched **15 literal substrings** (`forbiddenRustPatterns`) with
`strings.Contains` over each line — 14 forbidden spellings and one allowed-marker row.
`DurableLeaves`, `DurableClosure` and `Pure` in its output are permanently `0`: there is no call
graph and no reachability, so those three fields are placeholders, not measurements.

Since cleat#1784 the check runs inside `cleat build --target rust`, so it decides whether an artifact
is emitted. A checker that cannot see idiomatic code is now a build gate that cannot see idiomatic
code.

What escapes it is **ordinary Rust**, not evasion:

```rust
use std::{fs, net};               // grouped — no substring matches
use std::time::SystemTime as ST;  // aliased — the forbidden spelling never appears
ST::now()
```

Both are what `rustfmt` produces and what any competent author writes without thinking about it.

---

## The decision

**A hand-written `use`-declaration resolver in pure Go**, layered on the comment/string blanker that
landed in cleat#1782.

Not tree-sitter, not a `syn` helper binary, not a clippy lint. The reasoning is below, and the
deciding fact is one the issue's own options table does not price.

### Why not tree-sitter, which was the issue's provisional preference

The table scores tree-sitter as keeping the no-toolchain property. That is true of the **build
inputs** — no `cargo`, no `rustc` — and it is not the whole cost. **The Go bindings are cgo.**

`cmd/cleat` is deliberately pure-Go cross-compilable, and that is measured rather than assumed
(2026-09-17, `go build -o /dev/null ./cmd/cleat/`, exit status read without a pipe):

| target | `CGO_ENABLED=0` |
|---|---|
| darwin/arm64 | rc=0 |
| linux/amd64 | rc=0 |
| linux/arm64 | rc=0 |
| windows/amd64 | rc=1 — `engine/redact.go:190: undefined: unix.O_NOFOLLOW`, pre-existing and not a shipped target |

The three that pass are exactly the platforms `.goreleaser.yml` ships `cleat` and `cleat-gen` for.
IMPROVEMENT-PLAN §3.56 declined the wazero removal partly because it *"would force `cleat` onto CGO
(ending pure-Go cross-compilation for the CLI)"* — the same cost, already weighed once and declined.
Paying it for a determinism lint, when a dependency-free resolver handles every listed miss, is the
wrong trade.

The rest of the table stands as written: a `syn` helper binary means shipping a Rust binary per
platform, and a dylint/clippy lint moves the gate after the toolchain check, which gives up
`build_rust.go`'s deliberate ordering — the gate runs **before** the cargo lookup so that a crate
with determinism errors is refused on a machine that could not have compiled it anyway.

### The fifth row

| Approach | Sees | No toolchain | Pure-Go CLI | Cost |
|---|---|---|---|---|
| **`use` resolver (chosen)** | Grouped, nested and aliased imports; `self`; call sites resolved through the alias map | **Yes** | **Yes** | ~100 lines, no dependency; no type resolution |
| tree-sitter | The above, plus expression shapes | Yes | **No — cgo** | New cgo dependency |
| `syn` helper | Full syntax | No | Yes | A shipped Rust binary per platform |
| clippy/dylint | Full types and real resolution | No | Yes | Gate moves after the toolchain check |

---

## What it does

1. Blank comments and string literals (`rustCodeOnly`, cleat#1782), so prose naming a spelling is not
   a use of it.
2. Parse every `use` declaration into `localName -> fullPath`, expanding nested groups, `as` aliases
   and `self`.
3. Resolve each `a::b::c` path expression's head through that map.
4. Compare the **resolved** path against forbidden module prefixes, rather than comparing source text
   against spellings.

Prototyped before this note was written, against the fixtures the issue names:

```
use std::time::SystemTime as ST;          ST           -> std::time::SystemTime
use std::{collections::HashMap, ...};     HashMap      -> std::collections::HashMap
use std::net::{TcpStream, UdpSocket as US};  US        -> std::net::UdpSocket
use std::io::{self, Write};               io           -> std::io

R005  call std::time::SystemTime::now  (written ST::now)
R002  call std::net::UdpSocket::bind   (written US::bind)
R001  call std::fs::read_to_string     (written fs::read_to_string)   <- known_limit_grouped_use
```

The last line is the existing `known_limit_grouped_use` fixture, which the substring matcher reports
as clean. Reporting the resolved path **beside what was written** is deliberate: a finding that says
`ST::now` and nothing else sends the reader to a name that is not in the pattern list.

---

## What it gives up, stated so the next reader does not have to discover it

- **No type resolution.** A trait method, a re-export, or a value that reaches a forbidden API
  through a local variable is invisible. This is shared with tree-sitter and is the reason
  `known_limit_trait_method` is a fixture rather than a bug.
- **File-scoped imports.** A `use` inside a function body is applied to the whole file. Over-reports
  rather than under-reports; the safe direction for a gate.
- **No reachability.** Every `.rs` file under the crate is scanned whether or not the workflow can
  reach it, exactly as today. Go has `VerifyThreading` and Python has `ThreadingChecker`; Rust has
  neither, and this design does not add one.
- **`DurableLeaves`, `DurableClosure`, `Pure` stay permanently `0`.** They are not measurements and
  this change does not make them measurements. Three zero fields in a JSON report read as findings of
  zero; they are absence of a call graph.

---

## The HashMap rule, and why it is hardening rather than a correctness fix

`HashMap`/`HashSet` default to `RandomState`, seeded per process, so iteration order varies between
runs. The issue asks whether that seed reaches cleat's intercepted `random_get` before the rule's
severity can be decided. **Measured 2026-09-17, with a negative control:**

| | |
|---|---|
| a `cdylib` on `wasm32-wasip1` **with** a `HashMap` | imports `wasi_snapshot_preview1.random_get` |
| the same crate **without** one | imports **nothing at all** |
| the module run with `random_get` filled `0x00` / `0xFF` / `0x00` | `-1443680002` / `-252770918` / `-1443680002` |

Imports read from section id 2, not grepped out of the binary. So the order genuinely depends on
`random_get`, and it reproduces under a fixed one. cleat binds `random_get` to `handler.Random()`
(`engine/wasmtime_wasi_determinism.go:82`), seeded from workflow id and step, registered **after**
`DefineWasi` so it is the final word (`engine/backend_wasmtime.go:1171-1189`);
`engine/wasi_policy.go:101` classifies it `wasiIntercepted`, and the wazero dev path has the same
shape.

**So two replays of one workflow see the same iteration order, and the rule is defence in depth.**
Its message must not claim a replay divergence the runtime prevents. What was measured is the seed
path; a full replay of a real workflow observing stable order end to end was not, and a guest that
obtains entropy some other way is out of scope.

Detecting it still needs a type, not a path: the resolver can see `HashMap` imported and
`HashMap::new()` called, and a `for … in x.iter()` where `x` came from one is a heuristic rather than
a proof. Proportionate to a hardening rule; the limit goes in the fixture.

### Shipped as R101, a warning

`findRustHashIteration` implements exactly the above. It is the only rule in this checker that lands
in `Warnings` rather than `Errors`, so it never changes the exit code — measured both ways: a crate
whose only finding is R101 exits 0, and the same crate with a `std::fs` call exits 1.

Two passes, both over the already-blanked source the path rules use, so a comment or a string
literal naming `for kv in &m` reports nothing:

1. which local names hold a hash container — `let m: HashMap<..>`, `let m = HashMap::new()`, and a
   `fn` parameter `m: &HashMap<..>` — resolved through the same alias map as the path rules, so
   `use std::collections::HashMap as HM;` and a fully-qualified `std::collections::HashMap<..>` are
   both seen;
2. iteration over one of those names — the `for … in` form, which names no method at all, plus
   `iter`, `iter_mut`, `into_iter`, `keys`, `values`, `values_mut`, `drain`.

`get`, `insert`, `contains_key`, `len`, `remove` and `entry` are deliberately absent. **The rule is
about order, not about the type.** A rule that fires on using a `HashMap` at all is one people argue
with rather than fix, and `testdata/vet-checks/rust/r101_hash_iteration` keeps three of those calls
in the same function as the reported loop so the distinction is pinned rather than asserted.

`BTreeMap`/`BTreeSet` are never reported: their order is sorted and guaranteed, and they are what the
suggestion points at. Flagging the fix would send a reader in a circle.

**The message does not claim a replay divergence**, per the paragraph above — it says the code is
deterministic *because cleat intercepts the entropy WASI hands the guest*, which is what was measured
and is also the fragility being flagged. A reader who is told "unspecified in Rust" and then watches
the build pass would otherwise conclude the checker is broken.

**Go emits `E021` as an error and this is a warning.** A deliberate asymmetry, recorded because the
opposite mistake has already been made here: `docs/troubleshooting.md` and
`docs/workflow-go-constraints.md` agreed for months that map iteration was a *warning* under a code
the analyzer has never emitted, while it actually failed the build with `E021` — which is why
`cmd/cleat/documented_flags_and_codes_test.go` exists. (That code is deliberately not named here:
citing it is the very thing the guard refuses, and writing this paragraph tripped it.) That was a document contradicting the code. This is the code choosing, for a stated reason:
`E021` predates the interception and hardening it costs nothing, whereas making Rust fail the build
would refuse idiomatic code that works today and cannot be rewritten without changing the data
structure.

Known limits are in `TestRustHashIterationKnownLimits`, which **fails** rather than skips when one is
lifted, so the list and the code cannot drift apart: a map behind a struct field or returned from a
call is not seen, and an order-independent consumer (`m.values().sum()`) is reported anyway. That
last one is the main source of noise and the main reason this is not an error.

---

## What "passing" means, and what `LANGUAGE_SUPPORT.md` should say

Before: *passing is evidence that none of a short list of spellings appeared.*
After: *passing is evidence that no path resolving to a forbidden module is named in code, under
file-scoped imports and without type resolution.*

Neither is evidence of determinism. The doc should say which one is true rather than implying the
gate is an analysis.
