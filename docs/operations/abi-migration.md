# ABI versions

**One ABI version has ever shipped.** There is no migration to perform, and
this document does not describe one.

    The current ABI version is 1.

That sentence is asserted against `wasm.CurrentABIVersion` by
`wasm/the_abi_runbook_agrees_with_the_shipped_version_test.go`, so it cannot
quietly disagree with the code the way this file's previous contents did. The
constant is the authority; re-derive it with:

```sh
grep -n 'CurrentABIVersion = ' wasm/metadata.go
```

## What this file used to say, and why that matters

Until cleat#1322 this was a 377-line runbook for migrating workflows **from ABI
v4 to ABI v5**. It carried a five-version history table, a compatibility matrix
mapping ABI versions onto engine releases `0.1.x` through `0.5.x`, and six
numbered steps of shell commands.

None of it had happened. Measured on `develop`:

| the runbook said | measured |
|---|---|
| ABI v1 … v5, each added in a release | `CurrentABIVersion = 1` — one version, ever |
| engine releases `0.1.x` … `0.5.x` | two tags exist: `v0.1.0`, `v0.2.0` |
| v5 "changed event history checksum algorithm to BLAKE3" | `computeEventChecksum` uses **xxhash** (`engine/store_promises.go`); no BLAKE3 in `engine/` |
| `cleat start …` in the migration steps | no such subcommand; it was parked in `cmd/cleat/documented_cli_surface_test.go`'s baseline as a real defect |

The v5 row is the one worth pausing on: it was not an invented *history*, it
was a false claim about **present behaviour**. A reader debugging a checksum
mismatch would have gone looking for BLAKE3.

It is recorded here rather than deleted silently because the failure is
instructive and recurs: a document describing a procedure nobody can run
produces **no signal when it is wrong**. Nothing fails, no test goes red, and a
reader who tries to verify it concludes they are looking in the wrong place
rather than that the document is fiction. That is why it survived long enough
to be found by a review rather than by use.

## What the ABI actually is

The interface between a compiled WASM workflow module and the cleat worker
runtime:

- host function signatures — import names, parameter layout, return encoding
- the memory model — linear memory layout, buffer sizes for string I/O
- error code conventions
- SDK version expectations

Each row in `workflow_defs` carries an `abi_version` column
(`migrations/*/001_schema.sql`), and every module carries its own claim in its
`cleat.metadata` custom section — the same section `DetectLanguage` reads to
choose a backend.

## What happens when a module claims a newer ABI

It **runs, with a warning.** `Engine.warnIfModuleWantsANewerABI`
(`engine/executor.go`) compares the module's `abi_version` against
`wasm.CurrentABIVersion` and logs when the module's is higher:

```
this module was built against a newer host ABI than this worker implements;
it is being executed anyway, and a host call it expects may be missing
```

**It does not refuse, and that is deliberate** — cleat#1054 proposed refusing
and the reason it warns instead is worth knowing before anyone changes it:
while `CurrentABIVersion` is 1 no module can declare a higher version, so a
rejection path would ship having never run. A warning exercises the comparison
on every execution and cannot break a working guest.

Two consequences an operator should know:

- The value is the **module's own claim about itself**, so a guest can lie
  about it. Acceptable for a warning; not acceptable for a refusal without more
  thought than the comparison itself needs.
- The check is at the **worker**, not at deploy. The failure it describes is a
  property of a pair — a module built against ABI 2 claimed by an ABI 1 worker
  — and only the worker knows its own side. A deploy-time check would pass on a
  fleet where half the workers cannot run what it accepted.

## When there is a second ABI version

No procedure is written here, because writing one now would be the same
speculation this file previously contained. What would have to be **decided**,
rather than guessed, at that point:

- whether `warnIfModuleWantsANewerABI` becomes a refusal, and on what evidence
  — the comment above is the argument to answer
- whether a worker must load every older ABI, and for how long
- whether `abi_version` is sufficient, or a minimum-compatible floor is needed
  (`wasm.Metadata` already carries `MinCompatibleVersion`)
- what `cleat` subcommands a migration would actually use — the previous
  runbook's did not exist

`docs/operations/upgrading.md` covers engine upgrades, which is a different
question and is real today.
