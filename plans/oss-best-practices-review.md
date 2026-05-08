# Open Source Best Practices Review for Cleat

**Date:** 2026-05-07
**Purpose:** Gather best practices from 20 top open-source projects to make cleat attractive from day one of going public.

## Methodology

Four subagents reviewed 5 projects each across Go, Rust, JS/framework, and infrastructure ecosystems. Each agent examined:
- README quality (badges, quick start, clarity, visual appeal)
- CONTRIBUTING.md coverage
- Documentation structure
- Issue/PR templates
- CI/CD setup
- Code of Conduct, Security policy, Governance
- License clarity
- Community health signals
- CHANGELOG / release notes approach
- Code organization patterns

## Projects Reviewed

| # | Project | Stars | Ecosystem | Key Strength |
|---|---------|-------|-----------|-------------|
| 1 | Temporal | 20.1k | Go | Gold standard quick start, 12-section CONTRIBUTING.md |
| 2 | Caddy | 72.2k | Go | Best-in-class SECURITY.md, 8 badges, extraordinary release notes |
| 3 | PocketBase | 58.2k | Go | 8-word value prop, GoReleaser + Svelte UI build |
| 4 | Dagger | 15.8k | Go | Dogfooding, Discord-first, Changie changelog |
| 5 | Prometheus | 63.9k | Go | 9 badges, YAML issue forms, release note CI check |
| 6 | Rust | 100k+ | Rust | 12 issue templates, external doc ecosystem, dual-licensing |
| 7 | Zed | 55k+ | Rust | 50-section CONTRIBUTING.md, mdBook docs, discussion templates |
| 8 | Biome | 20k+ | Rust | 589-line CONTRIBUTING.md, full GOVERNANCE.md, 23 CI workflows |
| 9 | Tokio | 28k+ | Rust | Perfect README quick-start, LTS policy, RustSec integration |
| 10 | Ripgrep | 50k+ | Rust | Performance benchmarks, honest limitations, minimalist excellence |
| 11 | React | 245k | JS | Tiered onboarding, 24 CI workflows, categorized CHANGELOG |
| 12 | Next.js | 139k | JS | Branded README, branch strategy, bug bounty program |
| 13 | Astro | 59.1k | JS | Gold-standard 746-line CONTRIBUTING.md, full governance model, Changesets |
| 14 | Svelte | 86.5k | JS | Non-code contribution paths, breaking change template, RFC process |
| 15 | Tailwind CSS | 94.9k | JS | Keep a Changelog format, test-first policy, CODEOWNERS |
| 16 | Kubernetes | 122k | Infra | Governance as infrastructure, modular contributor guide, per-version changelogs |
| 17 | Docker/Moby | 71.5k | Infra | PRINCIPLES.md, "Everything is a PR" governance, DCO enforcement |
| 18 | OpenTofu | 28.6k | Infra | Transparent fork governance, UPGRADE NOTES, SECURITY.md depth |
| 19 | Grafana | 73.6k | Infra | 85 CI workflows, dual-licensing with exceptions, Champions Program |
| 20 | Vite | 80.4k | Infra | Commit convention rigor, best-in-class CONTRIBUTING.md, threat model |

---

## Ecosystem Findings

### Go Ecosystem

#### Temporal (temporalio/temporal) — 20.1k stars, MIT
- **Gold standard quick start:** `brew install temporal && temporal server start-dev` — zero config, under 30 seconds
- **Exceptional CONTRIBUTING.md:** 12 sections, per-OS build prerequisites, 3 test categories with exact commands, IDE debugging instructions
- **19 CI workflow files:** govulncheck, flaky-test detection, test sharding optimization, stale issue management
- **Gaps:** No SECURITY.md, no CODE_OF_CONDUCT.md at standard path, plain Markdown (not YAML) issue templates

#### Caddy (caddyserver/caddy) — 72.2k stars, Apache-2.0
- **Best-in-class SECURITY.md:** supported versions, scope boundaries, mandatory AI/LLM disclosure, exact reproduction requirements
- **8 badges in README:** each tells a specific quality story (CI, GoDoc, Best Practices, etc.)
- **Single YAML issue form** rather than multiple templates — pragmatic and low-maintenance
- **Extraordinary release notes:** narrative highlights, CVE disclosures, full commit log, contributor avatar grid
- **Gaps:** No CONTRIBUTING.md at root, no CODE_OF_CONDUCT.md, docs live off-repo

