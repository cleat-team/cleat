# CONTRACT: cleat-236 — Documentation Audit

## Deliverables

1. **ARCHITECTURE.md**: Updated with correct module layout, all 3 DB backends, signal auth, ChildWorkflow API, correct host import count (53)
2. **ABI.md**: Verified against `engine/imports.go` (53 `cleat_*` exports), added `cleat_poll_child`, `cleat_await_any_child`
3. **CHANGELOG.md**: 0.5.0 entry summarizing changes since 0.1.0
4. **LANGUAGE_SUPPORT.md**: Updated with current SDK status
5. **SECURITY.md**: Signal auth section, encryption-at-rest section added
6. **CONTRIBUTING.md**: Dev setup verified, corrected if needed
7. **README.md**: Quickstart, badges, links verified
8. **DX_COMPARISON.md**: Competitive facts updated if changed

## Invariants

- All documented facts verified against current code — no stale claims
- Host import count: 53 `cleat_*` (+3 non-cleat = 56 total) — not "15" as stale docs claim
- Module paths match current directory layout (no references to merged/deleted packages)

## Per-File Checklist

| File | Current Issues | Fix |
|------|---------------|-----|
| ARCHITECTURE.md | Stale paths (`internal/host/`, `internal/wasm/`, `internal/auth/`), missing ChildWorkflow docs, missing signal auth, wrong import count | Update all |
| ABI.md | Missing `cleat_poll_child`, `cleat_await_any_child` | Add with correct signatures |
| CHANGELOG.md | No 0.5.0 entry | Create from git log since 0.1.0 |
| SECURITY.md | Missing signal auth, missing encryption-at-rest | Add both sections |
| LANGUAGE_SUPPORT.md | May be stale | Verify and update |
| CONTRIBUTING.md | Unknown | Verify dev setup from scratch |
| README.md | Unknown | Verify quickstart, badges, links |
| DX_COMPARISON.md | Unknown | Check competitive facts |

## Test Requirements

- All links in all docs verified (no 404s)
- All code snippets in docs verified to compile/run
- Host import count verified against `engine/imports.go`

## Integration Points

- Consumes findings from cleat-235 (code review) for doc gaps
- ARCHITECTURE.md also touched by cleat-231 (ChildWorkflow docs) — coordinate
- ABI.md also touched by cleat-231 (ChildWorkflow host calls) — coordinate

## Coupling

- LOOSE with `cleat-231` (same ARCHITECTURE.md and ABI.md files)
- LOOSE with `cleat-235` (consumes code review findings for doc gaps)
- NONE with other leaf tasks
