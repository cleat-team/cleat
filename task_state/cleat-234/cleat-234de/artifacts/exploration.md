# cleat-234de Exploration Report — CI Enforcement Verification

**Date:** 2026-06-06
**Explorer:** cleat-234de (verification pass on cleat-234)
**Prior work:** cleat-234 STATUS.md (explored 2026-06-05 by explorer agent)

## 1. What's here now?

The cleat-234 STATUS.md contains an 8-section exploration covering CI workflow gaps, ecosystem CI, continue-on-error audit, branch protection, closure test failures, coverage architecture, TinyGo assessment, and a prioritized change list. This report verifies each finding against the current source.

### Files verified

| File | Verification |
|------|-------------|
| `.github/workflows/ci.yml` (656 lines) | Full read, all sections confirmed |
| `.github/workflows/ecosystem-ci.yml` (75 lines) | Full read, all gaps confirmed |
| `.github/workflows/multi-db-ci.yml` (176 lines) | Full read, coverage confirmed |
| `.github/workflows/plugin-harness-ci.yml` (164 lines) | Full read, TinyGo patterns confirmed |
| `.github/workflows/e2e-cross-language.yml` | Header read, confirmed separate from ci.yml |
| `internal/closure/closure_test.go` (414 lines) | Full read, both test bugs confirmed |
| `testdata/basic/order.go` (192 lines) | Full read, LongRunning at line 175 confirmed |
| `Makefile` (coverage section, lines 130-195) | Full read, module prefix bug confirmed |
| `go.mod` | Confirmed `go 1.25.7` vs CI's `1.26` |

## 2. Verified findings

### 2a. Closure test failures — CONFIRMED

`LongRunning()` at `testdata/basic/order.go:175` calls `h.DurableCall("noop", "", "")`. This makes it a 9th durable leaf.

**TestComputeBasicIdentifiesDurableLeaves (line 17):** `expectedLeaves` map has 8 entries, missing `basicFQ("LongRunning")`. The assertion `len(cr.DurableLeaves) != len(expectedLeaves)` will fail: 9 != 8.

**TestComputeBasicCorrectlyTagsPureFunctions (line 101):** `totalFuncs != 12` check will fail — `result.Funcs` now has 13 entries (8 original leaves + 4 closure + 1 LongRunning).

Fix is exactly as described in STATUS.md:
- Line 40: add `basicFQ("LongRunning"): true,` to `expectedLeaves`
- Line 120: change `12` to `13`

Also note: the comment on line 31 says "All eight functions" — should become "All nine functions" for consistency.

### 2b. Coverage job — CONFIRMED

ci.yml line 554-556: `if: github.ref == 'refs/heads/main' && github.event_name == 'push'` — only fires on push to main. PRs never get coverage results. Also line 558: `continue-on-error: true` means even main-push coverage failures don't block.

### 2c. test-tinygo disabled — CONFIRMED

ci.yml line 211: `if: false`. The comment says "Temporarily skipped; Go version issues being explored on another branch". The job when enabled tests `internal/wasm`, `internal/host`, `internal/closure`, and `internal/analyzer` — the closure test failures would be caught here.

### 2d. lint-go commented out — CONFIRMED, with new information

ci.yml lines 99-112. The comment says "action doesn't support Go 1.26 yet".

**NEW FINDING:** golangci-lint v2.9.0 (released 2026-02-10) added Go 1.26 support. v2.10.1 is current as of 2026-02-17. The blocker is that pre-built binaries compiled against older Go versions refuse to analyze Go 1.26 modules. Workaround:

```bash
GOTOOLCHAIN=go1.26.1 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1
```

Notes for re-enablement:
- Needs v2 config format (`version: "2"` in `.golangci.yml`)
- `run.deadline` → `run.timeout`, `typecheck` removed (always-on in v2)
- Formatters like `gofmt`, `goimports` moved to a `formatters` section

### 2e. Go version discrepancy — CONFIRMED

`go.mod:3`: `go 1.25.7`
ci.yml: `GO_VERSION_STABLE: "1.26"`

The CI compiles with Go 1.26 while go.mod declares 1.25.7. This works because Go 1.26 is backwards-compatible, but the go.mod should declare the version actually used in CI. Recommend bumping go.mod to `go 1.26.0` (or `1.26`).

### 2f. Multi-DB CI — CONFIRMED

`multi-db-ci.yml` is a separate workflow that covers:
- test-mysql: MySQL 8.4 service container, runs `./internal/host/...`
- test-mssql: SQL Server 2022 via Docker, runs `./internal/host/...`
- test-plugin-migrations: PostgreSQL + MySQL + MSSQL simultaneously, runs `TestPluginMigrations` and `TestPluginCalls_MultiDB`

