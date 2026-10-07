"""Minimal typed Python workflow: one required integer parameter.

cleat#1981's Python fixture. Built by `cleat build --target python`, which
emits a .schema.json sidecar that the start path validates against.

Uses `@cleat_entry("count_workflow")`, the preferred decorator form. The bare
`@cleat_entry` is equally supported and resolves to the same key -- with no
argument, cleat_sdk falls through to `func.__name__`, which is the string this
form passes explicitly -- and the SDK calls that form legacy rather than
preferred. That is the whole reason: a style preference stated in
`cleat_entry`'s own comment in cleat_sdk/entry.py -- "without parentheses,
legacy" against "with parentheses, preferred" -- and not a defect in either
form.

This paragraph used to say the opposite, and the reason it is worth the words:
it asserted the bare form was BROKEN, naming the dual-form branch as the cause
-- the registry keyed by the function object rather than a string, the emitter
then dying in json.dumps. That was true when it was written. cleat#2985 fixed
it, and the branch the sentence blamed is now the code that handles the case.
Kept in the past tense so it cannot be re-added, because the reader who reached
this file was being told the supported form did not work.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("count_workflow")
def count_workflow(h: HostCalls, count: int) -> str:
    h.log(f"count={count}")
    return str(count)
