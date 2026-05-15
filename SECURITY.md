# Security Policy

## Supported Versions

The cleat project provides security updates for the following versions:

| Version | Supported          |
|---------|--------------------|
| 1.x     | :white_check_mark: |
| < 1.0   | :x:                |

Only the latest minor release of the current major version receives security patches.
Users are strongly encouraged to upgrade to the latest version to ensure they receive
security fixes.

## Scope Boundaries

### In Scope

The following areas are considered in scope for security reports:

- **WASM sandbox escapes** -- vulnerabilities that allow a compiled WASM module to
  escape the wazero runtime sandbox and execute arbitrary code on the host, access
  the host filesystem, or make unintended network calls.
- **PostgreSQL injection via event history** -- SQL injection vectors where crafted
  workflow inputs, signal payloads, or event history data could manipulate database
  queries executed by the worker daemon.
- **Worker compromise** -- vulnerabilities that allow an attacker to take control of
  a `cleat-worker` process, including RCE, privilege escalation, or unauthorized
  access to the PostgreSQL database via worker credentials.
- **Denial of service against the worker** -- vectors that cause worker crashes,
  infinite loops, or resource exhaustion from crafted workflow inputs or WASM modules.
- **Secrets in workflow state** -- vulnerabilities that leak sensitive data (e.g.,
  database credentials, API keys, PII) through workflow event history, logs, metrics,
  or error messages.
- **Authentication and authorization bypass** -- flaws in the worker REST API or web
  UI that allow unauthorized access to workflow data, schedule management, or
  administrative operations.
- **Supply chain** -- compromises of the build pipeline, Go module dependencies,
  or the published CLI binaries.

### Out of Scope

The following areas are considered out of scope for security reports:

- **Application-level bugs in user workflows** -- logic errors, business rule
  violations, or data integrity issues in workflow code compiled to WASM.
  These are the responsibility of the workflow author.
- **Vulnerabilities in user-compiled WASM modules** -- security issues within the
  WASM binaries that users compile and deploy. Cleat provides the runtime boundary;
  the content of user modules is their own responsibility.
- **Vulnerabilities in user application code** -- security issues in services called
  via `DurableCall`. The cleat runtime passes requests and responses but does not
  validate service implementation security.
- **Dependency vulnerabilities in user projects** -- insecure dependencies in
  projects that use the cleat SDK, unless the cleat SDK itself introduces them.
- **Social engineering, phishing, or physical attacks** -- attacks against
  individual developers or infrastructure operators.

## AI/LLM Disclosure Requirement

If you used AI/LLM tools (including but not limited to ChatGPT, Claude, Copilot,
Gemini, or code generation assistants) to discover or analyze a vulnerability,
you must:

1. **State this explicitly** in the initial report. Include the tool name and
   version where applicable.
2. **Share the full conversation or prompt history** that led to the discovery,
   if possible and permissible.
3. **Indicate whether the AI tool identified a novel vulnerability** or merely
   helped analyze a finding you made through manual testing.

This information helps our security team assess the root cause, determine
remediation priority, and improve the project's security posture against
AI-assisted exploitation vectors. We will not penalize reporters for using
AI tools -- we simply require transparency.

## Exact Reproduction Requirements

All security reports must include:

- **Minimal Go workflow reproduction** -- a self-contained workflow file (or
  set of files) that triggers the issue. Include the workflow source, any
  required input data, and the expected vs. actual behavior.
- **Cleat version** -- the exact version or commit SHA where the vulnerability
  was observed. Include the output of `cleat version` or the Git commit hash.
- **PostgreSQL version** -- output of `SELECT version();` or the PostgreSQL
  version string (e.g., "PostgreSQL 16.2 on x86_64-pc-linux-gnu").
- **Operating system** -- OS name, version, and architecture (e.g.,
  "Linux Ubuntu 24.04 x86_64" or "macOS 15.0 arm64").
- **Go version** -- output of `go version`.
- **Worker invocation** -- the exact command line or configuration used to start
  `cleat-worker`, including all flags and environment variables.
- **Deployment topology** -- whether single worker or multi-worker, database
  connection details (host/port only, no credentials), and any proxy/load
  balancer configuration.
- **Logs and stack traces** -- relevant worker logs, PostgreSQL logs, or panic
  output. Remove any sensitive or personally identifiable information.

Reports lacking reproduction steps will be returned with a request for
additional detail.

## Disclosure Timeline

Cleat follows a **90-day disclosure policy**:

| Day | Event |
|-----|-------|
| **Day 0** | Report received. Security team acknowledges within 72 hours. |
| **Day 1-30** | Investigation, triage, and development of a fix. |
| **Day 31-60** | Internal testing and release preparation. |
| **Day 61-90** | Coordinated disclosure window. |

- We will work with the reporter to agree on a disclosure date. If the reporter
  wishes to disclose earlier (e.g., for academic publication), we ask for
  reasonable advance notice.
- If a fix requires more than 90 days, we will communicate the timeline and
  rationale to the reporter and negotiate an extension.
- If the vulnerability is being actively exploited in the wild, we may accelerate
  the timeline and issue an emergency release.
