# How a host call is bound (cleat#3827)

Every host call still to be bound belongs to one **group**. A group is three small files and one inventory file, and a group's
PR edits only those, so groups written in parallel do not conflict.

| Layer | The group's file | What goes in it |
|---|---|---|
| inventory | `packages/cleat-ts/host-calls.d/<group>.json` | the group's calls, `unbound` with a `pending:` reason until bound; binding a call moves its entry to `bound` (global name -> ABI export) **in this file** |
| plugin | `crates/cleat-js-plugin/src/groups/<group>.rs` | one `g.set("__cleat_<name>", Func::from(...))` per call, inside `install` |
| TypeScript | `packages/cleat-ts/groups/<group>.ts` | the `declare function __cleat_<name>`, the methods added to `<Group>Ctx`, and their implementations in `make<Group>` |

The groups are `updates`, `signals`, `children`, `detached`, `locks`, `schedules`, `continuation`, `calls`, `plugins` and
`utilities`. The mapping of calls to groups is the contents of `host-calls.d/`. Nothing else needs editing to bind a call:
`index.ts` already extends `Ctx` with each `<Group>Ctx` and spreads each `make<Group>(h)` into the context, and `lib.rs` already
calls every group's `install`.

`typescript_bindings_test.go` reads the union of `host-calls.json` and `host-calls.d/*.json`, the plugin's `src/` tree and
`index.ts` plus `groups/*.ts`, and fails on any disagreement. A call listed in two inventory files is a failure, not a merge.

## Conventions

A group follows these so ten authors produce one API.

1. **A waiting call returns a promise, made by `h.settle`.** The host answers a suspension with `{"suspended":true}` and `settle`
   turns that into a promise that never settles. Never throw for a suspension and never catch one.
2. **A call that does not wait is synchronous**, like `cancellation()` and `getScope()`: a poll returns its answer, it does not
   return a promise.
3. **Every effectful binding goes through `guarded` in the plugin.** Once a segment has suspended, a binding that reaches the
   host records an event the run does not own. A binding that only reads and records nothing may skip it, and the comment says why.
   Each group's PR carries a late-call test in the shape of `TestTypeScriptACallAfterASuspensionRecordsNothing`: the call after a
   `sleep` must leave no event in the history. Show it fail with `guarded` removed.
4. **Everything that leaves the guest goes through `h.encode`**, and everything that comes back as JSON text through
   `h.parseJSON`, so NaN and infinities are refused the way Go's `encoding/json` refuses them.
5. **Options are camelCase objects**, and an option the host does not read is not accepted.
6. **An error is a `CleatError`** carrying the host's own message. Do not replace it with a legend.
7. **Say what the call does on replay**, in the doc comment of its `Ctx` method. A call whose answer is not a function of
   recorded state (see cleat#3917) says so.
8. **A `Ctx` method name belongs to one group.** `ctx` is built by spreading each group's object into one literal and nothing runs
   `tsc` here, so two groups adding the same name would not fail to build: the later spread would silently win.
   `TestNoTwoGroupsAddTheSameCtxMethod` fails on it, naming both files; check the existing names in `index.ts` before choosing one.
9. **Do not edit `index.ts`, `host-calls.json` or `lib.rs`** from a group. If a group needs a change to one of them, that change
   is its own PR and the other groups are told.
10. **Do not edit the prose that says what TypeScript lacks** (`DIVERGENCES.md`, the getting-started page, `tiers.yaml`,
   `LANGUAGE_SUPPORT.md`). Put a group's harness divergences and new limits in its PR body; one closing PR collects them.
11. **State the plugin's size change in the PR body**: `packages/cleat-ts/.work/cleat-plugin.wasm` before and after.

## Binding a call, step by step

1. Move the call's entry from `unbound` to `bound` in `host-calls.d/<group>.json`, as `"__cleat_<name>": "<export>"`.
2. Install the global in `groups/<group>.rs`, and add the `declare function` in `groups/<group>.ts`.
3. Add the `Ctx` method and its implementation.
4. Run `go test ./tests/typescript/ -run 'TestEveryHostCallIsBoundOrListed|TestReconcileBindingsSelfTest'`. It fails until all
   four sources agree, and that is the check that the call is bound everywhere it has to be.
5. Write the end-to-end test against the real engine and the falsification (remove the binding, or its `guarded`, and watch
   the test go red for the reason you expect).
