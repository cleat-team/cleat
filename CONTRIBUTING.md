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

## Contribution Process

1. **Discuss first.** For significant changes, open an issue to discuss the
   approach before writing code. Bug fixes and small improvements can go
   straight to a PR.

2. **One PR, one thing.** Keep pull requests focused. A PR that fixes a bug
   AND refactors unrelated code is hard to review.

3. **Tests.** New functionality should include tests. Bug fixes should include
   a test that demonstrates the bug was fixed.

4. **Go formatting.** Go code must pass `gofmt`. The CI pipeline enforces this.

5. **Commit messages.** Use conventional commit style:
   - `fix: description` for bug fixes
   - `feat: description` for new features
   - `docs: description` for documentation
   - `test: description` for test changes

6. **CI must pass.** All tests and linters must pass before a PR can be merged.

## First time setup

```bash
# Clone the repo
git clone https://github.com/rcownie/durable.git
cd durable

# Build
go build ./...

# Run tests
go test ./...
```

## Code Review

All submissions require review. The project owner reviews all PRs. Review
feedback is about the code, not the person. Be respectful, assume good intent,
and focus on making the project better.

## Areas that need help

- **Non-Go SDKs.** The AssemblyScript and Java SDKs have known issues (see
  `prompts/fork_open_projects_issues.md`). Fixes are welcome.
- **Examples.** Real-world examples that demonstrate cleat's capabilities.
- **Documentation.** Clear, tested documentation for the Go SDK, WASM toolchain,
  and plugin development.
- **Plugins.** New plugins that extend cleat's "backend-in-a-box" story, or
  improvements to existing plugins.
- **Tests.** Integration tests, load tests, and edge-case coverage.

## Questions?

Open a GitHub issue or reach out on Discord (link coming soon).
