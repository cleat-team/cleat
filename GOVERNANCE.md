# Cleat Governance

This document describes the governance structure for the cleat project. It is
scaled for a small, growing open-source project -- not Kubernetes. The goal is
to be clear enough to prevent disputes, lightweight enough to not get in the
way, and explicit enough that every contributor knows where they stand.

---

## Roles and responsibilities

### Contributor

Anyone who contributes to the project -- opening issues, commenting on
discussions, submitting pull requests, writing documentation, or helping
others.

**Privileges:**
- Can open issues and participate in discussions
- Can submit pull requests
- Can participate in community calls
- Can review pull requests (review comments are welcome but non-binding)

**Responsibilities:**
- Follow the [Code of Conduct](CODE_OF_CONDUCT.md)
- Sign the [CLA](docs/cla.md) before their first PR is merged
- Include a `Signed-off-by` line on all commits (DCO)

### Regular Contributor

A contributor who has demonstrated sustained engagement with the project.

**Requirements:**
- 5+ merged pull requests
- Active over a period of 3+ months
- Participating in discussions, reviews, or community support

**Privileges:**
- All Contributor privileges
- May be assigned issues and PRs
- May be asked to review specific PRs in their area of expertise
- Listed in REGULAR-CONTRIBUTORS.md

**Responsibilities:**
- All Contributor responsibilities
- Help triage issues in areas of expertise
- Provide constructive reviews when requested

### Maintainer

A trusted contributor responsible for the quality and direction of a specific
area of the project (e.g., WASM pipeline, worker daemon, Go SDK, Rust SDK,
web UI).

