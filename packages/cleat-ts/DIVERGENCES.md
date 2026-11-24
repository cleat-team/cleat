# Where the TypeScript guest differs from Node

TypeScript is a **tier-2** guest language: in development, **not supported** (cleat#3825, `tiers.yaml`). This is the list of
known places where code that runs under Node gives a different answer in the guest, which runs on QuickJS inside a WASM
module with no clock, no event loop and no randomness of its own. It records what was measured and what was not, and each row
names the test that holds it.

A divergence is **silent** when the guest returns a plausible wrong value with no error, and **loud** when it throws. Silent
ones are the dangerous kind and must be fixed or removed before any support claim; loud ones are documented with a workaround.

Only what was looked at is listed. The first survey ran 27 cases over four npm packages (zod 4.6.7, date-fns 4.4.0,
lodash-es 4.18.1, uuid 14.0.3) unmodified; there may be more.

| # | Divergence | Kind | Status | Held by |
|---|---|---|---|---|
| 1 | `Date` setters given a non-finite argument returned `NaN` but left the date unchanged (spec: an Invalid Date). `date-fns` `isValid(parseISO("nope"))` was `true`. | **silent** | **fixed** (the `datesetters` shim in `crates/cleat-js-plugin/src/shims.js`) | `TestTypeScriptDateSettersAgreeWithNode`: 631 cases against Node |
| 2 | No `URL` or `URLSearchParams`. zod's `z.url()` calls `new URL(...)` inside a `try`/`catch`, so the `ReferenceError` was swallowed and a valid URL reported invalid. | **silent** | **fixed** for what can be answered exactly (the `url` shim); for the rest it throws, which a catch-all such as zod's still reads as "invalid" (see below) | `TestTypeScriptURLAgreesWithNode` (the whole `urls.ts` corpus), `TestTypeScriptURLSearchParamsAgreeWithNode`, `TestTypeScriptURLRefusesWhatItCannotAnswer` |
| 3 | No `TextEncoder` / `TextDecoder`. | loud (`ReferenceError`) | **fixed** (the `textcodec` shim in `crates/cleat-js-plugin/src/shims.js`), for UTF-8 only — see rows 7 and 8 for what still differs | `TestTypeScriptTextCodecAgreesWithNode`: 91 cases against Node, including lone surrogates (U+FFFD), a BOM split across stream chunks, `fatal`, `stream`, and `encodeInto`'s read/written counts |
| 4 | No timers: `setTimeout`, `setInterval`, `setImmediate` are not defined, by design (a timer is not a durable operation). lodash-es `debounce` fails with `setTimeout is not defined`. The static check refuses a source that names one. | loud | by design | `TestTypeScriptStaticCheckRefusesATimerAndNamesTheLine` |
| 5 | No `crypto`. uuid `v4()`/`v7()` with no arguments fails with `crypto is not defined`. With the `random` option supplied they equal Node: pass the SDK's replay-safe random in. | loud | by design | none yet |
| 6 | Local time is UTC: the guest has no time zone. A workflow must not depend on the machine's zone anyway, since a replay may run elsewhere. | — | by design | the conformance tests run Node with `TZ=UTC` |
| 7 | `TextDecoder` implements UTF-8 and nothing else. A valid WHATWG label for another encoding (`utf-16le`, `windows-1252`, the ISO-8859 family, the CJK encodings) throws `RangeError` naming cleat#3829; a label WHATWG does not define throws `RangeError` worded like Node's. Nothing is ever decoded as if it were UTF-8. | loud | by design | `TestTypeScriptTextCodecKeepsStreamsAbsentAndNamesItsRefusals` |
| 8 | `TextDecoderStream` and `TextEncoderStream` are **undefined**, not throwing stubs: a library that feature-detects `typeof TextDecoderStream === "function"` must see the truth rather than discover it later. | loud (`ReferenceError` at the use) | by design | `TestTypeScriptTextCodecKeepsStreamsAbsentAndNamesItsRefusals` pins `typeof` as `"undefined"`, so defining them later fails the test and updates this row |

## What the URL shim does and does not do

It is the WHATWG URL algorithm for the part that can be done exactly: the special schemes (`http`, `https`, `ws`, `wss`, `ftp`,
`file`) and generic ones, relative resolution against a base, percent-encoding, IPv4 hosts (including the hex, octal and short
forms), IPv6 hosts in brackets (including the embedded-IPv4 form, written back as hex pairs as the specification does),
`file:` URLs (the empty host, `localhost` folded to it, and a Windows drive letter kept in the path), internationalised hosts,
`URLSearchParams` in full, `URL.canParse` and `URL.parse`, and the `search` and `hash` setters (which update `searchParams`).

IDNA is NFKC folding (fullwidth and compatibility forms), the three non-ASCII dot characters becoming `.`, lowercasing,
percent-encoded UTF-8 decoded first (so `m%C3%BCnchen.de` is `münchen.de`), and RFC 3492 Punycode. Validity is **measured, not
tabulated by hand**: a table of the code points Node 26.4.0 accepts in a label, plus RFC 5893's bidi rule approximated with the
measured right-to-left class. A label already in `xn--` form is **checked, not trusted**: it must decode, its decoded form must
be what UTS46 would have emitted (NFKC, case-folded — with the two measured exceptions, SHARP S and Cherokee — and free of
default-ignorable code points), it must contain a non-ASCII code point and satisfy the same validity and bidi rules, and its
re-encoding must be the same payload.

**The exhaustive check, measured 2026-11-17:** every code point U+0000-U+10FFFF (1,112,064, surrogates excluded), in a raw
label and in an already-Punycode-encoded one, compared with Node 26.4.0. **Zero code points are silently accepted in either
form.** In the `xn--` form the guest refuses some code points Node accepts; that count is a property of the **Node version**,
not of the shim — **23** on 26.4.0 and **28,179** on 24.22.3, which accepts malformed `xn--` payloads that 26 refuses — and
all of them are **loud**: the shim throws where Node answers.

Because an accept/refuse comparison cannot see a host both sides accept and encode DIFFERENTLY, the resulting host VALUE is
compared too, over every seventh BMP code point and a stride of the supplementary planes, in the between-letters and
label-initial positions. That found three real encoding faults the accept/refuse sweep could not: default-ignorable code
points were kept instead of removed (`a­b` was `xn--ab-5da` where Node says `ab`), a label could start with a combining
mark, and UTS46 case folding is not `toLowerCase`. The first two are fixed; the third — Cherokee, Cyrillic Extended-C, Greek
Extended's ypogegrammeni forms, U+0345, U+03F2/U+03F9 and the Greek mathematical alphanumerics — is **refused** rather than
mis-encoded, because reproducing it needs the UTS46 table. cleat-review's exhaustive value probe on this shim reads
**0 silent accepts, 0 silent value differences, 1,409 loud**.

**Rules that need context are probed with pairs**, which a single-code-point sweep cannot replace (cleat-review's pairs
probe, 57,600 hosts: 0 silent accepts, 0 silent value differences, 11,538 loud; a second probe over 50,880 hosts: 0 silent).
Three were found this way and each is pinned in `urls_idna_context.ts`:
- **capital sigma**: UTS46 folds it to sigma ALWAYS, while JavaScript's `toLowerCase` applies Final_Sigma and turns a
  label-final capital sigma into final sigma, so `ΕΛΛΑΣ.gr` was `xn--mxahsa0b.gr` instead of `xn--mxahsa5b.gr`. The shim folds
  U+03A3 before lowercasing;
- **combining-mark order**: lowercasing can move a mark out of canonical order (U+0130 + a mark), so NFC runs again after it;
- **bidi class AN labels**: Node 26 accepts a label of Arabic-Indic digits and of the other bidi-class-AN code points, and
  Node 24, the version CI pins, refuses them. The shim refuses the whole class — U+0600-U+0605, U+0660-U+0669, U+066B-U+066C,
  U+06DD, U+0890-U+0891, U+08E2, U+10D30-U+10D39, U+10E60-U+10E7E — which is loud against 26, exact against 24, the same
  version-split rule as the malformed `xn--` labels. cleat-review's digit probe reads 0 lone-label and 0 in-context silent.

`TestTypeScriptURLHostCodePointSweep` re-runs the code-point sweep (BMP in full plus a stride of the supplementary planes; the
whole range is longer than one guest execution is allowed), fails on any silently accepted code point or any host value that
differs where both sides accept, and reports the loud set against a sanity bound rather than a census.
`TestTypeScriptURLContextualRules` holds the joiner and bidi probes, `TestTypeScriptURLIDNAContextRules` the sigma, ordering
and digit rules, and `TestTypeScriptURLIDNASpecRefusesMalformedLabels` the malformed `xn--` labels.

**The contextual rules a single-code-point sweep cannot see** are covered by 48 probes in
`tests/typescript/testdata/conformance/urls_context.ts`, each in raw and `xn--` form, answered by both sides and held by
`TestTypeScriptURLContextualRules`: ZWNJ/ZWJ after a Devanagari and a Malayalam virama, ZWNJ between Arabic dual-joining
letters, the same joiners with no joining context, an all-RTL label, an RTL label ending in a European digit, an Arabic digit
and a combining mark, EN and AN in one RTL label, an RTL label starting with a digit, RTL beside a Latin label, an RTL letter
with a Latin one, and three Hebrew/Catalan context checks. Result, measured 2026-11-17: **0 silent, 12 loud** — the 12 are the
six valid joiner labels in their raw and `xn--` forms, which the guest refuses because `CheckJoiners` is not implemented. That
direction is loud, and it is cleat#4020. `CONTEXTO` is not applied by Node either, which the probe records: `l·x`, a middle dot with no Catalan
context, is accepted by both.



It **throws** `cleat: URL: <what> is not supported in the guest`, and never returns a guess, for setting `href`, `protocol`,
`username`, `password`, `host`, `hostname`, `port` or `pathname`. `TestTypeScriptURLRefusesWhatItCannotAnswer` holds each one, and
Node accepts or silently no-ops all eight setters, so each is loud in the guest and quiet in Node.

**That is loud to code that lets the error out, and not to code that catches everything.** zod's `z.url()` wraps `new URL` in a
`try`/`catch`, which is how the `ReferenceError` was swallowed in the first place: for an input in the list above `z.url()` still
reports "invalid" where Node says valid, and for a setter the error escapes `z.url()` entirely. So row 2 is fixed for every input
the shim answers and **remains silent through a catch-all for the ones it refuses** (the eight setters). It is not a support
claim for those; cleat#4016 holds them.

**Where Node itself departs from the specification** (measured on Node 26.4.0, not in the guest): a relative input that carries a
fragment is accepted against a base with an opaque path (`new URL("x#y", "foo:bar")` is `foo:bar/x#y`, while `new URL("x",
"foo:bar")` throws). The specification makes every one of them a failure unless the input starts with `#`, and the guest follows
the specification. Those pairs are excluded from the comparison in `urls.ts`.

## What fixing one costs

Row 1 is a patch over the setters in the plugin's initialisation script. Measured on macOS arm64, `hello.ts`: the plugin
grows by 377 bytes (1,082,221 to 1,082,598) and a deployed module by 4,340 bytes (1,125,603 to 1,129,943, about 0.4%), because
the initialised heap carries the patched functions.

Row 2, measured 2026-11-14 on the same machine with the same toolchain: the plugin grows by 1,405 bytes (1,274,892 to
1,276,297) and a deployed `hello.ts` module by **74,202 bytes (1,330,301 to 1,404,503, about 5.6%)**, for the compiled classes
that the initialised heap carries. That is the price of removing a silent wrong answer from every module, used or not.

## How a row is held

A conformance test runs one source, plain JavaScript in a `.ts` file under `tests/typescript/testdata/conformance/`, in both
places and compares what each prints: the guest through the module `build.sh` builds, Node through the same source bundled by
esbuild. The oracle is Node itself, not a belief about Node. Every fix is also run **with its shim left out of the build**
(`CLEAT_SHIMS_OFF=<name> REBUILD_PLUGIN=1`), and the test must fail: row 1 does (235 of 631 cases differ without it), and row 2 does (all three tests fail with `URL is not defined`).

The oracle is **Node 24 or newer** for the URL rows, and the tests fail, not skip, on an older one. Node 20 (which the tier-2 job used until
this row) leaves `^` unencoded in a path and drops the root slash of a non-special URL whose path `..` climbs above the root (`foo:`
where the current specification gives `foo:/`): against it the corpus reports 9 differences that are the oracle's age and not the
shim's. Measured 2026-11-14 on Node 20.20.2, 24.21.0 and 26.4.0; the last two agree with the shim on every case except the one
documented in the next paragraph. Two answers that vary by Node version, `new URLSearchParams(null)` and the key order of an
iterator's result object, are left out of the corpus.

**Where Node's own versions disagree, the shim follows the specification and names the version it is tested against.** One
place is known: shortening a `file:` path whose only segment starts with a drive letter but is longer than two code points.
The specification pops `C:x` (`file:///C:x/..` is `file:///`), Node 24 does the same, and Node 26 keeps it. The URL corpus is
compared against the Node CI pins (24), so that shape is excluded from the comparison and pinned guest-side by
`TestTypeScriptURLFileSpecShortening` -- neither version can quietly change the answer.

A second place, in the same family: **a malformed `xn--` label**. Node 26 refuses an empty payload (`http://a.xn--/`), one
that decodes to a control character (`xn--a`, `xn--1ug`, `xn--kba`), one that is not NFC or not lowercase (`xn--a-xbb`,
`xn--kkg`), and the Cherokee small letters; Node 24 accepts them. The shim takes the stricter answer and refuses, which is
exact against Node 26 and **loud** against Node 24 -- the direction the flip's condition allows. Those inputs are excluded
from the comparison (they would test whichever Node the runner has) and pinned guest-side by
`TestTypeScriptURLIDNASpecRefusesMalformedLabels`, together with five valid labels that must still be accepted.

A control build leaves the shim-less plugin in `packages/cleat-ts/.work/`, and the next ordinary build reuses it (cleat#3897): run the next build with `REBUILD_PLUGIN=1` to put the shipped plugin back.

## Where the guest differs from the other cleat SDKs

The rows above are guest-versus-Node. These are guest-versus-Go-and-Rust, and they matter for a different reason: a cleat
workflow's behaviour should not depend on which language it was written in. A tier-2 guest is allowed to differ where the
difference is **recorded here**; what it may not do is differ silently.

| # | Difference | What it means for you | Held by |
|---|---|---|---|
| D1 | **A killed workflow's defers now run — EXCEPT when the guest never reaches the interrupt poll.** The host stops a runaway guest through two soft arms, both read from `cleat_interrupt_requested`: within a reserve of the remaining fuel (when `--wasm-instruction-limit` is set) or of the remaining instance timeout (always). The plugin polls that from a QuickJS interrupt handler, QuickJS raises an exception user code cannot catch, and the Rust frames return rather than being abandoned — so nothing is left borrowed and the drain runs. Verified 2026-11-17: the defer body ran 5 of 5 runs, in a try/catch/finally guest whose `catch` and `finally` both stayed unrun. **The residue is a guest whose work never returns to the bytecode loop.** QuickJS polls once per ~10,000 bytecode operations, while fuel is spent by whatever an operation does, so one native operation can burn more than the reserve. Measured 2026-11-17 over four endless shapes: the largest gap between two polls was 26.2M fuel / 8.2ms (`JSON.parse` of a 200k-element payload), and a loop of `"x".repeat(1_000_000)` reached **no poll at all** in 400M fuel or in 2s of wall clock. Such a guest still ends on the hard trap, and its defers are still lost. | A workflow whose cleanup matters must not do its work in one giant native operation without ever yielding. That is true on every SDK — a native call that never returns starves every cooperative stop — but here it is the only remaining way to lose the cleanup. | `TestTypeScriptKilledWorkflowRunsItsDefers` and `TestTypeScriptDefaultConfigurationSoftKills` pin the two arms; `TestTypeScriptAKillTheGuestCannotObserveStillLosesItsDefers` pins the residue and fails, saying so, if it ever changes. The reserve numbers and how to re-derive them are in `engine/wasmtime_options.go`. |
| D2 | **A defer body that suspends stops the later bodies' calls.** Rust keeps calling the host from the remaining bodies after one suspends (`cleat-sdk`'s `run_deferred`: "the remaining bodies still run after a suspending one"). This guest's bindings refuse every call once the suspended flag is up, so the bodies after a suspending one run but record nothing. | Cleanup that follows a body which waits (a sleep, a promise) in the same defer drain is not performed in that segment. Each body's own calls up to the suspension are unaffected. | Not held by a test: it follows from `guarded()` in `crates/cleat-js-plugin/src/lib.rs`, and no fixture suspends inside a defer body yet. |

Defers otherwise behave as in the other SDKs: they run after the workflow returns **or throws**, in reverse registration
order, each body awaited, and one failing body does not stop the rest. They do **not** run when the workflow suspends — a
suspended run has not finished, and running cleanup at the first sleep would release work the resumed run still needs. The exception is a defer *segment* — the host replaying a workflow whose outcome is already decided purely to run its cleanup — where the host drains the table itself through `__cleat_run_deferred` (`TestTypeScriptDeferSegmentRunsOnlyTheDefers`).

## The test harness (cleat#3828)

`packages/cleat-ts/testing.ts` runs the module that gets deployed, so the guest cannot drift from what a worker runs. What
CAN drift is the host around it: the runner supplies stubbed answers and in-memory stores, and a stub that stopped
matching the shipped store would turn the harness from a detector into a green light. So the stubs are stated here
rather than assumed.

| # | Difference | What it means for you | Held by |
|---|---|---|---|
| H1 | **The harness scripts durable CALLS, PROMISES and CHILDREN, and queues SIGNALS.** A test may pass `answers` (`{"<service>/<operation>": <JSON text>}`); a durable call the script does not name is **refused** with the missing key, not answered with `{}`. It may pass `promises` (`{"<name>": <value>}`), `promiseRejects` (`{"<name>": "<message>"}`), `signals` (`{"<name>": [<payload>, …]}`) and `children` (`{"<name>": {"result": …}}` or `{"<name>": {"error": "…"}}`). A name a script does not cover is **not** invented: an unscripted promise stays pending, an unqueued signal is not found and an unscripted child stays **running**, so a workflow waiting on any of them **SUSPENDS** rather than failing or being handed an empty answer. | A scripted answer exercises **the workflow's handling of the value**, nothing else. **A promise is settled ON CREATE and a child is reported as finished BEFORE the run began** — stand-ins for the other run and for the child's own execution — so the harness tests "the workflow creates, awaits and uses the value", not the park-then-resume path either really takes. A signal is already queued when the run starts, which is the "delivered before the first poll" case and not the "delivered while parked" one. The child store answers `GetChildCompletedAtMs` with a near-zero instant for the same reason: a real child's completion is compared against the parent's durable clock (#847), and this process has one segment and no clock to compare. | **`cmd/cleat-ts-run`'s `scriptedCaller`/`loadAnswers`, `memoryPromises`, `memorySignals` and `memoryChildren`**, unit-tested in `main_test.go` and `stores_test.go` (all of which run without a toolchain). The end-to-end half is `packages/cleat-ts/testing.test.ts`, which pairs every store test with its control — scripted completes, unscripted parks — and includes a child that FAILS, so the parent's error path is exercised too. It runs in the tier-2 CI job. |
| H2 | **Updates are scripted through the `updates` store** (cleat#3827 group A). A test passes `scripts: { updates: [{ name, payload? }, …] }`; each is pending from the start and delivered oldest first, and `extras.updates` reports `{ completed, pending, settled }`. | **Every update is pending BEFORE the run starts**: this tests "the handler answers an update", not "an update arrives while the workflow is parked". A workflow that polls for none gets none, and a name that is not registered fails the run rather than configuring nothing. | `cmd/cleat-ts-run`'s update store and `TestTypeScriptUpdatesAreAnsweredByTheGuestsHandlers`; the worker half is the per-dialect job's test. |
