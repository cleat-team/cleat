# Contributing to cleat

Thank you for your interest in contributing. This document explains the process
and requirements.

## License

cleat is licensed under the [Apache License 2.0](LICENSE). By contributing to
this project, you agree that your contributions will be licensed under the same
terms.

## Contributor License Agreement (CLA)

**Before we can accept your first contribution, you must sign the cleat
Contributor License Agreement.** The CLA is documented in [docs/cla.md](docs/cla.md).

The CLA gives the project owner the right to re-license the project in the
future (for example, to adopt a stricter license if a managed cloud offering is
launched). It does NOT transfer copyright — you retain full ownership of your
code. It simply grants the project permission to sublicense your contributions
under different terms if needed.

### How to sign (individuals)

Open your pull request as normal. If you have not signed, the CLA Assistant
bot comments on it with a link to the agreement, and the `CLA Assistant` check
stays red until you reply on the pull request with exactly:

```
I have read the cleat Contributor License Agreement and I hereby sign the CLA
```

Your signature is then recorded in `signatures/version1/cla.json` on the
`cla-signatures` branch, against your GitHub username and the pull request you
signed on. You sign once; later pull requests are checked against that file.
If the check is stale for any reason, comment `recheck`.

> Until 2026-08-07 signing meant pasting a sentence into the pull request
> description. That established nothing — a description is editable after the
> fact and is not carried into the repository by a squash merge, so there was
> no record of who had agreed. The signature file is that record.

### Corporate contributors

If you are contributing as part of your employment, your employer must sign a
Corporate CLA. Contact the project owner to request one.

## Developer Certificate of Origin

