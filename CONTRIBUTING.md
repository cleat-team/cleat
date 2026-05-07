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

Include the following statement in your first pull request description:

```
I hereby agree to the terms of the cleat Contributor License Agreement
(docs/cla.md).

Signed-off-by: Your Name <your.email@example.com>
```

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

| Tool | Version | Required | Notes |
|------|---------|----------|-------|
| **Go** | 1.26+ | Yes | See `go.mod` |
| **PostgreSQL** | 14+ (16 recommended) | Yes | For worker daemon and workflow storage |
| **TinyGo** | Latest | No | Smaller WASM binaries via `--target tinygo` |
| **Rust toolchain** | Stable | No | For `cleat-macro` / `cleat-sdk` crates and Rust workflows |
| **Node.js** | 20+ | No | For Svelte web UI and AssemblyScript SDK |
| **Java** | 17+ | No | For Java SDK |
| **Docker** | Latest | No | For cluster integration tests |

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

6. **CI must pass.** All tests and linters must pass before a PR can be merged.

## Build from source

```bash
# Clone the repo
git clone https://github.com/rcownie/cleat.git
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
DURABLE_TEST_DB=postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable \
  go test -count=1 -timeout=120s ./internal/host/...

# Rust crates
cd crates/cleat-macro && cargo test
cd crates/cleat-sdk && cargo test

# AssemblyScript SDK
cd packages/cleat-as && npm test
```

> **Note:** The `DURABLE_TEST_DB` environment variable is used by cluster
> integration tests. See `docker-compose.cluster.yml` for the default Postgres
> configuration.

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
# Default: compile with standard Go toolchain (wasip1/wasm target)
cleat build -o ./out ./path/to/workflow/package

# Smaller binaries with TinyGo
cleat build --target tinygo -o ./out ./path/to/workflow/package
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

1. Open a pull request against the `main` branch. Keep PRs small and focused
   (one PR, one concern).
2. CI automatically runs linting, tests (Go, Rust, AssemblyScript, Java,
   Python), and benchmarks. All checks must pass before merge.
3. A **Developer Certificate of Origin (DCO)** check verifies every commit
   includes a `Signed-off-by` line. See the DCO section above for setup.
4. All PRs require review from the project owner or a designated maintainer.
5. Once approved and passing CI, the PR is squashed and merged into `main`.

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
[GitHub issue tracker](https://github.com/rcownie/cleat/issues). The project
uses these labels:

- `good-first-issue` — Well-scoped tasks with clear acceptance criteria,
  suitable for newcomers to the codebase.
- `help-wanted` — Contributions wanted but may require more context.
- `area/sdk` — SDK-specific issues (Go, Rust, AssemblyScript, Java, Python).
- `area/wasm` — WASM compilation, transformer pipeline.
- `area/worker` — Worker daemon, polling, execution loop.
- `area/ui` — Svelte web UI.
- `area/docs` — Documentation improvements.

Check the [Discussions](https://github.com/rcownie/cleat/discussions) page for
RFCs and design proposals that need implementation.

## Questions?

Open a GitHub issue or reach out on Discord (link coming soon).
