# cleat-236 Status

**Phase:** completed (audit)
**Last updated:** 2026-06-05
**Dispatched by:** cto-lap-032

## Summary

Full documentation audit completed across 8 files + codebase verification. Found **28 discrepancies** (6 critical, 14 important, 8 minor). Major themes: stale host import counts ("15"/"22" instead of 54), outdated line counts and SDK metrics, phantom cmd directory, and two implemented security features (signal auth, encryption-at-rest) not documented.

---

## Findings by Document

### 1. ARCHITECTURE.md — 8 issues (1 critical, 4 important, 3 minor)

**Critical:**
- References `cmd/clew-service/` in module table and coupling matrix, but this directory **does not exist**. No code was ever built for it. Remove or mark as planned.

**Important:**
- Module table omits ~14 significant directories: `cmd/cleat/`, `cmd/cleat-gen/`, `cmd/cleat-bench/`, `cmd/cleat-gen-plugin/`, `cmd/cleat-plugin-verify/`, `cmd/deploy-workflow/`, `cmd/durable-worker/`, `cmd/wit-rewrite/`, `crates/`, `python-sdk/`, `plugins/`, `cleat/` (public API), `pluginapi/`, `wasm-demo/`
- No mention of signal authorization despite it being a major feature (enabled by default)
- TinyGo described as "default and only Go WASM target" (line 112) — but standard Go (`--target go`) is the default now
- Missing encryption-at-rest documentation

**Minor:**
- `engine/` module description says "WASM runtime" but references `wasm/` module for that. Engine doesn't own WASM runtime directly.
- Module table says `wasmrw/` "depends on" nothing, but it may depend on standard library patterns
- The correct top-level package paths are `wasm/`, `wasmrw/`, `migration/`, `auth/`, `plugin/` (not `internal/host/`, `internal/wasm/`, `internal/auth/`). ARCHITECTURE.md is actually correct here — the TASK.md's claim of stale paths was wrong.

**Verified correct:**
- ChildWorkflow API documentation is present and accurate
- All three DB backends (PostgreSQL, MySQL, MSSQL) are mentioned
- Module paths that ARE listed are correct (packages exist where claimed)

---

### 2. ABI.md — 3 issues (2 critical, 1 minor)

**Critical:**
- Documents **52** host imports (sections 2.1–2.52), but the engine registers **54**. Missing: `cleat_poll_child` and `cleat_await_any_child`. Both are fully implemented in `engine/imports.go`, `engine/backend_wasmtime.go`, `wasm/generator.go`, and the Rust/AS SDKs.
- Because of the missing entries, section numbering from 2.44 onward is off by 2. The Rust SDK correctly labels `cleat_workflow_id` as ABI 2.46 but ABI.md lists it as 2.44.

**Minor:**
- Changelog v3 entry (line 1399) says "Expanded from 22 to 50 documented host functions" — actual was 50 to 52 (not counting the now-missing 2). Count references are stale.

---

### 3. CHANGELOG.md — 2 issues (1 critical, 1 minor)

**Critical:**
- No 0.5.0 entry. The [Unreleased] section is a skeleton with one bullet point. Needs a comprehensive 0.5.0 entry summarizing all changes since 0.1.0.

**Minor:**
- 0.1.0 entry is a placeholder with all "Nothing" entries. Should either be filled in or removed if 0.1.0 was never actually released.

---

### 4. LANGUAGE_SUPPORT.md — 6 issues (2 critical, 4 important)

**Critical:**
- "15 host functions" appears **4 times** (lines 11, 25, 48, 73). The actual count is 54. This is the most pervasive stale number across all docs.
- Python: "22 host imports" appears **4 times** (lines 141, 150, 268, 270). The actual count in `host_calls.py` is 53 `_import_*` stubs.

**Important:**
- Rust SDK line counts are all wrong:
  - Claims "537 lines total (host_calls 290 + memory 126 + proc-macro 121)"
  - Actual: `host_calls.rs` = 1,519, `memory.rs` = 170, proc-macro = 423. Full SDK src = 3,896 lines.
- Python SDK: claims "4,508 lines". Actual: 13,490 total (10,552 without generated `_wit/` code).
- Python tests: claims "80 tests pass (61 memory/encoding, 19 entry decorator)". Actual: 332–471 tests across 8 test files.
- Rust SDK source comment in `host_calls.rs` itself says "18 WASM host function imports" — also stale.

---

### 5. SECURITY.md — 5 issues (2 critical, 2 important, 1 minor)

**Critical:**
- **Missing signal authorization section.** Signal auth is a complete, default-enabled feature (see `engine/engine.go` `requireSignalAuth`, `engine/db.go` `GetAllowedSignalCallers`, `cmd/cleat-worker/main.go` enforcement at WASM and HTTP layers). It has tests and works across all three DB backends. Not documented anywhere.
- **Missing encryption-at-rest section.** AES-256-GCM encryption of 11 sensitive event fields exists for PostgreSQL (see `engine/encryption.go`, `--encrypt-sensitive-payloads`, `--encryption-key-file`). Not documented anywhere. MySQL/MSSQL have stub methods but encryption is not yet functional for them — this should also be noted.

**Important:**
- "15 cleat host function imports" (line 144) — should be 54.
- Supported versions table says "1.x" is supported, but the project is pre-1.0. This makes pre-1.0 users think they're unsupported. Fix: list 0.x versions as supported or clarify that "pre-1.0" receives security updates during open beta.

