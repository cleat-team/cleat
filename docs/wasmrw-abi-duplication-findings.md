# `wasmrw` vs. the 7 production packing sites — duplication survey

Written 2026-08-09, Stream O (`fix/remove-dead-code`), addressed to whoever is doing the
uniform WASM entry-point/return work (origin/develop `7942b77`, `d9f7435`, `940758c`, `cf4d9c1`,
`115b421` — `engine/component_callbacks.go` / `engine/component_cgo.go`).

Stream O was briefed to consider deleting `wasmrw/wasmrw.go` (zero production callers, only
importer in the repo is a test that tests the package itself) or adopting it into 7 production
sites that supposedly hand-duplicate its packing scheme. **Do not treat this note as a
recommendation to delete or adopt** — that call is deferred to the ABI-unification work in
progress. This is the investigation only: are the 7 sites byte-identical to each other and to
`wasmrw`, or have they diverged?

## What `wasmrw` claims

```go
func OK() uint64                { return 0 }
func OKWithLen(n uint32) uint64 { return uint64(n) << 32 }
func Error(err error) uint64    { return 1 }               // ignores err — see Finding 3
func Suspend() uint64           { return 1 << 62 }
```

## Finding 1 — the suspend sentinel `1<<62` IS byte-identical everywhere

Re-derive: `grep -rn "1 *<< *62" --include="*.go" .`

Every producer and consumer in the tree uses the literal `1 << 62`: `wasm/exports.go:224`,
`engine/helpers.go:86` (`packAwaitChildResultSuspend`), and the readers `engine/runtime.go:552`,
`engine/backend_wasmtime.go:741,1392`, `packages/cleat-as/test_runner/test_runner.go:393`. No
divergence found on this half of the encoding.

## Finding 2 — the "len<<32 | code" packing is NOT one duplicated pattern

Re-derive: `grep -n "<<32\|<< *32\|<<40\|<<48\|<<8\b" engine/children.go engine/memory.go
engine/wasmtime_hostfuncs.go engine/helpers.go engine/scope.go engine/signaller.go
wasm/exports.go`

It is a family of at least four distinct bit layouts, and only one sub-family matches
`wasmrw`'s two-field shape:

**(a) length + error code, 32/32 split — matches `wasmrw.OKWithLen`'s shape:**
- `engine/children.go`: `errWritten<<32 | 4`, `errWritten<<32 | 3`, `written<<32 | 1` (codes 1,
  3, 4 depending on the failure)
- `wasm/exports.go:205`: `outLen<<32` (code 0, i.e. `OKWithLen`)
- `engine/helpers.go` `packAwaitChildResult`: `written<<32 | errCode`

**(b) two lengths packed together, no error code at all — divergent semantics, same bit split:**
- `engine/wasmtime_hostfuncs.go:110`: `argsLen | entryLen<<32`
- `engine/scope.go:183`: `objTypeLen<<32 | instKeyLen`

  A caller adopting a `wasmrw`-shaped helper here would be putting a *second length* where the
  API says "error code" — these are not error+length duplicates, they're a different value
  shape that happens to reuse the same 32/32 split.

**(c) length + boolean flags word, not an error code — same bit split, different meaning:**
- `engine/signaller.go:129`: `reasonWritten<<32 | 1` (`// cancelled=true`, not an error)
- `engine/signaller.go:142`: `written<<32 | flags` where `flags = 0x0100` (`// found=true`)

**(d) multi-field packs with >2 fields at non-32 shifts — no 2-field helper fits these:**
- `engine/helpers.go` `packAwaitSignalsResult`: `sigNameLen<<48 | payloadLen<<32 |
  toFlag<<16 | errCode` (4 fields)
- `engine/helpers.go` `packAwaitPromiseResult`: `resultLen<<32 | toFlag<<16 | errCode`
  (3 fields)
- `engine/helpers.go` `packAcquireLockResult`: `a<<8 | errCode` (shift 8, not 32)
- `engine/memory.go` `packDurableCallResult`: `responseLen<<40 | callErrorCode<<8 | errCode`
  (3 fields, shifts 40/8/0)

## Finding 3 — even within family (a), `wasmrw.Error` doesn't fit

`wasmrw.Error(err error) uint64` takes only an `error` and returns a bare `1` — it has **no
length parameter**. Every real call site in family (a) needs to pack a message length alongside
the code (e.g. `wasm/exports.go:219`: `(int64(msgLen) << 32) | 1`). So `wasmrw.Error` is not a
drop-in for any of the 7 sites as currently typed. Adopting it would require:

1. Changing its signature to accept a length (`Error(code uint32, n uint32) uint64`, or similar).
2. Deciding what the numeric codes *are* — the 7 sites currently use ad hoc values (1, 3, 4,
   `0x0100`, plain `errCode`/`callErrorCode` parameters) with no shared enum tying them together.

Both are ABI-ownership decisions, not a mechanical fix, which is why Stream O did not make them.
Per the coordinator's revised instruction, `wasmrw.Error`'s bug (silently discarding `err` and
always returning `1`) was **left unfixed** rather than fixed unilaterally, since any correct fix
requires deciding what the codes are — exactly the question the uniform-ABI work is answering.

## Recommendation

Don't delete `wasmrw` and don't force-adopt it while `engine/component_callbacks.go` /
`engine/component_cgo.go` are in flux. Decide the error-code vocabulary first — are the ad hoc
values above (1, 3, 4, `0x0100`, ...) meant to be one enum or several independent per-call-site
vocabularies — then `wasmrw`'s fate (become the shared 2-field encoder for family (a) only;
become a bigger set of named encoders covering (b)/(c)/(d) too; or stay deleted) falls out as a
consequence rather than a guess made in isolation from that work.
