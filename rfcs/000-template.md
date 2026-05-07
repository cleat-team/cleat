# RFC: [Title]

- **Status:** [Draft | Proposed | Accepted | Rejected | Implemented | Obsolete]
- **Created:** YYYY-MM-DD
- **Author(s):** @handle
- **PR:** link

## Summary

One paragraph explanation of the proposal.

## Motivation

Why is this change needed? What problem does it solve? What user workflows or
system behaviors are affected?

## Design

Detailed design of the proposed change. Include:

- API changes (new types, methods, function signatures)
- Schema changes (new tables, columns, indexes)
- New dependencies (libraries, tools, services)
- Behavioral changes (replay semantics, error handling, edge cases)
- Configuration changes (new flags, environment variables)

### Example

If applicable, show a before/after comparison of the user-facing API:

```go
// Before
result, err := h.DurableCall("service", "Op", request)

// After
result, err := h.DurableCallWithOptions(opts, "service", "Op", request)
```

## Alternatives considered

What other approaches were evaluated and why were they rejected? Include at
least 2-3 alternatives for significant changes.

## Impact

### Breaking changes

Does this change break existing workflows? List every breaking change.

### Migration path

How will existing users migrate? What steps are needed? Provide a concrete
migration guide if applicable.

### Performance implications

- Memory: WASM module size, cache pressure
- Latency: additional host calls, serialization overhead
- Throughput: database query patterns, lock contention
- Scalability: impact on `SKIP LOCKED` polling, worker concurrency

### Security implications

- New attack surfaces (network connections, file system access)
- Privilege escalations
- Data validation concerns

## Open questions

- [ ] Unresolved design decisions
- [ ] Items requiring further investigation
- [ ] Consensus needed from specific contributors

---

## RFC lifecycle

1. **Draft** -- Author is still iterating on the design
2. **Proposed** -- Ready for community review and discussion
3. **Accepted** -- Maintainers have approved; implementation may begin
4. **Rejected** -- Proposal was evaluated and declined
5. **Implemented** -- Changes are merged and released
6. **Obsolete** -- Superseded by a newer RFC or no longer relevant
