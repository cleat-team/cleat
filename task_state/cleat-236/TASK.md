# cleat-236: Documentation Audit

**Budget:** $15 (~2 days)
**Priority:** 2 (public-facing)
**Status:** pending
**Depends on:** none

## Scope

Documents must be accurate for a public 0.5 release.

## Actions

1. **ARCHITECTURE.md**: update with current component layout, mention all 3 DB backends, document signal authorization, fix stale module paths
2. **ABI.md**: verify WASM import/export signatures match current code
3. **CHANGELOG.md**: prepare 0.5.0 entry summarizing changes since 0.1.0
4. **LANGUAGE_SUPPORT.md**: update with current SDK status, accurate line counts, known limitations
5. **SECURITY.md**: verify accuracy, add signal auth and encryption-at-rest sections
6. **CONTRIBUTING.md**: verify dev setup instructions work from scratch
7. **README.md**: verify quickstart instructions, badges, links
8. **DX_COMPARISON.md**: update if any competitive facts changed
9. Fill any documentation gaps found during code review (item 5)

## Key Files

- `ARCHITECTURE.md` — stale module paths, missing ChildWorkflow docs
- `ABI.md` — missing `cleat_poll_child`, `cleat_await_any_child`
- `CHANGELOG.md` — no 0.5.0 entry
- `SECURITY.md` — missing "signal auth" and "encryption-at-rest" sections
- `LANGUAGE_SUPPORT.md`
- `CONTRIBUTING.md`
- `README.md`
- `DX_COMPARISON.md`

## Additional Scope (from surveys)

- Fix stale module paths in ARCHITECTURE.md (`internal/host/`, `internal/wasm/`, `internal/auth/` → correct paths)
- Fix "15" host imports to "53" across ~7 files
- ARCHITECTURE.md missing ChildWorkflow API documentation
