# Plan: Get All CI Checks Green

## Status

Go CI is fully green (tests, vet, fuzz). 15 checks remain failing.

---

## Locally-testable (11 failures across 5 jobs)

### 1. Java Tests (5 assertion failures)

All 5 are pre-existing bugs that predate Stream G/H/I.

**1a. `testDecodeCallResultMaxValues`** — FIXED locally.
Signed `int` returned where test expected unsigned `long`. Fixed with `Integer.toUnsignedLong()`.

**1b. `testDecodePollSignalFound`** — `MemoryTest.java:145`
Expected `true` but got `false`. The `decodePollSignal()` method returns wrong value. Fix: inspect the bit-packing logic in `Memory.decodePollSignal()`. The poll signal status bit may be masked/shifted incorrectly.

**1c. `testNotEquals`** — `CleatResultTest.java:74`
`CleatResult.equals()` returns true when comparing objects with error code 120 that should not be equal. Fix: inspect `CleatResult.equals()` and `hashCode()` — likely missing an `errorCode` field in comparison.

**1d. `testScopePrefixesStateKeys`** — `TestHostCallsTest.java:651`
Scope prefixing for state keys not working — `hasState('status')` returns true when it should be prefixed. Fix: inspect `TestHostCalls` state key handling. The `setScope()` method may not be correctly prefixing subsequent state operations.

**1e. `testAnnotationOnFieldProducesError`** — `CleatEntryProcessorTest.java:601`
Java compiler changed error message format. Update assertion to accept new message "annotation interface not applicable to this kind of declaration" instead of checking for "method".

**1f. `testProcessorDoesNotGenerateFilesWithoutCleatEntry`** — `CleatEntryProcessorTest.java:739`
Expected `CleatEntryIndex` to be generated, but empty file list. The annotation processor may have changed behavior. Fix: update test expectation or fix processor to generate the index file.

**Environment:** Java 21 + Gradle 8.13, run `gradle -p crates/cleat-java test`

### 2. Lint (go vet ✅, ruff ⚠️, shellcheck ⚠️, clippy ⚠️)

- `go vet` — already passes ✅
- `ruff check python-sdk/` — 336 style errors. Solution: `ruff check --fix python-sdk/` then `ruff format python-sdk/`
- `shellcheck` — pre-existing script issues. Already non-blocking.
- `clippy` — Rust lint. Run `cargo clippy` in `crates/cleat-macro` and `crates/cleat-sdk` locally.

**Environment:** Need `pip install ruff`, `apt install shellcheck`, `rustup component add clippy`

### 3. Build (Java part)

Same Java test failures. Also builds AssemblyScript (`npm run build` in `packages/cleat-as`). The AS build step may succeed locally — the CI failure was Gradle test failures cascading the Build job.

### 4. AssemblyScript WASM Build

`npm ci` in `examples/as-workflow` fails with missing `@cleat/sdk` from lock file. Fix: run `npm install` (not `npm ci`) in CI, or regenerate lock file and commit. Local fix: `cd examples/as-workflow && rm -rf node_modules && npm install`.

### 5. Coverage

`make coverage-check` — installs Python SDK then runs Go coverage. The Python SDK install (`pip install` from `python-sdk/`) fails due to `pyproject.toml` issues (already partially fixed). May need further pip compatibility updates.

**Environment:** `pip install -e python-sdk/`

---

## Not locally-testable (8 failures across 5 jobs)

### 6. Python Tests (3.10/3.11/3.12)

`componentize-py 0.23.0` changed CLI: `-d/-w` flags work, but `componentize <APP_NAME>` now expects a Python module name, not a file path. The test passes `--entry /path/to/workflow.py:func_name` to `build_wasm.py`, which passes the file path to `componentize-py`. Fix: update `build_wasm.py` to extract the module path from the file path (strip `.py`, convert `/` to `.`, set up `sys.path`).

**Environment:** Needs `pip install componentize-py>=0.12.0` — system pip restricted locally.

### 7. Test Go with TinyGo

Dedicated TinyGo install step uses wrong download URL format (underscores vs dots). Fixed in latest commit (6545cb5) but previous CI run used old URL. The fix should take effect in next run.

**Environment:** GitHub API + curl to download TinyGo — CI-only.

### 8. Multi-DB (MySQL, SQL Server, Plugin Migrations)

Service container connection refused. MySQL health check fixed (added `-u root -pcleat`), MSSQL health check fixed (was using `mysqladmin` — copy-paste bug). Need to verify containers start correctly in GitHub Actions environment.

**Environment:** Docker service containers — CI-only.

### 9. DCO (Developer Certificate of Origin)

Requires `Signed-off-by:` in every commit message, or an organization-level exemption. Current commits use `Co-Authored-By: Claude Opus 4.7`. Fix options:
- Add `Signed-off-by: rcownie <rcownie@users.noreply.github.com>` to each commit (requires rebase)
- Disable DCO check for this repo
- Modify DCO check to accept PR body Signed-off-by (currently only checks commits)

**Environment:** GitHub Actions — CI-only.

### 10. Semantic PR / Labeler

PR title must match conventional commit format. Current title: `fix: final hostile review fixes — Streams G, H, I`. This is too long (>72 chars). Also needs the labeler config to exist.

**Environment:** GitHub Actions — CI-only.

---

## Execution order

### Phase 1: Locally-testable fixes (do now)
1. Complete Java test fixes (1a-1f above)
2. Fix AS WASM Build lock file + change CI to use `npm install`
3. Run `ruff check --fix` + `ruff format` on python-sdk
4. Run `cargo clippy --fix` on Rust crates

### Phase 2: Push and verify in CI
5. Push Phase 1 fixes, check CI results
6. Fix TinyGo URL (already done, verify)
7. Fix Multi-DB service containers if still failing

### Phase 3: Remotely-testable fixes (in CI)
8. Fix Python componentize-py module resolution
9. Fix Coverage (depends on Python fix)
10. Fix DCO (add Signed-off-by to commits via rebase)

### Phase 4: Policy checks
11. Fix PR title length
12. Fix semantic PR format
13. Fix labeler config