- We expect coordinated disclosure: please do not publish vulnerability details
  before the agreed-upon date without consulting us first.

## Contact

- **Email**: security@cleat.dev (placeholder -- not yet monitored)
- **GitHub Security Advisories**: https://github.com/cleat-team/cleat/security/advisories/new
  (preferred reporting method)

We encrypt sensitive communications using PGP. Our security team's PGP key
fingerprint is available on the GitHub security advisories page.

## Threat Model

### WASM Sandbox Boundary

Cleat uses [wazero](https://github.com/tetratelabs/wazero), a zero-dependency
WebAssembly runtime for Go, to execute compiled WASM modules. The threat model
assumes:

- **Trust boundary**: The wazero runtime and the host worker process are trusted.
  The WASM module is untrusted.
- **Capabilities**: WASM modules have access only to the 15 cleat host function
  imports (`cleat_call`, `cleat_sleep`, etc.). They cannot access the filesystem,
  network, environment variables, or system clock except through these host
  functions.
- **Memory isolation**: Each WASM module has its own linear memory. The wazero
  runtime enforces memory bounds; modules cannot read or write outside their
  allocated memory region.
- **Denial of service**: A malicious or buggy WASM module could enter an infinite
  loop or allocate excessive memory. The worker enforces execution timeouts and
  resource limits at the instance level.
- **Data flow**: Workflow inputs and outputs cross the WASM boundary via a
  pointer+length protocol through a scratch memory region. Host functions validate
  pointer bounds and length before reading or writing.

**Attack vector**: A compromised WASM module attempting to escape the sandbox,
read host memory, or execute unauthorized host system calls.

### PostgreSQL as Trust Boundary

The PostgreSQL database is a critical trust component:

- **Trust boundary**: The database is trusted. All workers and CLI tools that
  connect to it must authenticate with valid credentials.
- **Data integrity**: Event history, workflow definitions, and workflow state are
  stored in PostgreSQL. Corruption or unauthorized modification of this data
  could lead to incorrect workflow execution, replay divergence, or data leakage.
- **Injection vectors**: Workflow inputs and signal payloads pass through
  PostgreSQL as JSONB. While JSONB is typed and parameterized queries are used,
  any SQL injection in event history processing would compromise the entire
  system.
- **Multi-worker isolation**: Workers claim instances via `SELECT ... FOR UPDATE
  SKIP LOCKED`. A malicious worker could claim instances and fail to process them,
  blocking progress. Heartbeat monitoring and reaper goroutines mitigate this.
- **Credential management**: Database credentials should be provisioned with the
  minimum necessary privileges (INSERT/UPDATE/SELECT on workflow tables, CREATE
  for schema migrations). Never use a superuser account for worker connections.

**Attack vector**: An attacker gaining database access could read, modify, or
delete workflow data, or inject malicious event history that alters workflow
replay behavior.

### Worker Compromise Impact

If a `cleat-worker` process is compromised:

- **Database access**: The worker has credentials to read and write workflow data.
  A compromised worker could exfiltrate or corrupt the entire workflow database.
- **WASM module cache**: Workers cache WASM modules in memory. An attacker with
  worker access could read or replace cached modules.
- **REST API**: If the worker exposes an API (`--api-addr`), a network-level
  attacker could submit signals, manage schedules, or trigger workflow executions.
  The REST API should be bound to a trusted network or protected by a reverse
  proxy with authentication.
- **Horizontal spread**: Workers are designed to be stateless and interchangeable.
  Compromise of one worker does not directly compromise others, but shared
  database credentials could enable lateral movement.

**Mitigations**:
- Run workers with the minimum necessary OS privileges (not root).
- Bind the REST API to localhost or an internal network, not a public interface.
- Use connection pooling with encrypted connections (TLS) to PostgreSQL.
- Monitor worker heartbeats and set up alerts for anomalous behavior.
- Regularly rotate database credentials and use short-lived tokens where possible.
- Enable PostgreSQL audit logging for workflow tables.

### Dependencies and Supply Chain

- **Go modules**: All dependencies are fetched via the Go module proxy and
  verified using `go.sum` checksums. Dependabot is configured for automated
  update notifications.
- **Wazero**: As the WASM runtime, wazero is a critical dependency. We track
  wazero security advisories and update promptly.
- **WASM toolchains**: Go's `wasip1` target and TinyGo are compilation tools,
  not runtime dependencies. WASM binaries are compiled by the workflow author,
  not by the cleat project infrastructure.
- **Node.js dependencies**: The web UI build dependencies are development-only
  and not present in the worker runtime binary.

## Reporting Process

1. **Do not file a public GitHub issue** for security vulnerabilities.
2. Submit a report via [GitHub Security Advisories](https://github.com/cleat-team/cleat/security/advisories/new)
   or email **security@cleat.dev**.
3. You will receive an acknowledgement within 72 hours.
4. We will maintain confidentiality throughout the investigation and fix process.
5. We will add you to the advisory and credit you (unless you prefer to remain
   anonymous) when the fix is published.

We appreciate your help in keeping cleat and its users safe.
