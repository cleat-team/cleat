# Cleat Public Launch Polish — Implementation Plan

**Date:** 2026-05-07
**Based on:** Research from 20 top OSS projects (`oss-best-practices-review.md`)

## Context

Before taking cleat public, we benchmarked it against 20 highly-rated open-source projects (Temporal, Caddy, Prometheus, Kubernetes, Astro, Vite, etc.) to identify best practices in README quality, community health files, CI/CD, documentation, and governance. The full research report is at `plans/oss-best-practices-review.md`.

**Significant work has already been done.** This plan documents what's complete and what remains.

---

## What's Already Done (23 items — see the corrections below)

> **Audited 2026-08-07.** Four rows in this table were wrong. Two claimed workflows that do not
> exist (CodeQL, semantic PR title), one counted badges the README does not carry, and two more
> describe checks since deleted for never having gated anything (#366, #373). Each is marked in
> place rather than silently edited, because a table that quietly corrects itself teaches the next
> reader nothing. Status that CI does not check rots; see `tiers.yaml`'s header and
> `plans/ci-integrity-followup-plan.md` §8.
>
> **Updated later the same day.** The CodeQL row is now genuinely Done — see the row itself for
> what was enabled and what it immediately found. The audit missed a fifth wrong claim, in
> "Remaining Gaps" rather than in this table: gap 1 asks for a placeholder to be fixed in an
> OpenSSF badge the README does not contain. Corrected there.

| Category | Item | Status |
|----------|------|--------|
| **README** | Badges | **Corrected 2026-08-07** — README carries 6 (`grep -c '^\[!\[' README.md`): CI, Go version, license, Go Report Card, Discord, Go Reference. Codecov, OpenSSF Scorecard and govulncheck badges are not present. |
| **README** | One-line value proposition ("Durable workflow engine on PostgreSQL...") | Done |
| **README** | Quick start with brew install + docker compose commands | Done |
| **README** | "When NOT to use cleat" section | Done |
| **README** | Architecture ASCII diagram, full API reference, CLI reference, DB schema | Done |
| **Community** | SECURITY.md (231 lines — supported versions, scope, threat model, AI disclosure, disclosure timeline) | Done |
| **Community** | CODE_OF_CONDUCT.md (156 lines — Contributor Covenant 2.1 + 4-level enforcement ladder) | Done |
| **Community** | GOVERNANCE.md (442 lines — roles, decision-making, TSC, meeting cadence, removal policy, conflict resolution) | Done |
| **Community** | PRINCIPLES.md (230 lines — 8 design principles) | Done |
| **Community** | CONTRIBUTING.md (366 lines, 17 sections — up from 109 lines) | Done |
| **Community** | CODEOWNERS (37 lines, all paths mapped) | Done |
| **Community** | FUNDING.yml (GitHub Sponsors) | Done |
| **Templates** | Issue templates: bug_report.yml, feature_request.yml, config.yml | Done |
| **Templates** | PR template with change-type checklist, release notes, AI disclosure, breaking change section | Done |
| **CI/CD** | Main CI (ci.yml) — lint, golangci-lint, govulncheck, test matrix, fuzz, benchmarks, build, cluster tests | Done |
| **CI/CD** | CodeQL scanning | **Done 2026-08-07, but not as this row described it.** There is still no `codeql.yml` and there will not be — this is CodeQL *default setup*, which is one fewer hand-written workflow to rot, and this repo's evidence on hand-written workflows is not encouraging. Enabled for `actions`, `go`, `javascript-typescript` and `python` (the API reports six, expanding `javascript-typescript` into its two aliases). Deliberately **not** `c-cpp`, `java-kotlin` or `rust`: those need autobuild, the `C` GitHub reports is vendored, and a scanner that fails to build is a scanner that reports nothing. Re-derive: `gh api repos/cleat-team/cleat/code-scanning/default-setup --jq '{state,languages}'`. |
| **CI/CD** | Secret scanning + push protection | **Done 2026-08-07.** Both enabled, plus non-provider patterns. `gh api repos/cleat-team/cleat --jq .security_and_analysis` |
| **CI/CD** | Dependabot security updates | **Done 2026-08-07, and it is not cosmetic.** Enabling alerts surfaced **68 existing vulnerabilities — 16 critical, 25 high, 26 medium, 1 low** — on the default branch, and opened 8 update PRs. That backlog was always there; nothing was reporting it. Triage it before making any of this a required check: requiring it first blocks every PR on unrelated findings, which is how a security gate gets switched off. `gh api repos/cleat-team/cleat/dependabot/alerts --jq 'map(select(.state=="open"))\|length'` |
| **CI/CD** | DCO check (dco-check.yml) | Done |
| **CI/CD** | Semantic PR title check (semantic-pull-request.yml) | **NOT DONE, and decided 2026-08-07 not to do it.** No such workflow ever existed. `Validate branch name` already enforces a prefix vocabulary (`feature/`, `bugfix/`, `fix/`, `docs/`, `release/`, `hotfix/`) and is a required check, so a title check would be a second guard over the same convention. The repo's problem has never been unparseable PR titles; it has been checks that pass without measuring anything. Adding a third overlapping guard is not the answer to that. Row kept rather than deleted so the decision is visible. |
| **CI/CD** | Auto-labeler (labeler.yml + labeler.yml config) | Removed 2026-08-06 (#366) — v4 config under a v5 action; never applied a label |
| **CI/CD** | Release notes check (release-notes-check.yml) | Removed 2026-08-07 — keyed on labels nothing could apply; never gated a PR (IMPROVEMENT-PLAN §1.12a) |
| **CI/CD** | Ecosystem CI (ecosystem-ci.yml — tests Python/Rust/Java/AS SDKs on core changes) | Done |
| **CI/CD** | Stale issue/PR management (stale.yml) | Done |
| **CI/CD** | GoReleaser-based release workflow (release.yml + .goreleaser.yml) | Done |
| **CI/CD** | Renovate + Dependabot configured | Done |
| **DevEx** | Dev container with PostgreSQL (`.devcontainer/`) | Done |
| **Docs** | CHANGELOG.md (Keep a Changelog format) | Done |

---

## Remaining Gaps (7 items)

### 1. ~~Fix OpenSSF Best Practices Badge (placeholder URL)~~ — corrected 2026-08-07

**This gap described a badge the README does not contain.** Both sentences of it were wrong:

```
$ grep -n '^\[!\[' README.md
3: CI   4: Go Version   5: License   6: Go Report Card   7: Discord   8: Go Reference
$ grep -c -i 'bestpractices\|scorecard\|XXXX' README.md
0
```

There is no OpenSSF Best Practices badge with a placeholder to fix, and the claim that "the OpenSSF
Scorecard badge is already present and working" is false — that badge is not present either, which
the badge row of the table above already recorded. The two statements contradicted each other
inside one document.

**What is actually open**, stated as a choice rather than a fix: decide whether cleat wants an
OpenSSF presence at all. Registering at https://www.bestpractices.dev/ earns a real badge; running
the Scorecard action earns another. Both are worth having, and neither is a five-minute edit — the
entry in the effort table below said 5 min because it assumed a URL typo. Until one is chosen,
there is nothing here to fix.

### 2. Create `SUPPORT.md`

**File:** `SUPPORT.md` (new, repo root)
**Model:** Astro + Kubernetes

Content outline (~30 lines):
- GitHub Issues for bug reports and feature requests (link to templates)
- GitHub Discussions for questions and ideas
- Discord for live chat and community
- SECURITY.md for vulnerability disclosure (do not open a public issue)
- Commercial support: not yet available
- Stack Overflow tag suggestion (once there's enough community)

Most of this information is already in the issue template `config.yml` contact links, but a standalone SUPPORT.md is expected by GitHub's community profile checks and by contributors who look for it.

### 3. Create `MAINTAINERS.md`

**File:** `MAINTAINERS.md` (new, repo root)

GOVERNANCE.md (line 428) explicitly references this file: "The MAINTAINERS.md file will list the current project maintainers and their areas of responsibility." The file needs to exist so this cross-reference isn't broken.

Content:
```markdown
# Maintainers

- **Richard Cownie** ([@rcownie](https://github.com/rcownie)) — Project Lead
  - All areas: engine, CLI, SDK, WASM compiler, web UI, docs, CI/CD

## Becoming a Maintainer

See [GOVERNANCE.md](./GOVERNANCE.md) for the full contributor pathway and maintainer nomination process.
```

### 4. Expand Dependabot to Cover Package Ecosystems

**File:** `.github/dependabot.yml` (edit)

Currently Dependabot only covers GitHub Actions. Renovate handles more but has limits on PR rate (5/hour). Adding Dependabot for the other ecosystems provides defense-in-depth:

```yaml
# Add these sections:
- package-ecosystem: "gomod"
  directory: "/"
  schedule: { interval: "weekly", day: "monday" }

- package-ecosystem: "npm"
  directory: "/web"
  schedule: { interval: "weekly" }

- package-ecosystem: "pip"
  directory: "/python-sdk"
  schedule: { interval: "weekly" }

- package-ecosystem: "cargo"
  directory: "/crates/durable-sdk"
  schedule: { interval: "weekly" }
```

### 5. Add Release Target to Makefile

**File:** `Makefile` (edit)

Add a `release-dry-run` target so contributors can test GoReleaser locally before pushing a tag:

```makefile
.PHONY: release-dry-run
release-dry-run:
	goreleaser release --snapshot --clean --skip=publish
```

### 6. Logo / Branding

**Status:** None exists.

This is the single biggest visual gap. Every top project has a logo that appears in the README header. Recommendations:

- Commission or generate a simple SVG logo for cleat
- Place at `web/public/logo.svg` or `assets/logo.svg`
- Add centered logo to README above the badge bar (Next.js/React pattern)
- Use theme-aware rendering if the logo has dark/light variants (`#gh-dark-mode-only` / `#gh-light-mode-only`)

This is the one item that can't be done purely in code — it requires design work. It could be deferred to post-launch if a logo isn't ready, but it has high visual impact for first impressions.

### 7. Verify / Create Dockerfile

**Issue:** The CI workflow (`ci.yml`) runs `docker build -t cleat-worker:latest .` which requires a `Dockerfile` at the repo root. No `Dockerfile` exists at HEAD.

**Fix:** Either:
- The `Dockerfile` was accidentally not committed (check git history)
- It needs to be created (a minimal multi-stage Go build Dockerfile)
- The CI job needs to be fixed if the Dockerfile is generated by a prior build step

This is a correctness issue, not just polish — CI will fail without it.

---

## Implementation Sequence

All 7 items are independent and can be done in any order. Recommended order by impact:

| Order | Item | Effort | Impact |
|-------|------|--------|--------|
| 1 | Verify/create Dockerfile | 15 min | Blocks CI |
| 2 | Create MAINTAINERS.md | 5 min | Fixes broken cross-reference in GOVERNANCE.md |
| 3 | Create SUPPORT.md | 10 min | GitHub community profile completeness |
| 4 | ~~Fix OpenSSF badge placeholder~~ — **void, corrected 2026-08-07.** No such badge exists; see gap 1. What remains is a decision about whether to seek an OpenSSF presence, not a 5-minute edit | — | — |
| 5 | Expand Dependabot config | 10 min | Dependency hygiene |
| 6 | Add release-dry-run to Makefile | 5 min | Contributor convenience |
| 7 | Logo/branding | Variable | High visual impact, may need design time |

---

## What Cleat Already Excels At

The audit confirms cleat is already well above the median of the 20 reviewed projects:

- **README:** 821 lines, **6** badges (`grep -c '^\[!\[' README.md`, 2026-08-07 — this line said 12 until it was checked), architecture diagram, full API reference, CLI reference, DB schema, testing guide, honest limitations section — better than 80% of projects reviewed
- **CONTRIBUTING.md:** 366 lines, 17 sections — better than Rust, Tokio, and Ripgrep; competitive with Temporal and Vite
- **SECURITY.md:** 231 lines with threat model — better than all but Caddy and Vite
- **GOVERNANCE.md:** 442 lines with TSC, meeting cadence, removal policy — better than 17/20 projects
- **CI/CD:** 14 workflows (`ls .github/workflows/ | wc -l`, 2026-08-07), including DCO, CLA, ecosystem CI, the tier 1 and tier 2 gates, cross-language E2E and the plugin harness. **Corrected 2026-08-07:** of the five named here previously, CodeQL and the semantic-PR check never existed, the release-notes check never gated a PR (deleted, #373), and the auto-labeler never applied a label (deleted, #366). 32 of these jobs are required status checks with `enforce_admins: true` — a stronger claim than the workflow count, and a checkable one. **CodeQL now runs**, as default setup rather than a workflow, so it does not appear in that count at all.
- **PRINCIPLES.md:** 8 design principles — only Docker/Moby had this among 20 projects

The remaining 7 items are small polish tasks. The repo is substantially public-ready.

---

## Verification Checklist

- [ ] `Dockerfile` exists and `docker build -t cleat-worker:latest .` succeeds
- [ ] `MAINTAINERS.md` exists and GOVERNANCE.md's reference to it resolves
- [ ] `SUPPORT.md` exists and links to Discord, Issues, Discussions, SECURITY.md
- [ ] Decide whether cleat wants an OpenSSF presence (Best Practices registration, Scorecard action, or neither) — there is no such badge in the README today, so there is nothing to fix, only something to choose
- [ ] Triage the 68 Dependabot alerts surfaced on 2026-08-07, and the first CodeQL analysis, before making either a required check
- [ ] Dependabot covers Go, npm, pip, and Cargo in addition to Actions
- [ ] `make release-dry-run` runs GoReleaser in snapshot mode
- [ ] `make lint && make test` passes
- [ ] GitHub community profile shows all checks green (100%)