#### PocketBase (pocketbase/pocketbase) — 58.2k stars, MIT
- **"Open Source realtime backend in 1 file"** — value proposition in 8 words
- **GoReleaser + embedded Svelte UI build** — directly applicable to cleat's architecture
- **Dual Go + Svelte contribution instructions** in single CONTRIBUTING.md
- **Gaps:** No CODE_OF_CONDUCT, no issue/PR templates, no community channels on GitHub, only 3 badges

#### Dagger (dagger/dagger) — 15.8k stars, Apache-2.0
- **Dogfooding:** uses its own product for CI/CD — ultimate confidence signal
- **Discord-first community:** joining Discord is step 1 in CONTRIBUTING.md
- **Changie for structured release notes:** every PR that needs a changelog entry gets one
- **Full Docusaurus docs site in-repo** — documentation versioned alongside code
- **Gaps:** No SECURITY.md, no issue/PR templates, visually sparse README

#### Prometheus (prometheus/prometheus) — 63.9k stars, Apache-2.0
- **9 badges covering every quality dimension:** CI, Docker, Go Report Card, CII Best Practices, OpenSSF Scorecard, govulncheck, fuzzing
- **YAML-based issue forms with required fields** — ensures complete bug reports
- **Release note verification as a CI check** — disciplined changelog process
- **PR template with change-type labels:** `[FEATURE]`, `[BUGFIX]`, `[SECURITY]`, etc.
- **Multi-layered security CI:** CodeQL, govulncheck, fuzzing, scorecards
- **Gaps:** Complex build (Go + Node.js), no "good first issue" onboarding path

### Rust Ecosystem

#### Rust (rust-lang/rust) — 100k+ stars, MIT + Apache-2.0
- **12 structured issue templates** with `config.yml` that redirects off-topic queries to external forums
- **External documentation ecosystem:** The Book, rustc-dev-guide, std-dev-guide — README delegates to these
- **Dual-licensing clarity:** MIT + Apache 2.0 explicitly stated with trademark policy
- **Governance transparency** through the Rust Foundation and Security Response WG
- **Gaps:** No badges in README, CONTRIBUTING.md is a 37-line redirect stub, no quick-start code on main page

#### Zed (zed-industries/zed) — 55k+ stars, GPL
- **Production-grade CONTRIBUTING.md (50+ sections):** explicit "will not merge" list, UI/UX quality checklist (120fps target, accessibility), crate-by-crate architecture tour, AI-assisted contribution policy
- **Rich mdBook documentation:** 14 major sections covering AI features, collaboration, customization, 70+ language support guides
- **Discussion templates** alongside issue templates — treats feature proposals as discussions
- **PR template with release notes field:** `Release Notes:` with `N/A or Added/Fixed/Improved` prefixes
- **Honest for-profit positioning:** upfront about being company-developed, transparent about sponsorship
- **Gaps:** No SECURITY.md, Code of Conduct is a 3-line stub, no project-wide CHANGELOG

#### Biome (biomejs/biome) — 20k+ stars, MIT
- **Exceptional CONTRIBUTING.md (589 lines):** AI disclosure with teeth, concrete test commands, changeset workflow, conventional commits, crate development guides, full release checklist
- **Full GOVERNANCE.md:** 4-tier hierarchy (Lead > Core > Maintainer > Contractor) with voting rules, quorum, lead veto
- **23 CI/CD workflows:** autofix, benchmarks, beta channels, label automation, parser conformance, PR checks
- **Contributor Covenant 2.1 with enforcement ladder:** 4-step process (Correction, Warning, Temporary Ban, Permanent Ban)
- **Gaps:** No `docs/` directory in repo, documentation lives on external website

#### Tokio (tokio-rs/tokio) — 28k+ stars, MIT
- **Perfect README quick-start:** complete, copy-pasteable 20-line TCP echo server that works immediately
- **LTS and versioning policy:** explicit LTS lines with end dates, MSRV declaration, monthly release cadence
- **Security policy with RustSec integration:** dedicated email, GitHub Security Advisories, RustSec advisory database
- **Contributing in layers:** short welcoming root doc (54 lines) + detailed 700-line sub-guide
- **Gaps:** Root CONTRIBUTING.md is minimal, only 2 issue templates