The STATUS.md claim that this is "architecturally fine but means a PR doesn't get single unified status" is accurate. The multi-db-ci job names are `Test MySQL`, `Test SQL Server`, `Plugin Migrations` — these should be added to branch protection required checks.

### 2g. TinyGo scattered installation — CONFIRMED

Four locations install TinyGo with near-identical scripts:
1. ci.yml test-go job (lines 171-181) — for internal package group
2. ci.yml test-tinygo job (lines 235-244) — disabled
3. plugin-harness-ci.yml test-layer2 (lines 40-49) — WASM integration
4. plugin-harness-ci.yml test-multi-db (lines 106-115) — multi-DB

Plus a `tools-tinygo` Makefile target for local dev. All four use the same fallback: `v0.41.1`. A composite action would eliminate ~60 lines of duplication.

### 2h. Ecosystem CI path filter — CONFIRMED

ecosystem-ci.yml lines 5-10: only triggers on `cleat/**`, `internal/**`, `cmd/**`, `go.mod`, `go.sum`. Missing: `plugins/**`, `crates/cleat-sdk/**` (Rust SDK), `python-sdk/**` (Python SDK), `packages/cleat-as/**` (AS SDK). A change to plugins that breaks cross-SDK compatibility won't trigger ecosystem CI.

## 3. continue-on-error audit — ALL 12 CONFIRMED

| # | File | Line | Context | Verdict |
|---|------|------|---------|---------|
| 1 | ci.yml | 52 | lint job | REMOVE |
| 2 | ci.yml | 81 | ruff check step | Keep (informational) |
| 3 | ci.yml | 85 | shellcheck step | Keep (informational) |
| 4 | ci.yml | 267 | test-python job | REMOVE |
| 5 | ci.yml | 297 | test-java job | REMOVE |
| 6 | ci.yml | 319 | test-assemblyscript job | Keep (AS fragility) |
| 7 | ci.yml | 369 | test-assemblyscript-wasm job | Keep (AS fragility) |
| 8 | ci.yml | 490 | build job | REMOVE |
| 9 | ci.yml | 558 | coverage job | Keep (after it blocks on PRs: REMOVE) |
| 10 | release-notes-check.yml | 20 | check-release-notes | Keep (documentation only) |
| 11 | ai-pr-review.yml | 33 | AI review | Keep (advisory) |
| 12 | ecosystem-ci.yml | 63 | assemblyscript-sdk | Keep (AS fragility) |

**5 safe to remove:** lint, test-python, test-java, build, and (conditionally) coverage.

## 4. Makefile coverage module prefix — CONFIRMED

Makefile line 158: `sub(/^github\.com\/rcownie\/cleat\//, "", path);`

`go.mod:1`: `module github.com/cleat-team/cleat`

The awk substitution uses the wrong prefix (`rcownie` instead of `cleat-team`). Package paths won't match, per-package thresholds won't apply, and coverage check likely reports all packages as untracked. This is a real bug.

## 5. Branch protection

Cannot verify (requires GitHub admin API access). The STATUS.md descriptions of main (3 required checks) and develop (0 required checks) are taken as accurate.

## 6. Risks

1. **golangci-lint re-enable introduces new lint failures.** Migrating from v1 (if a config exists) to v2 changes config format. Re-enabling will likely produce new warnings on existing code that need to be addressed before the job can be required.

2. **go.mod bump from 1.25.7 to 1.26 may break downstream consumers** who compile with Go 1.25. The `go` directive is a minimum version declaration.

3. **Removing continue-on-error from test-python/test-java will surface latent failures.** If those suites have been silently failing (or flaky), making them blocking will stall PRs. These should be verified green before removing the soft-fail.

4. **Coverage threshold on PRs will cause friction.** Adding a 50% global threshold to PRs when current coverage may be below that in some packages will immediately block merges. The ratchet approach needs a baseline first.

## 7. Complexity assessment

**LEAF-READY.** All findings are concrete and well-specified. No architectural unknowns remain. The work splits cleanly into independent file edits:

- 2 test fixes (single file, 2 line changes)
- CI configuration changes (multiple workflow files, no code logic changes)
- Makefile fix (1 line)
- Branch protection (documentation + GitHub settings, no code)

## 8. Recommendation

Mark cleat-234 as ready for implementation. The exploration is thorough and verified.

The dependency gates (cleat-232 multi-DB green, cleat-233 SDK green) should still be checked before implementation starts — the CONTRACT.md explicitly requires them. However, the closure test fixes and ci.yml configuration changes are independent and could proceed immediately.

### New finding to fold into implementation

When re-enabling golangci-lint, use the install-from-source approach:

```yaml
- name: Install golangci-lint
  run: GOTOOLCHAIN=go1.26.1 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1
- name: Run golangci-lint
  run: golangci-lint run --timeout=5m ./...
```

And create a `.golangci.yml` with `version: "2"` if one doesn't exist.