**Requirements:**
- 10+ merged pull requests
- 6+ months of sustained activity
- Sponsored by an existing Maintainer or Core Maintainer
- Demonstrated code review ability (has provided thorough, correct reviews)
- Demonstrated understanding of the project's [design principles](PRINCIPLES.md)
- Passes a maintainer vote (see [Decision making](#decision-making))

**Privileges:**
- All Regular Contributor privileges
- Write access to the repository (`git push` to non-protected branches)
- Can approve and merge pull requests (with another Maintainer's approval)
- Can label and close issues
- Can propose RFCs
- Voting rights in medium-changes decisions
- Listed in MAINTAINERS.md

**Responsibilities:**
- Review PRs promptly in their area of expertise
- Triage and respond to issues
- Mentor new contributors
- Participate in maintainer votes
- Attend community calls when possible
- Uphold the project's design principles and code quality standards

### Core Maintainer

A maintainer with a deep understanding of the entire codebase and a proven
track record of architectural stewardship over 12+ months.

**Requirements:**
- Maintainer for 12+ months
- Sustained, high-quality contributions across multiple areas
- Demonstrated architectural judgment (design docs, RFC reviews, cross-cutting
  changes)
- Approved by the Technical Steering Committee (TSC)
- Currently limited to 3 Core Maintainers

**Privileges:**
- All Maintainer privileges
- Can merge PRs with only their own approval (for trivial/blocked changes)
- Can veto a decision (sends it to TSC review)
- Can nominate new Maintainers
- TSC membership (see [TSC](#technical-steering-committee-tsc))

**Responsibilities:**
- All Maintainer responsibilities
- Architectural guidance and design reviews
- Drive the project roadmap
- Resolve technical disputes escalated by Maintainers
- Guide the project through major version changes and breaking releases

### Lead Maintainer

The project's ultimate decision-maker. Currently **@rcownie** (Richard Cownie),
the project creator. The Lead Maintainer role is not permanent -- it is
defined by the project's dependency on its founder. If the Lead Maintainer
steps down, the role passes to the most senior Core Maintainer by TSC vote.

**Requirements:**
- Appointed by the current Lead Maintainer, or elected by 2/3 TSC vote
- Must be a Core Maintainer

**Privileges:**
- All Core Maintainer privileges
- Final say on all decisions (see [Conflict resolution](#conflict-resolution))
- Can veto maintainer votes (veto can be overridden -- see below)
- Manage repository secrets, CI configuration, and release publishing
- Owner of the GitHub organization and package registries

**Responsibilities:**
- All Core Maintainer responsibilities
- Final escalation point for disputes
- Community stewardship and Code of Conduct enforcement
- Release management
- Relationship with external partners and sponsors

---

## Decision making

Decisions are categorized by impact. Each category has a different process.

### Minor changes (lazy consensus)

Bug fixes, documentation corrections, dependency updates, test improvements,
and other changes that do not alter the project's behavior or API surface.

**Process:**
1. Submit a pull request following the branch naming convention.
2. Automated CI checks must pass (lint, test, DCO, semantic title, branch naming,
   CLA check for first-time contributors).
3. An AI review is posted automatically as a first-pass advisory check.
4. Leave the PR open for 24 hours for human review.
5. If no objections are raised, any Maintainer can approve and merge.
6. If an objection is raised, the change is elevated to medium.

**Voting:** Not needed. Silence implies consent.

**Exception:** Security fixes may be merged immediately and disclosed
separately.

### Medium changes (maintainer vote)

New features (behind feature flags), non-breaking refactors, dependency
additions, and changes that alter behavior without breaking existing APIs.

**Process:**
1. Open an issue or discussion to propose the change.
2. Discuss the approach and gather feedback (minimum 72 hours).
3. Submit a pull request implementing the proposal.
4. Maintainers vote: approve, request changes, or abstain.
5. A simple majority of voting Maintainers is required for approval.
6. The change is merged once approved and all CI passes.

**Voting:** Each Maintainer gets one vote. Core Maintainer votes are not
weighted differently. Abstentions do not count toward the total.

### Major changes (2/3 majority, RFC required)

API changes, breaking changes, new SDK languages, architectural changes,
governance changes, and removal of a Maintainer.

**Process:**
1. Write an RFC (see [RFC process](#rfc-process)).
2. Publish the RFC as a GitHub discussion for minimum 14 days.
3. Incorporate feedback and revise.
4. Call a formal vote.
5. 2/3 majority of all Maintainers (not just those who vote) is required.
6. The Lead Maintainer may veto (requires 2/3 override -- see below).
7. The change is merged or implemented once approved.

**Voting:** All Maintainers and Core Maintainers vote. A "no" vote must
include a rationale. The RFC and voting record are archived.

### RFC process

RFCs (Request for Comments) are used for major changes. An RFC must include:

- **Summary:** One-paragraph description of the proposed change
- **Motivation:** Why this change is needed, what problem it solves
- **Design:** Technical approach, API surfaces, migration path
- **Alternatives considered:** What else was evaluated and why this was chosen
- **Impact assessment:** Backward compatibility, performance, observability,
  dependencies

RFCs are published as GitHub Discussions with the `RFC` label. The discussion
period is 14 days minimum. The RFC author is responsible for incorporating
feedback and updating the proposal.

---

## Technical Steering Committee (TSC)

The TSC provides architectural oversight and escalation resolution.

**Current composition:**
- @rcownie (Lead Maintainer) -- sole member
- When there are 5+ Maintainers, the TSC expands to 3 members:
  - The Lead Maintainer
  - One Core Maintainer elected by the Core Maintainers
  - One Maintainer elected by the Maintainers (non-Core)

**Term:** TSC members serve 12-month renewable terms.

**Responsibilities:**
- Approve architectural RFCs
- Resolve technical disputes escalated from Maintainers
- Approve new Core Maintainers
- Approve removal of a Maintainer (see [Removal policy](#removal-policy))
- Define the project roadmap

**Meetings:** The TSC meets as needed, but at least quarterly. Meetings are
announced on the public community calendar.

---

## Meeting cadence

### Bi-weekly community call

- **Frequency:** Every two weeks
- **Duration:** 30 minutes
- **Format:** Public video call, published agenda in advance, notes published
  after
- **Topics:** Roadmap updates, RFC reviews, demo sessions, Q&A
- **Recording:** Notes are published within 48 hours. Recordings are optional
  and posted when available.
- **Calendar:** Published on the cleat website and in the repository
  (`docs/community-calendar.ics`)

### Quarterly TSC meeting

- **Frequency:** Every three months
- **Duration:** 60 minutes
- **Format:** Internal to TSC members, summary published
- **Topics:** Roadmap prioritization, architectural direction, maintainer
  reviews

---

## Removal policy

Maintainers can be removed for two reasons:

### Inactivity

A Maintainer who has been inactive for 6+ months may be removed. "Inactive"
means no PR reviews, no issue comments, no commits, and no community call
attendance. Before removal:

1. A Core Maintainer sends a private email asking about the Maintainer's
   status and intentions.
2. If there is no response within 30 days, or if the Maintainer confirms they
   no longer wish to participate, a removal vote is called.
3. The TSC votes. Simple majority required.
4. The Maintainer is moved to emeritus status and listed in
   MAINTAINERS.md with (Emeritus) annotation.

Inactive Maintainers may reapply at any time by opening an issue and
demonstrating renewed activity.

### Vote of no confidence

If a Maintainer's behavior is harmful to the project or community, a vote
of no confidence may be called.

1. Any Core Maintainer may propose a vote of no confidence.
2. The proposal must include a written rationale with specific examples.
3. The Maintainer in question is given 14 days to respond in writing.
4. A vote is held among all Maintainers (excluding the subject).
5. 2/3 majority is required for removal.
6. The decision is final. No reapplication for 12 months.

Removal under the no-confidence process does not affect the Maintainer's
past contributions, which remain in the project history under the original
license.

---

## Conflict resolution

Disagreements are normal and healthy. The goal is to resolve them fairly and
quickly.

1. **Direct discussion:** The parties involved discuss the disagreement, with
   a goal of reaching consensus. A third Maintainer may be brought in as a
   mediator.

2. **Escalation to Lead Maintainer:** If direct discussion does not resolve
   the issue, either party may escalate to the Lead Maintainer. The Lead
   Maintainer reviews the arguments and makes a decision.

3. **Lead Maintainer decision (14-day window):** The Lead Maintainer's
   decision is final for 14 days. During this window, the decision stands
   and is implemented.

4. **Maintainer override:** After 14 days, any Core Maintainer may call a
   vote to override the Lead Maintainer's decision. A 2/3 majority of all
   Maintainers (including the Lead Maintainer) is required. If the override
   passes, the decision is reversed or revised.

This two-step process prevents blocking decisions from being stalled
indefinitely while still providing a check on unilateral authority.

---

## Community membership pathway

The pathway from first contribution to maintainer is designed to be
transparent and merit-based. Each rung has concrete, measurable criteria.
There is no time limit for advancement -- contributors advance when they
meet the criteria, not when a seat opens up.

### Summary table

| Role | PRs | Time | Sponsor | Vote | Review ability |
|------|-----|------|---------|------|----------------|
| Contributor | 1 merged | -- | -- | -- | -- |
| Regular Contributor | 5 merged | 3+ months | -- | -- | -- |
| Maintainer | 10+ merged | 6+ months | Yes | Yes | Demonstrated |
| Core Maintainer | Sustained | 12+ months as Maintainer | -- | TSC | Architectural |

### Contributor

**Criteria:** 1 pull request merged into any cleat repository.

This is automatic. Anyone who has had a PR merged is a Contributor. There is
no nomination process -- it happens when the PR merges.

**How to get here:** Submit a pull request and get it reviewed and merged.

### Regular Contributor

**Criteria:**
- 5+ pull requests merged into any cleat repository
- Active over a period of 3+ months (contributions spread across the time
  period, not clustered in a single week)
- Demonstrated participation in discussions, issue triage, or community
  support (e.g., answering questions on GitHub Discussions or Discord)

**How to get here:** No formal nomination. A Maintainer nominates by adding
the contributor to `REGULAR-CONTRIBUTORS.md` in a pull request. The
nomination is announced on the community call and in a GitHub Discussion.
If there are no objections within 7 days, the PR is merged.

**Why this matters:** Regular Contributor is a recognition of sustained
engagement. It signals trustworthiness and readiness for more responsibility.
It is a prerequisite for Maintainer nomination.

### Maintainer

**Criteria:**
- 10+ pull requests merged into any cleat repository
- 6+ months of sustained activity (at least one contribution per quarter on
  average)
- Sponsored by an existing Maintainer or Core Maintainer
- Demonstrated code review ability (at least 5 substantial reviews on other
  contributors' PRs that were accurate and constructive)
- Demonstrated understanding of the project's design philosophy and
  development process
- Passes a maintainer vote

**Nomination process:**
1. A Maintainer or Core Maintainer opens a GitHub issue titled
   `Nomination: @username for Maintainer`.
2. The issue includes:
   - A summary of the nominee's contributions (linked PRs)
   - Evidence of code review ability (linked review comments)
   - Why the nominee would make a good Maintainer
3. The nomination is discussed for 14 days. Other Maintainers may ask
   questions or share observations.
4. A vote is held among existing Maintainers and Core Maintainers. Simple
   majority required.
5. If approved, the nominee is added to MAINTAINERS.md and granted repository
   write access.
6. If not approved, the sponsor may renominate after 3 months with additional
   evidence.

**How to get noticed:**
- Submit quality PRs that follow the project's conventions
- Provide thorough, constructive reviews on other contributors' PRs
- Participate in discussions and community calls
- Help triage issues and answer questions
- Show understanding of the project's design principles

### Core Maintainer

**Criteria:**
- Sustained Maintainer activity for 12+ months
- Contributions across multiple areas of the codebase
- Demonstrated architectural judgment (led or significantly contributed to
  at least one major feature or RFC)
- Approved by the TSC

**Nomination process:**
1. A Core Maintainer (or the Lead Maintainer) nominates the candidate in a
   TSC issue or meeting.
2. The TSC discusses and votes. 2/3 majority required.
3. If approved, the candidate is added to MAINTAINERS.md as a Core Maintainer
   and granted TSC membership (if TSC seats are available; otherwise they
   serve as Core Maintainer without TSC seat until the next TSC election).
4. There is a hard limit of 3 Core Maintainers at any time. If the limit is
   reached, a Core Maintainer must be elevated to Lead Maintainer or step
   down before a new Core Maintainer can be appointed.

### Emeritus status

Any Maintainer or Core Maintainer who steps down (voluntarily or through the
removal process) is listed as Emeritus in MAINTAINERS.md. Emeritus
Maintainers retain their contribution history and are welcome to rejoin
by going through the nomination process again. Their past service is
recognized and valued regardless of current status.

---

## AI/LLM usage policy

cleat welcomes the use of AI and LLM tools to assist with contributions. This
policy sets clear expectations for transparency and accountability.

### AI-assisted contributions

Contributors who use AI/LLM tools in creating a pull request must disclose this
in the PR template's AI disclosure section. Disclosure is not about permission —
it is about calibration. Knowing a contribution was AI-assisted helps reviewers
focus on correctness, safety, and fit rather than surface-level issues an AI
would typically catch.

The contributor is responsible for every line of code they submit, regardless
of origin. Using an AI tool does not excuse a contributor from understanding,
testing, and standing behind their contribution.

### AI code review

Every pull request receives an automated AI review as a first-pass advisory
check. The AI review identifies obvious issues (missing error handling, potential
race conditions, API inconsistencies, test gaps) before a human reviewer spends
time on the PR.

The AI review is **advisory, not blocking**. It exists to save reviewer time,
not to replace human judgment. Contributors may address AI feedback directly or
respond with a rationale for why a suggestion does not apply.

The AI reviewer has no vote in the decision-making process. Approval and merge
authority remains exclusively with human Maintainers.

### Vulnerability disclosure

If an AI/LLM tool assists in discovering a security vulnerability, this must be
disclosed in the vulnerability report per the [security policy](SECURITY.md).
This helps the security team assess the finding's provenance and reliability.

---

## MAINTAINERS.md

The `MAINTAINERS.md` file in the repository root lists all current
Maintainers, Core Maintainers, and the Lead Maintainer, along with their
areas of expertise and contact information. This file is updated whenever a
Maintainer is added, removed, or changes role.

---

## Amending this document

Changes to governance follow the [major changes](#major-changes-23-majority-rfc-required)
process: RFC required, 2/3 majority of all Maintainers, Lead Maintainer
vetoable. This ensures that governance changes are carefully considered and
broadly supported.
