# The Java determinism checker: resolve the type, not the spelling

**Status: design decision, 2026-09-17. cleat#1812.**
Written before the implementation, so the approach and what it gives up are a decision on the
record rather than something inferred afterwards from whatever code got written. Deliberately
shaped like `rust-determinism-checker.md` (cleat#1811), because the two checkers have the same
defect and should not acquire two different cures.

---

## The problem

`cmd/cleat/vet_java.go` matches **33 literal substrings** (`forbiddenJavaPatterns`) with
`strings.Contains` over each line of `javaCodeOnly`'s output. `DurableLeaves`, `DurableClosure` and
`Pure` in its result are permanently `0` — there is no call graph and no reachability, so those
three fields are placeholders rather than measurements.

Since cleat#1791 the check runs inside `cleat build --target java`, so it decides whether an
artifact is emitted.

The 33 divide into three shapes, and the shape is what determines what escapes:

| shape | count | example |
|---|---|---|
| import-anchored | 5 | `import java.io.` |
| `new`-anchored | 9 | `new Socket(` |
| bare | 19 | `java.io.File`, `InputStream`, `Socket socket` |

Re-derive:

```bash
python3 - <<'EOF'
import re
body = open('cmd/cleat/vet_java.go').read().split('var forbiddenJavaPatterns = []struct {',1)[1]
pats = re.findall(r'\{`([^`]*)`,\s*"(J\d+)"', body)
print(len(pats), 'patterns')
for kind, test in (('import', lambda p: p.startswith('import ')),
                   ('new',    lambda p: p.startswith('new ')),
                   ('bare',   lambda p: not p.startswith(('import ','new ')))):
    print(kind, sum(1 for p,_ in pats if test(p)))
EOF
```

---

## What escapes, measured

Every row below was run through the built CLI — `cleat vet --lang java <dir>` — on
2026-09-17, not reasoned about. Each fixture directory carries a **positive control**
(`System.currentTimeMillis()` in a separate file) so a clean result is distinguishable from a
checker that did not run; the control fired in every case.

| spelling | caught? |
|---|---|
| `System.currentTimeMillis()` (the control) | yes, J001 |
| `java.io.File.createTempFile("x","y")` fully qualified, no import | **yes**, J013 |
| `import static java.lang.System.currentTimeMillis;` then `currentTimeMillis()` | no |
| `java.time.Clock.systemUTC().millis()` fully qualified, no import | no |
| `Class.forName(name)` | no |
| `for (String k : new HashMap<>(m).keySet())` | no |

**The second row corrects cleat#1812's own claim**, which I wrote: the issue asserts a
fully-qualified `java.io.File` call escapes because there is no `import java.io.` and no
`new java.io.`. It does not escape — one of the 19 bare patterns is the literal `java.io.File`.

That correction is the most useful thing in this table, because of *why* it is caught. It is not
that fully-qualified calls are handled; it is that **one class name happened to be listed bare and
another did not.** `java.io.File` is in the table, `java.time.Clock` is not. The coverage of
fully-qualified calls is therefore incidental — a property of which strings someone thought to add,
not of any rule — and no reader of the pattern list can tell which spellings are covered without
running it.

### The sharpest case: three patterns match a variable naming convention

`Connection con`, `Socket socket` and `Random random` are not API names. They are *declarations as
an author is likely to spell them*, and the check therefore depends on what the author called the
variable. Measured end-to-end, each declared as a method parameter so no `new …(` or `import`
pattern can account for the result:

| file | content | errors |
|---|---|---|
| `Caught.java` | `import java.sql.Connection;` … `void a(Connection conn)` etc. | **6** |
| `Evades.java` | `void a(java.sql.Connection db)`, `void b(java.net.Socket sock)`, `void c(java.util.Random rng)` | **0** |

Three JDBC/socket/RNG parameters, fully qualified, ordinary names, and the checker reports nothing.
`Connection conn` is caught only because `Connection con` is a *prefix* of it; `Socket sock` is not
caught because `Socket socket` is not a prefix of `Socket sock`. Which conventional name a
developer picks decides whether the build is refused:

```
Connection con / conn / connection    caught
Connection db / c / jdbc              not caught
Socket socket                         caught
Socket sock / s / client / peer       not caught
Random random                         caught
Random rng / r / rnd / gen            not caught
```

### And it refuses code that is fine

`java.io.ByteArrayInputStream` is pure, in-memory and perfectly replayable. It draws **two**
errors on one line — J008 from `new java.io.` and J016 from the bare `InputStream`, which is a
substring of `ByteArrayInputStream`. A substring table cannot separate "reads a file" from "reads
a byte array it was handed", because the distinguishing fact is the resolved type and the table
never resolves anything.

So the checker is wrong in **both** directions at once, and the two directions have the same cause.

---

## The decision

**A `import`-declaration and fully-qualified-name resolver in pure Go**, layered on the
comment/string blanker that landed in cleat#1824. Forbidden APIs become *resolved paths*
(`java.io.File`, `java.net.Socket`, `java.util.Random`) and the resolver's job is to decide, for
each name used in the file, which path it denotes.

That is the same decision cleat#1811 made for Rust, for the same reason, and the two should share
the shape even though they cannot share code.

### Why not tree-sitter-java

Identical to #1811's reasoning and inherited from its measurement rather than re-derived: the Go
bindings are cgo, `cmd/cleat` is deliberately pure-Go cross-compilable for the three platforms
`.goreleaser.yml` ships, and IMPROVEMENT-PLAN §3.56 already declined to pay that cost once. A
determinism lint is not the thing to pay it for.

### Why not bytecode — for now, and with a real loss stated

Bytecode is the better analysis target and #1812 is right about why: syntax disappears, grouped
imports and static imports and `var` and fully-qualified calls all compile to the same
`invokevirtual` against a resolved owner, and **a real call graph exists**, which is the one thing
no non-Go checker in this repo has.

The issue prices the trade as "loses the no-JDK property". Measured, that is narrower than it
sounds. `cleat build --target java` runs `gradle generateWasm` (`cmd/cleat/build_java.go`), so the
**build path already requires a JDK by construction** — a bytecode checker gives up nothing there.
What it gives up is `cleat vet --lang java` as a standalone, toolchain-free command, which is a
real thing to lose but a much smaller one than the framing suggests.

It is deferred anyway, for a reason that is not about toolchains:

**A resolver fixes five of the six measured misses; bytecode fixes six.** Static imports,
fully-qualified calls of any class, the variable-naming cases, reflection's `Class.forName`, and
the `ByteArrayInputStream` false positive are all resolution problems, and a resolver resolves
them. The sixth — `HashMap` iteration order — is not an import problem at all and needs a new
rule whichever substrate carries it.

What bytecode buys that a resolver cannot is **reachability**: today every `.java` file under
`src/main` is scanned whether or not the entry point reaches it, which is why `DurableClosure` is
`0`. That is a genuine capability gap and this note does not pretend to close it.

**So the decision is staged, and the staging is the decision — not a way of avoiding one.** The
resolver lands first because it fixes the cases that are wrong *today* without a new dependency or
a new build-order constraint. Reachability is a separate piece of work with a different cost, and
it should be argued on its own evidence rather than smuggled in as a side effect of fixing
spellings. It needs its own issue.

---

## What the resolver must handle

Each of these is a fixture under `testdata/vet-checks/java/`, and each is a case the current
checker gets wrong:

```
j004_fully_qualified_time/      java.time.Clock.systemUTC()      -- refuse (today: passes)
j001_static_import/             import static java.lang.System.currentTimeMillis
j0xx_reflection/                Class.forName(...)               -- refuse; see the limit below
naming_does_not_decide/         java.sql.Connection db           -- refuse (today: passes)
pure_byte_array_stream/         java.io.ByteArrayInputStream     -- ALLOW (today: two errors)
```

`import java.util.*` is a wildcard and resolves a bare `Random` only against a known package
listing. Resolve the JDK packages the table names and treat an unresolvable bare name as
unresolved rather than as either verdict — an unresolved name is exactly the `UNMEASURED` case
CLAUDE.md asks checks to have a word for.

### Stated limits, so a pass is not read as more than it is

- **Reflection with a computed name is undecidable.** `Class.forName(s)` where `s` is a variable
  cannot be resolved by any static analysis, bytecode included. The proposal is to forbid
  `Class.forName` outright in workflow code, which is a rule that *can* be enforced exactly,
  rather than to imply coverage that does not exist.
- **No reachability.** Every production source file is scanned. A helper the entry point never
  calls can refuse a build.
- **No `HashMap` iteration rule yet.** Go has `E021` for this and Java has nothing. It is the one
  miss in the table above that is a missing *rule* rather than a missing *resolution*, and it is
  plain idiomatic Java — the author does nothing unusual and gets a replay hazard with no
  diagnostic.

---

## What a test for this must assert

`TestRustBuildRefusesNondeterminism` carries the lesson in its own comment: an arm asserting only
"the build fails" passes against a tree with no gate at all. **Both arms**, always — a fixture that
must be refused and a fixture that must be accepted — and for a change that *widens* what is
scanned, the accepting arm is the one that matters, because the failure mode of a widening is
refusing code that is fine. `java.io.ByteArrayInputStream` is in this note precisely because it is
already that failure, today, with no widening required.
