You are working on one stream of a multi-stream plan. You need to ask for the name of the plan, and which stream is your responsibility.

Read that plan first to understand the full context — it defines several independent streams plus cross-cutting concerns. Your stream and its scope are specified in the plan.

## Quality standards (every error/warning you emit)

Every error or warning message must satisfy these five criteria:
1. **WHAT** — what went wrong (the specific condition or constraint violated)
2. **WHERE** — the exact file path, line number, or code location
3. **WHY** — why it matters (correctness risk, ABI contract, safety invariant)
4. **HOW** — the fix direction (not just "fix this")
5. **CLARITY** — a new team member could diagnose the issue from the message alone

## Execution

Implement the entire stream using up to 5 subagents. The top-level agent does NO implementation work.
It reports subagent progress at 5-minute intervals and writes a summary of findings and work done.

## Review phase

When all subagents complete:
1. Thoroughly and skeptically review every code change for correctness, consistency, and style. Fix any issues yourself.
2. Run all tests. Fix any failures.
3. Verify that no regressions were introduced — check that existing tests still pass and existing functionality is preserved.