**Minor:**
- References TinyGo as "the compilation tool for Go workflows" (line 226) — standard Go (`--target go`) is the default now.

---

### 6. CONTRIBUTING.md — 4 issues (1 critical, 2 important, 1 minor)

**Critical:**
- PR target says "main" branch (line 305), but the repo's default branch is `develop`. New contributors will clone `develop` and be told to open PRs against `main`. CI triggers on both, so either fix the doc to say `develop` or change the default branch.

**Important:**
- Go version matrix: line 323 claims "Go (1.22/1.23/1.24 matrix)" but CI only tests Go 1.26. There is no matrix. Update to reflect the single Go 1.26 target.
- Prerequisites table says "Go 1.26+" (line 59) but `go.mod` says `go 1.25.7`. Minor mismatch — the doc is slightly ahead of the actual minimum.

**Minor:**
- Build instructions (line 231) say "Compile with TinyGo (the default and only Go WASM target)" but standard Go is now the default.

---

### 7. README.md — 1 issue (minor)

**Minor:**
- Quickstart (`cleat build -o ./out ./testdata/basic/`) — `testdata/basic/` exists, but the output filename `place_order.wasm` in step 3 doesn't match the build command's `-o ./out` which would produce a directory. The flow works but could be clearer.

**Verified correct:**
- All local file references (docs/tutorials/, docs/how-to/, docs/reference/, docs/explanation/, docs/operations/, docs/migration/, docs/index.md, CONTRIBUTING.md, LICENSE) resolve correctly.
- All external badges and links are well-formed.
- `testdata/basic/` exists.

---

### 8. DX_COMPARISON.md — 7 issues (2 critical, 3 important, 2 minor)

**Critical:**
- "202 documented issues across 19 ports" (lines 3-4, 333) — **unsubstantiated**. Only ~38 port issues documented across 3 ISSUES.md files. No aggregate tracking file exists. These numbers cannot be verified.
- "34 WIT imports" for Python SDK (line 23) — no "34" found anywhere. The WIT file defines 18 interfaces (49 individual functions). The number appears fabricated or from a stale source.

**Important:**
- Rust SDK "1,090 lines" (line 83) — `host_calls.rs` alone is 1,519 lines. Full SDK src is 3,896 lines.
- Java SDK "2,072 lines total" (line 101) — main src alone is 7,492 lines. Total Java files are 13,288. If "2,072" refers to what was added in the hardening pass, the doc should say so explicitly.
- "end-to-end end-to-end" duplication at lines 23-24 and again at lines 149-150. This is a copy-editing artifact.

**Minor:**
- "88M steps/sec" (line 30) — documented in-process maximum is ~86.6M steps/sec. The 88M figure is rounded up and doesn't account for WASM overhead. Minor but should be precise.
- AS test harness "1,626 lines" (line 128) — VERIFIED CORRECT.

---

## Cross-Cutting Issues

### Stale Host Import Count ("15")

The number "15" appears as the host import count in **4 files**: LANGUAGE_SUPPORT.md (4 occurrences), SECURITY.md (1 occurrence), plus the Rust SDK source comment. This was the count at ABI v1 (2026-05-05). The current count is 54. All references need updating.

Affected files: `LANGUAGE_SUPPORT.md` (lines 11, 25, 48, 73), `SECURITY.md` (line 144), `crates/cleat-sdk/src/host_calls.rs` (line 1 comment — says "18").

### Stale "22" for Python Host Imports

LANGUAGE_SUPPORT.md uses "22" for Python host imports in 4 places. The actual count in `host_calls.py` is 53.

### Unsubstantiated Metrics in DX_COMPARISON.md

"202 issues" and "19 ports" are central to DX_COMPARISON.md's credibility. Neither number can be verified from the repository. Only 3 port ISSUES.md files with ~38 issues exist. Either the claims need documented backing or they should be softened/removed.

### Undocumented Security Features

Both signal authorization and encryption-at-rest are complete, tested, default-enabled features that appear nowhere in the documentation. This is the highest-value fix — SECURITY.md is where potential adopters look to assess production readiness.

---

## Implementation Priority

| Priority | Document | Issue | Effort |
|----------|----------|-------|--------|
| P0 | ABI.md | Add `cleat_poll_child` + `cleat_await_any_child` (2.44/2.45), renumber | ~30 min |
| P0 | SECURITY.md | Add signal auth and encryption-at-rest sections | ~1 hr |
| P0 | CHANGELOG.md | Write 0.5.0 entry | ~2 hr |
| P1 | SECURITY.md | Fix "15" → "54", fix version support table | ~15 min |
| P1 | CONTRIBUTING.md | Fix branch target (develop vs main), Go version matrix | ~15 min |
| P1 | LANGUAGE_SUPPORT.md | Fix ALL stale numbers (15→54, 22→53, line counts, test counts) | ~1 hr |
| P1 | ARCHITECTURE.md | Remove `cmd/clew-service/`, add missing packages, add signal auth + encryption | ~1 hr |
| P2 | DX_COMPARISON.md | Fix unsubstantiated numbers, stale SDK line counts, "end-to-end" dup | ~1 hr |
| P2 | README.md | Clarify quickstart output path | ~5 min |
| P3 | Multiple | Fix TinyGo references (standard Go is default now) | ~15 min |