In addition to the CLA, all commits must include a `Signed-off-by` line
certifying the [Developer Certificate of Origin](https://developercertificate.org/)
(DCO). You can add this automatically with:

```
git commit --signoff
```

The DCO confirms you have the right to submit the contribution under the
project's license. Unlike the CLA, it does not grant re-licensing rights —
that's what the CLA is for.

## Prerequisites

To build and test cleat you will need:

**Minimum (write and run Go workflows):**
| Tool | Version | Required | Notes |
|------|---------|----------|-------|
| Go | 1.25+ | Yes | Standard Go toolchain |
| PostgreSQL | 14+ (16 recommended) | Yes | Or MySQL 8.0+, or SQL Server 2017+ |
| Docker | Latest | No | Only if using Docker for the database |

**Full (develop cleat itself):**
| Tool | Version | Required | Notes |
|------|---------|----------|-------|
| Everything above | | Yes | |
| Rust toolchain | Stable | No | For `cleat-macro` / `cleat-sdk` crates and Rust workflows |
| Python 3 | 3.10+ | No | For Python SDK |
| Java | 17+ | No | For Java SDK |
| Node.js | 20+ | No | For Svelte web UI and AssemblyScript SDK |
| MySQL | 8.0+ (Docker) | No | For MySQL backend integration tests |
| SQL Server | 2017+ (Docker) | No | For MSSQL backend integration tests |
| Docker | Latest | No | For cluster integration tests |

## Branch naming

cleat uses a gitflow-derived branch naming convention. All branches must follow
one of these prefixes:

| Prefix | Purpose | Example |
|--------|---------|---------|
| `feature/` | New functionality | `feature/multi-db-support` |
| `bugfix/` | Bug fixes | `bugfix/claim-race-condition` |
| `fix/` | CI, config, and tooling fixes | `fix/ci-token` |
| `docs/` | Documentation only | `docs/branch-conventions` |
| `release/` | Release preparation | `release/v1.2.0` |
| `hotfix/` | Critical production fixes | `hotfix/worker-panic-on-nil-input` |

Branch names must be lowercase, use hyphens (not underscores), and be concise
but descriptive. The prefix must be one of those listed above.

A CI check validates branch naming on pull requests. Branches opened by
dependabot or with the `bot` label are exempt.

## Contribution Process

1. **Discuss first.** For significant changes, open an issue to discuss the
   approach before writing code. Bug fixes and small improvements can go
   straight to a PR.

2. **One PR, one thing.** Keep pull requests focused. A PR that fixes a bug
   AND refactors unrelated code is hard to review.

3. **Tests.** New functionality should include tests. Bug fixes should include
   a test that demonstrates the bug was fixed.

4. **Go formatting.** Go code must pass `gofmt`. The CI pipeline enforces this.

5. **PR titles (conventional commits).** All pull request titles must follow the
   Angular-inspired conventional commit format. The CI pipeline enforces this
   via the `Semantic Pull Request` workflow.
   
   **Format:** `type(scope): description` or `type: description`
   
   Valid types:
   - `feat(scope): description` — new feature (scope expected)
   - `fix(scope): description` — bug fix (scope expected)
   - `docs(scope): description` — documentation (scope optional)
   - `test(scope): description` — tests (scope expected)
   - `refactor(scope): description` — refactoring (scope expected)
   - `chore(scope): description` — maintenance (scope optional)
   - `ci(scope): description` — CI changes (scope optional)
   - `perf(scope): description` — performance improvements (scope expected)
   
   Types that expect a scope (`feat`, `fix`, `refactor`, `test`, `perf`) should
   always include one. Types where scope is optional (`docs`, `chore`, `ci`) may
   omit it.
   
   Valid scopes: `engine`, `wasm`, `cli`, `worker`, `sdk`, `ui`, `plugins`,
   `docs`, `ci`, `deps`, `build`

6. **CI must pass.** All tests, linters, and checks must pass before a PR can
   be merged. This includes DCO, semantic PR title, branch naming, CLA check
   (for first-time contributors), and the AI review gates.

## Quick setup

For a one-command toolchain check:

```bash
make setup        # minimum: Go + PostgreSQL (for workflow authors)
make setup-full   # everything: all languages + databases (for engine contributors)
```

Or open this repo in VS Code with the [Dev Containers](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers) extension — the `.devcontainer/` configuration provisions a complete environment automatically.

## Build from source

```bash
# Clone the repo
git clone https://github.com/cleat-team/cleat.git
cd cleat

# Build all Go packages
go build ./...

# Build CLI binaries (cleat, cleat-worker, cleat-gen)
go build ./cmd/...

# Build Rust crates (optional, for Rust workflow support)
cd crates/cleat-macro && cargo build && cd ../..
cd crates/cleat-sdk && cargo build && cd ../..

# Build AssemblyScript SDK (optional)
cd packages/cleat-as && npm ci && npm run build && cd ../..

# Build the Svelte web UI (optional, embeds into cleat-worker)
cd web && npm ci && npm run build && cd ..

# Install CLI tools
go install ./cmd/cleat
go install ./cmd/cleat-worker
go install ./cmd/cleat-gen
```

## Test commands by category

```bash
# Unit tests (no database required)
go test -short ./...

# All tests (including integration tests that need a database)
go test -count=1 ./...

# Tests for a specific package
go test -count=1 -v ./internal/transform/...

# Cluster integration tests (requires Docker)
# Starts a PostgreSQL cluster via docker-compose, then runs tests
CLEAT_TEST_DB=postgres://cleat:cleat@127.0.0.1:5432/cleat?sslmode=disable \
  go test -count=1 -timeout=120s ./engine/...

# MySQL backend tests (requires MySQL 8.0+ at localhost:3306)
# Skipped if CLEAT_TEST_MYSQL is not set
CLEAT_TEST_MYSQL=root:cleat@tcp(localhost:3306)/cleat \
  go test -count=1 -timeout=120s ./engine/...

# SQL Server backend tests (requires SQL Server 2017+ at localhost:1433)
# Skipped if CLEAT_TEST_MSSQL is not set
CLEAT_TEST_MSSQL=sqlserver://sa:CleatTest123!@localhost:1433?database=master \
  go test -count=1 -timeout=120s ./engine/...

# Rust crates
cd crates/cleat-macro && cargo test
cd crates/cleat-sdk && cargo test

# AssemblyScript SDK
cd packages/cleat-as && npm test
```

> **Note:** The `CLEAT_TEST_DB` environment variable is used by cluster
> integration tests. See `docker-compose.cluster.yml` for the default Postgres,
> MySQL, and SQL Server configurations. The compose file defines all three
> database services for local multi-backend development.

## Svelte UI dev setup

The web UI is a Svelte 5 application located in the `web/` directory. It
embeds into the `cleat-worker` binary at build time.

```bash
cd web
npm install
npm run dev
```

The dev server proxies API requests to the worker (`http://localhost:5173`
defaults to connecting to `http://localhost:8080`). Start the worker with the
`--api-addr` flag to serve the API:

```bash
cleat-worker --db "postgres://user:pass@localhost/cleat?sslmode=disable" \
    --api-addr :8080
```

To build the UI for production (output goes to `cmd/cleat-worker/web/dist/`):

```bash
cd web
npm run build
```

## WASM build instructions

Workflows are compiled to WebAssembly using the `cleat build` command.

### Go WASM

```bash
# Compile with the standard Go toolchain (the only Go WASM target)
cleat build -o ./out ./path/to/workflow/package
```

The pipeline: analyzer.Load -> callgraph.Build -> closure.Compute -> transform ->
wasm.Compile. The output is a `.wasm` file ready for deployment.

### Rust WASM

```bash
cleat build --target rust -o ./out ./examples/rust-workflow/
```

Requires the `cleat-macro` and `cleat-sdk` crates (see `crates/`). Rust
workflows use the `#[cleat_entry]` proc-macro attribute.

### Deploy

```bash
cleat deploy --db "postgres://user:pass@localhost/cleat?sslmode=disable" \
    --name my_workflow ./out/my_workflow.wasm
```

## IDE debugging

The project does not ship VS Code launch configurations in the repository.
For debugging with VS Code + Delve, create a `.vscode/launch.json` in the
project root:

```json
{
    "version": "0.2.0",
    "configurations": [
        {
            "name": "Debug cleat-worker",
            "type": "go",
            "request": "launch",
            "mode": "exec",
            "program": "${workspaceFolder}/cmd/cleat-worker",
            "args": ["--db", "postgres://user:pass@localhost/cleat?sslmode=disable"]
        },
        {
            "name": "Debug cleat tests",
            "type": "go",
            "request": "launch",
            "mode": "test",
            "program": "${workspaceFolder}/internal/transform"
        }
    ]
}
```

For command-line debugging, use `dlv debug`:

```bash
dlv debug ./cmd/cleat-worker -- --db "postgres://..."

# Debug a specific test
dlv test ./internal/transform -- -test.run TestTransformOrder
```

## Code Review

All submissions require review. The project owner reviews all PRs. Review
feedback is about the code, not the person. Be respectful, assume good intent,
and focus on making the project better.

## PR process

All pull requests go through the same pipeline. The steps are sequential —
each gate must pass before the next begins.

### 1. Open the PR

Open a pull request against the `main` branch from a branch that follows the
[branch naming convention](#branch-naming). Use the PR template to describe
your change. If you have not signed the CLA, a bot will comment on the pull
request asking you to (see [How to sign](#how-to-sign-individuals)); there is
nothing to prepare in advance.

Keep PRs small and focused. A single concern, a single PR.

### 2. Automated checks

CI runs automatically on every push. The following checks must all pass:

| Check | What it verifies |
|-------|-----------------|
| DCO | Every commit has a `Signed-off-by` line |
| Semantic PR title | Title follows `type(scope): description` format |
| Branch naming | Branch name follows `feature/`, `bugfix/`, `fix/`, `docs/`, `release/`, or `hotfix/` prefix convention |
| CLA Assistant | The author has signed the ICLA, recorded in `signatures/version1/cla.json` on the `cla-signatures` branch |
| Lint | `go vet`, `golangci-lint`, `ruff`, `shellcheck`, `clippy` |
| Test | Go (1.22/1.23/1.24 matrix), Python (3.10–3.12), Java, Rust, AssemblyScript |
| Vulncheck | `govulncheck` for known vulnerabilities |
| CodeQL | CodeQL static analysis |
| Fuzz | Fuzz tests on the WASM runtime |
| Cross-language E2E | Multi-language WASM round-trip tests |
| Coverage | Go and Python coverage (informational, non-blocking) |

### 3. AI review

After automated checks pass, an AI code review is posted automatically. The
AI review is a first-pass filter — it catches obvious issues (missing error
handling, potential race conditions, API inconsistencies, test gaps) before a
human spends time on the PR.

The AI review is advisory, not blocking. It exists to save reviewer time, not
replace human judgment. Contributors may address AI feedback directly or
respond with a rationale for why the suggestion does not apply.

The PR template includes an [AI disclosure](#ai-disclosure) section. If AI/LLM
tools assisted in creating the PR, this must be disclosed so the reviewer can
calibrate accordingly.

### 4. Human review

A maintainer (or the project owner) reviews the PR. Review is about the code,
not the person. The reviewer considers:

- Correctness: does it do what it claims?
- Safety: are there security, race, or data-loss concerns?
- Fit: does it follow project conventions and design principles?
- Tests: are the right things tested?

The reviewer may approve, request changes, or comment with questions.

### 5. Merge

Once approved and all checks are green, the PR is **squash-merged** into
`main`. Squash is the only merge method — no merge commits, no rebase merges.
This keeps the main branch history linear and each commit atomic.

The squash commit message must retain the PR title as its subject line and
include any `Co-authored-by` trailers for contributors who participated.

## Release process overview

Releases are automated via GoReleaser. When a maintainer pushes a version tag
(e.g., `v0.5.0`) to the repository, the release workflow:

1. Builds release binaries for Linux (amd64, arm64), macOS (amd64, arm64), and
   Windows (amd64).
2. Publishes the `cleat`, `cleat-worker`, and `cleat-gen` binaries to the
   GitHub release page.
3. Publishes the `cleat-macro` and `cleat-sdk` crates to crates.io.
4. Builds and publishes the `cleat` Docker image to GitHub Container Registry.

Release candidates follow semver pre-release tags (e.g., `v0.5.0-rc.1`).

## Areas that need help

- **Non-Go SDKs.** The AssemblyScript and Java SDKs have known issues (see
  `prompts/fork_open_projects_issues.md`). Fixes are welcome.
- **Examples.** Real-world examples that demonstrate cleat's capabilities.
- **Documentation.** Clear, tested documentation for the Go SDK, WASM toolchain,
  and plugin development.
- **Plugins.** New plugins that extend cleat's "backend-in-a-box" story, or
  improvements to existing plugins.
- **Tests.** Integration tests, load tests, and edge-case coverage.

## Non-code contributions

Not a coder? There are many other valuable ways to contribute to cleat. These
are appreciated and valued equally with code contributions.

### Triage issues

Help manage the issue tracker by reproducing bugs, asking clarifying questions,
and confirming fixes. Good issue hygiene makes the project easier for everyone.

### Improve documentation

Fix typos, add examples, improve explanations, or translate documentation into
other languages. Clear documentation is a force multiplier for the community.

### Answer questions

Help others in GitHub Discussions and on Discord. Answering questions is one of
the most impactful ways to contribute — it helps people get unstuck and frees
up maintainers to focus on development.

### Write tutorials and blog posts

Share how you use cleat. Tutorials, blog posts, and video guides help new users
get started and showcase what the project can do.

### Give talks and workshops

Spread the word at meetups, conferences, and user groups. A talk about your
cleat experience can inspire others to try it.

### Design and UX

Improve the web UI, suggest workflow improvements, or contribute mockups and
design feedback. Good design makes cleat more accessible.

### Test releases

Run release candidates and report issues. Testing prereleases helps catch
regressions before they reach production users.

## Finding work

Good first issues are tagged `good-first-issue` and `help-wanted` in the
[GitHub issue tracker](https://github.com/cleat-team/cleat/issues). The project
uses these labels:

- `good-first-issue` — Well-scoped tasks with clear acceptance criteria,
  suitable for newcomers to the codebase.
- `help-wanted` — Contributions wanted but may require more context.
- `area/sdk` — SDK-specific issues (Go, Rust, AssemblyScript, Java, Python).
- `area/wasm` — WASM compilation, transformer pipeline.
- `area/worker` — Worker daemon, polling, execution loop.
- `area/ui` — Svelte web UI.
- `area/docs` — Documentation improvements.

Check the [Discussions](https://github.com/cleat-team/cleat/discussions) page for
RFCs and design proposals that need implementation.

## Documentation Standards

### SDK README template

All SDK READMEs should follow the template at
`docs/contributor/SDK_README_TEMPLATE.md`. The template covers:

1. Installation
2. Quick Start (minimal example)
3. HostCall API Reference (table format)
4. WASM Compilation
5. Constraints / Known Limitations
6. Testing Guide
7. Troubleshooting

See `python-sdk/README.md` for a complete worked example that follows the
template. When creating a new SDK or updating an existing one, use the template
as a starting point and fill in language-specific details.

### Diataxis documentation framework

The project follows the
[Diataxis](https://diataxis.fr/) framework for documentation:

- **Tutorials** — Learning-oriented, step-by-step guides (in `docs/tutorials/`)
- **How-to guides** — Goal-oriented recipes (in `docs/how-to/`)
- **Explanation** — Understanding-oriented background (in `docs/explanation/`)
- **Reference** — Information-oriented technical descriptions (in `docs/reference/`)

New documentation should be placed in the appropriate directory. If you are
unsure which category fits, ask in the PR review or open an issue for guidance.

### New SDKs must follow the template

Any new SDK added to the repository must include a README that follows the SDK
README template. The review gate for new SDK PRs includes a check that the
README covers all seven sections.

## Questions?

Open a GitHub issue or reach out on Discord (link coming soon).