#### Ripgrep (BurntSushi/ripgrep) — 50k+ stars, MIT/Unlicense
- **Performance as differentiator:** detailed benchmark comparison table vs. 6 competing tools with timing ratios
- **Honest limitations section:** "when not to use ripgrep" — counterintuitive transparency that builds enormous trust
- **Comprehensive user guide (GUIDE.md, 1026 lines, 11 sections):** converts curious visitors into users
- **FAQ with 27 practical questions:** sign of a project that listens to users
- **Minimalist excellence:** single maintainer, no Discord, no elaborate CI, proves code quality and docs speak for themselves
- **Gaps:** No CONTRIBUTING.md, no Code of Conduct, no SECURITY.md, no issue/PR templates, no badges

### JS/Framework Ecosystem

#### React (facebook/react) — 245k stars, MIT
- **Tiered onboarding in README:** "Quick Start" (5 min), "Add to Existing Project", "Create New App"
- **24 CI workflows organized by domain:** compiler_, devtools_, runtime_, shared_ prefixes
- **Targeted bug report templates:** separate forms for general bugs, compiler bugs, DevTools bugs
- **Clean categorized CHANGELOG:** version-date headers, categorized changes, PR links per entry
- **Gaps:** CONTRIBUTING.md is 5-line stub, no Discussions tab, no Discord, no governance document

#### Next.js (vercel/next.js) — 139k stars, MIT
- **Branded README with strong visual identity:** centered logo, badges, "MADE BY Vercel" authority badge
- **Detailed contributing.md (157 lines):** fork/clone/branch setup, exact yarn commands, ChromeDriver prerequisites
- **Clear branch strategy:** PRs target `canary`; `canary` merges into `master` on release
- **Security bug bounty program:** advertises "Open Source Software Bug Bounty program"
- **Gaps:** No quick-start commands in README, no inline CHANGELOG link, minimal issue templates

#### Astro (withastro/astro) — 59.1k stars, MIT
- **Gold-standard CONTRIBUTING.md (746 lines):** prerequisites, Codespaces one-click setup, hot-reload dev, 3 ways to test locally, complete test-running reference (Mocha, node:test, Playwright), TypeScript project references explained, code structure documentation, public vs. internal API design, SSR execution contexts, triage workflow with flowchart, priority system (p1-p5), preview releases via label, benchmark system (!bench trigger), release process, prerelease management
- **Sophisticated governance model:** 4 contributor levels + special teams (Moderator, TSC, Core Residency, Alumni), 70% voting majority, consensus-seeking RFCs, documented moderation enforcement
- **config.yml disables blank issues and redirects:** 4 contact links (Support/Discord, Docs, Feature Ideas/Discussions, Chat)
- **Changesets for automated version management:** per-PR version bumps, automated release PRs
- **Auto-labeler for PRs:** file path globs mapped to labels
- **Org-level `.github` repository** for shared community health files across all repos
- **Gaps:** No Discussions tab on core repo, FUNDING.md at org level not repo level

#### Svelte (sveltejs/svelte) — 86.5k stars, MIT
- **Multiple non-code contribution paths:** triaging issues, improving tutorials, answering Discord questions, filing feature requests
- **Breaking change template:** 4 mandatory fields (who affected, how to migrate, why, severity)
- **Ecosystem CI testing:** tests changes against dependent projects in the Svelte ecosystem
- **RFC process** for large features via dedicated `sveltejs/rfcs` repo
- **Gaps:** No quick start in README, no formal governance document, no code architecture docs, minimal README (2 badges)

#### Tailwind CSS (tailwindlabs/tailwindcss) — 94.9k stars, MIT
- **Gold-standard CHANGELOG:** explicitly follows Keep a Changelog and SemVer, `## [X.Y.Z] - YYYY-MM-DD` headers, Added/Fixed/Changed/Deprecated categories, scope prefixes, PR links
- **Test-first culture:** "we do not accept contributions without tests" stated clearly
- **Clear boundaries on feature contributions:** "We don't often accept pull requests for new features" — directs to Discussions
- **CODEOWNERS file:** single `* @tailwindlabs/engineering` line for auto-review assignment
- **Gaps:** Sparse README (no quick start), no Code of Conduct in README, no governance document, no Discussions tab

### Infra/DevOps Ecosystem

