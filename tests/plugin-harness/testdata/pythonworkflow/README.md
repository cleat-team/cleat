# pythonworkflow fixtures

**`call_all_plugins.wasm` and `call_all_plugins.wasm.component.wasm` do NOT instantiate on the current engine.** Do not use either to run a workflow.
They are a 19,300,914-byte componentize-py build from before IMPROVEMENT-PLAN 3.220 removed the `send-signal-and-wait` host calls; instantiating one fails with

    component imports instance `cleat:host-calls/durable-signals`, but a matching implementation was not found in the linker
      instance export `durable-send-signal-and-wait` has the wrong type

`engine/a_checked_in_python_component_that_cannot_link_says_why_test.go` asserts exactly that failure, so this note cannot go stale unnoticed: it fails
if the fixture is refreshed or the import returns, and says what to update.

## Why it is kept

IMPROVEMENT-PLAN 3.308: the committed bytes are a **witness**. They carry 9 occurrences of `register_query_handler`; a fresh build carries 1.
`engine/imports.go` cites this file as the reason `RegisterQueryHandler` survives as a no-op host import (removing an import breaks guests already
compiled against it). **That claim is currently unfalsifiable**, because 3.220 removed another import this guest needs, so it cannot link either way (cleat#4260).
Whether the policy still holds is an ABI decision, not made here.

componentize-py's output is not reproducible (five builds of unchanged source gave five different digests, 3.308), so **a refreshed copy cannot be kept
current and committing new bytes is not the fix.** The harness never reads these files: `tests/plugin-harness/wasm_plugin_test.go` builds the workflow from
`plugin_harness_workflow.py` into a temporary directory every run.

## What reads them

| reader | what it needs from the bytes |
|---|---|
| `engine/a_component_is_compiled_once_test.go` (`TestTheCheckedInPythonComponentIsCompiledOnce`) | a real 19 MB component to compile; the call is *expected* to fail at link time, and the test asserts the compile count and nothing the guest does |
| `engine/the_compiled_size_estimate_still_holds_test.go` | the file's size and compiled-size ratio |
| `python-sdk/tests/test_stamp_metadata.py` | a real component to stamp and read metadata back from |
| `engine/a_checked_in_python_component_that_cannot_link_says_why_test.go` | the link failure above |

`call_all_plugins.wasm.component.wasm` is byte-identical to `call_all_plugins.wasm` (the build's own backup copy) and is read by no test; whether to delete
the duplicate is open on cleat#4260.

To run a current build of this workflow, build it: `cleat build --target python --entry plugin_harness_workflow.py:call_all_plugins -o <dir>`
(needs `componentize-py`; the `cleat-py-toolchain` image has it).
