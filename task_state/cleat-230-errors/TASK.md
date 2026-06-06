# cleat-230-errorse — Error Message Quality Review

**Parent:** cleat-230 (Engine Reliability Polish)
**Budget:** ~$6.67 (~0.5 day)
**Priority:** 2 (feature/polish)
**Assigned to:** explorer agent (cleat-230-errorse)

## Scope

Review error messages surfaced to clew users through the dashboard and ensure they're actionable. The core principle: an error like "workflow failed: WASM trap" should include which trap and what the workflow was doing.

## Deliverables

1. Audit of error messages in engine hot paths (`internal/host/engine.go`, `cmd/cleat-worker/main.go`, and related files)
2. Identification of error messages that are not actionable (missing context, generic, misleading)
3. Specific recommendations for each error message that needs improvement
4. Implementation of the recommended error message improvements

## Files

- `internal/host/engine.go` — primary
- `cmd/cleat-worker/main.go` — primary
- Any other files in the error-reporting path from WASM execution to user-facing output

## Success Criteria

- Error messages include context: what was happening, which component, what to do
- "workflow failed: WASM trap" → includes which trap and what the workflow was doing
- User-facing errors are actionable without needing to read source code

## What NOT to Do

- Don't change error handling logic — only improve the messages
- Don't add new logging infrastructure
- Don't change error types or interfaces