#### Kubernetes (kubernetes/kubernetes) — 122k stars, Apache-2.0
- **Governance as Infrastructure:** dedicated `kubernetes/community` repo with SIG definitions, membership tiers, steering committee, election processes
- **Structured YAML-form issue templates:** bug-report, enhancement, failing-test, flaking-test with auto-labeling
- **Modular contributor guide:** `contributors/guide/` with 15+ standalone files (first-contribution, github-workflow, pull-requests, coding-conventions, style-guide, issue-triage, non-code-contributions)
- **Per-version changelogs:** `CHANGELOG/CHANGELOG-1.X.md` for every minor version
- **100% community profile:** all GitHub Community Standards checks present
- **Gaps:** CONTRIBUTING.md is thin redirect, no FUNDING.yml, README has minimal visual appeal

#### Docker/Moby (moby/moby) — 71.5k stars, Apache-2.0
- **PRINCIPLES.md:** 11 design principles ("Be an ingredient, not a replacement", "Readability over cleverness", "Prioritize reversibility")
- **Complete `project/` directory:** GOVERNANCE.md, PRINCIPLES.md, RELEASE-PROCESS.md, REVIEWING.md, ISSUE-TRIAGE.md, PACKAGERS.md, BRANCHES-AND-TAGS.md
- **"Everything is a PR" governance:** decisions about philosophy, design, roadmap, and APIs all flow through PRs
- **DCO integration:** `Signed-off-by` enforced at CI level with full DCO v1.1 text in CONTRIBUTING.md
- **18 CI workflows:** DCO, unit tests, integration tests, VM tests, CodeQL, labeler, buildkit, ARM64, zizmor security
- **Gaps:** No quick-start in README, no LICENSE badge, minimal CODEOWNERS (8 entries)

#### OpenTofu (opentofu/opentofu) — 28.6k stars, MPL-2.0
- **Transparent fork governance:** 6 named TSC members, public bi-weekly meetings, agendas 3 days in advance, removal policies
- **Structured changelog with UPGRADE NOTES:** three categories per release — UPGRADE NOTES, ENHANCEMENTS, BUG FIXES
- **SECURITY.md depth:** PST structure, fix lead rotation, disclosure timelines, upstream dependency advisories, false positive policy
- **CNCF Code of Conduct** with specific reporting email
- **Theme-aware logo rendering:** `#gh-dark-mode-only` / `#gh-light-mode-only` image filters
- **Gaps:** Thin CONTRIBUTING.md, complex docs build system, no CODEOWNERS

#### Grafana (grafana/grafana) — 73.6k stars, AGPL-3.0
- **85 CI workflows:** backend checks, unit tests, lint, integration, E2E, frontend, perf, a11y, i18n, artifact publishing, release, backporting, stale, TruffleHog, govulncheck, schema checks
- **Dual-licensing with LICENSING.md:** AGPL-3.0 for main project, explicit Apache-2.0 exceptions for SDK/client libraries
- **Six issue templates:** bug, feature, accessibility, data source request, staff issues, plus config
- **Dependabot + Renovate with grouped updates:** Go modules grouped by ecosystem
- **Contributor Experience:** Champions Program, feedback survey, Storybook, plugin development guide
- **Gaps:** No quick-start in README, thin MAINTAINERS.md, overwhelming CI surface area

#### Vite (vitejs/vite) — 80.4k stars, MIT
- **Commit convention rigor:** Angular-inspired format enforced by `semantic-pull-request.yml` CI, enables auto-changelog
- **Best-in-class CONTRIBUTING.md:** repo setup (pnpm, Corepack, Windows cloning), dependency philosophy, debugging (VS Code config, logging scopes, source maps), testing (Vitest, integration, Playwright), PR guidelines, maintenance with Mermaid flowcharts for triage, review, and release
- **SECURITY.md with threat model:** defines trusted vs. untrusted inputs, explicit scope boundaries
- **Issue template automation:** auto-close without reproductions (3-day timer), `npx envinfo` for system info
- **Release process documentation:** SemVer, cadence (patches weekly, minors bi-monthly, majors yearly), support tiers, deprecation policy
- **Gaps:** PR template is too bare (just comment instructions), no FUNDING.yml, no CODEOWNERS

---

## Cross-Cutting Synthesis: What the Best Projects All Do

### Universal Patterns (15+ of 20 projects)

1. **README with badges** — Every project except Ripgrep and Rust uses CI/version/license badges
2. **LICENSE file at root** — 20/20 projects
3. **CONTRIBUTING.md** — 17/20 projects (exceptions: Ripgrep, Tailwind, React)
4. **GitHub Actions CI** — 19/20 projects (all except Ripgrep which uses Travis CI legacy)
5. **Issue templates** — 16/20 projects have at least basic templates
6. **CODE_OF_CONDUCT.md** — 14/20 projects (missing: Caddy, PocketBase, Ripgrep, Tailwind, React)
7. **SECURITY.md** — 11/20 projects (most commonly missing file)

### High-Impact Differentiators (only the top 5-8 projects)

| Practice | Projects That Do It | Impact |
|----------|-------------------|--------|
| Governance document with membership tiers | Kubernetes, Astro, Biome, OpenTofu, Moby | Signals institutional maturity |
| PRINCIPLES.md or design philosophy | Moby (only one) | Gives contributors a shared decision framework |
| Structured changelog with categories | React, Tailwind, Prometheus, OpenTofu, Vite | Professional release management |
| Changesets/Changie for automated changelogs | Astro, Biome, Dagger, Svelte | Eliminates "forgot changelog" problem |
| Threat model in SECURITY.md | Vite, Caddy | Prevents wasted effort on non-issues |
| Per-version changelog files | Kubernetes, OpenTofu | Essential for enterprise consumers |
| Auto-labeling for PRs | Astro, Biome, Grafana | Routes PRs to right reviewers automatically |
| RFC process for major changes | Svelte, Astro, Rust | Prevents wasted implementation effort |
| Codespaces/devcontainer one-click setup | Astro | Removes environment setup friction |
| Dogfooding own product in CI | Dagger, Temporal | Ultimate confidence signal |

---

## The Master Checklist for Cleat

### Tier 1: Launch Blockers (must be in place before going public)

- [ ] **Two-command quick start** — `brew install cleat && cleat dev start` or equivalent. Inline in README, not behind a link. (Temporal model)
- [ ] **8-10 quality badges** in README header: CI, Go Report Card, license, codecov, OpenSSF Scorecard, govulncheck, Go version, Discord, GitHub release (Prometheus/Caddy model)
- [ ] **Single-sentence value proposition** opening the README — answers "what and why" in one line (PocketBase model)
- [ ] **Detailed SECURITY.md** — supported versions, scope boundaries, threat model (WASM sandbox, PostgreSQL), AI/LLM disclosure, reproduction requirements, disclosure timeline (Caddy/Vite model)
- [ ] **CODE_OF_CONDUCT.md** — Contributor Covenant 2.1 with full enforcement ladder, not a stub (Biome model)
- [ ] **CONTRIBUTING.md (10+ sections)** — prerequisites (Go, Rust, Node, PostgreSQL), build from source, test commands by category, Svelte UI dev setup, WASM build instructions, IDE debugging, commit conventions, release process (Astro/Temporal model)
- [ ] **YAML-form issue templates** — bug_report.yml (required: cleat version, OS, PostgreSQL version, repro steps), feature_request.yml, config.yml that blocks blank issues (Prometheus/Astro model)
- [ ] **PR template** — change-type label `[FEATURE]/[BUGFIX]/[SECURITY]/[ENHANCEMENT]`, release notes block, testing checklist, AI disclosure dropdown (Prometheus/Caddy model)
- [ ] **LICENSE clarity** — if dual-licensing (AGPL server + MIT/APACHE SDKs), document exceptions in LICENSING.md (Grafana model)
- [ ] **Discord server** — link prominently in README header, make joining step 1 in CONTRIBUTING.md (Dagger model)

### Tier 2: First-Week Priorities (before first external PR lands)

- [ ] **PRINCIPLES.md** — articulate cleat's design philosophy (PostgreSQL as source of truth, WASM-first sandboxing, boring infrastructure) (Moby model)
- [ ] **GOVERNANCE.md** — maintainer tiers, voting process, meeting cadence, TSC structure (Astro/Biome model)
- [ ] **community-membership.md** — contributor → reviewer → maintainer pathway with time and contribution requirements (Kubernetes model)
- [ ] **Commit convention + CI enforcement** — Angular-inspired `feat(scope):`, `fix(scope):` with semantic-pull-request.yml (Vite model)
- [ ] **CODEOWNERS** — auto-assign reviewers based on changed paths (Tailwind model)
- [ ] **Auto-labeling for PRs** — file path globs mapped to labels (`area/cli`, `area/wasm`, `area/ui`) (Astro model)
- [ ] **Structured changelog** — Keep a Changelog format with UPGRADE NOTES section at top of each release (Tailwind/OpenTofu model)
- [ ] **Release notes CI check** — validate that PRs include changelog entries (Prometheus model)
- [ ] **GoReleaser + embedded Svelte UI workflow** — pre-build UI, validate no dirty state, GoReleaser (PocketBase model)
- [ ] **DCO signing as CI check** — `Signed-off-by` enforcement (Moby model)

### Tier 3: First-Month Investments (before first stable release)

- [ ] **Multi-layered CI pipeline** — CI, golangci-lint, govulncheck, CodeQL, fuzzing, stale issue management, benchmark tracking (Temporal/Prometheus model)
- [ ] **Renovate + Dependabot** with grouped Go module updates (Grafana model)
- [ ] **Performance benchmarks vs. competitors** — comparison table vs. Temporal, Restate, DBOS (Ripgrep model)
- [ ] **Honest limitations section** — "When NOT to use cleat" (Ripgrep model)
- [ ] **docs/architecture/** — system diagrams, execution engine, WASM compilation path, PostgreSQL schema (Temporal model)
- [ ] **Separate user guide** (GUIDE.md or docs site) — step-by-step tutorials beyond the README (Ripgrep/Tokio model)
- [ ] **RFC process** — dedicated repo or discussions category for major architectural proposals (Svelte/Astro model)
- [ ] **Discussion templates** — for feature proposals, not just bug reports (Zed model)
- [ ] **FUNDING.yml** — Open Collective or GitHub Sponsors (Astro/Svelte model)
- [ ] **Codespaces/devcontainer** — one-click development environment (Astro model)
- [ ] **Release process documentation** — versioning approach, release cadence, support tiers, deprecation policy (Vite/Tokio model)
- [ ] **Ecosystem CI** — test changes against dependent SDKs/clients (Svelte model)
- [ ] **Breaking change template** — mandatory migration impact assessment in PRs (Svelte model)
- [ ] **Non-code contribution paths** — explicitly encourage docs, triage, community support contributions (Svelte/Kubernetes model)

### Specific to Cleat's Architecture

- **Svelte + Go dual-track:** Pre-build the Svelte UI and commit artifacts. CI validates they stay in sync. This avoids making every contributor install Node.js. (PocketBase model — the closest analogue to cleat's architecture)
- **WASM compilation docs:** No project reviewed covers WASM compilation. Document this prominently in both README and CONTRIBUTING.md. This is a differentiator.
- **PostgreSQL dependency:** Include `docker compose up -d` in quick start. Consider SQLite support for zero-config local dev (Temporal supports 5 database backends including SQLite-in-memory).
- **Threat model:** Durable execution engines run untrusted WASM. Define what's in scope (sandbox escapes, PostgreSQL injection) vs. out of scope (application-level bugs in user workflows). (Vite model)
- **Dual-licensing:** AGPL for server, MIT/Apache-2.0 for SDKs and WASM runtime. Document exceptions in LICENSING.md. (Grafana model)
- **Value proposition:** "Write workflows in Go, compile to WASM, store in PostgreSQL, monitor from a Svelte UI" — this is cleat's unique selling point vs. Temporal's Java/TS-only SDKs.

---

## Summary Statistics

| Dimension | Projects That Excel | Cleat's Current State |
|-----------|-------------------|----------------------|
| README badges | 18/20 | Has none |
| Quick start inline | 8/20 | Has example code but no runnable quick start |
| SECURITY.md | 11/20 | Missing |
| CODE_OF_CONDUCT.md | 14/20 | Missing |
| CONTRIBUTING.md > 100 lines | 12/20 | Has basic CONTRIBUTING.md, could expand |
| YAML issue forms | 10/20 | Missing |
| PR template | 12/20 | Missing |
| Governance doc | 6/20 | Missing |
| Changelog automation | 8/20 | Missing |
| Discord community | 12/20 | Missing |
| DCO/CLA | 8/20 | Missing |

**Bottom line:** Cleat already has solid bones — good README content, architecture diagrams, a working codebase, and a basic CONTRIBUTING.md. The gap to "looks like a top-tier OSS project" is about 15-20 files in `.github/`, a Discord server, a polished README header with badges, and a structured changelog. The Astro project is the single best model to follow overall, given cleat's multi-package monorepo structure and community-driven aspirations.
