# cleat

[![CI](https://github.com/cleat-team/cleat/actions/workflows/ci.yml/badge.svg)](https://github.com/cleat-team/cleat/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/Go-1.27+-00ADD8?logo=go)](https://go.dev/doc/devel/release)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://github.com/cleat-team/cleat/blob/main/LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/cleat-team/cleat)](https://goreportcard.com/report/github.com/cleat-team/cleat)
[![Discord](https://img.shields.io/badge/Discord-join%20chat-5865F2?logo=discord&logoColor=white)](https://discord.gg/cleat)
[![Go Reference](https://pkg.go.dev/badge/github.com/cleat-team/cleat.svg)](https://pkg.go.dev/github.com/cleat-team/cleat)

> **Durable workflow engine -- runs on PostgreSQL, MySQL, or SQL Server. Write in Go, compile to WASM, deploy via INSERT.**

```bash
git clone https://github.com/cleat-team/cleat && cd cleat
go install github.com/cleat-team/cleat/cmd/cleat@latest
cleat dev --entry-point Greet --input '{"name":"Ada"}' ./testdata/hello/
```

<!-- Corrected 2026-08-09: this previously read
       docker compose up -d postgres
       cleat dev start
     Neither line worked. `docker-compose.yml` doesn't exist at the repo root
     (only docker-compose.{partner,dev,cluster,monitoring}.yml do), and there
     is no `dev start` subcommand -- `cleat dev` parsed "start" as a Go
     package path and exited 1 with "package start is not in std". `cleat
     dev` also runs entirely locally, without a database, so the compose
     line was never needed for it in the first place; that's the point of
     `dev` mode -- see the full Quick Start below for the build/deploy/worker
     path that does need Postgres.

     Corrected again 2026-09-23 (cleat#1967): this then pointed `--entry-point
     PlaceOrder` at testdata/basic, which makes a DurableCall to a "catalog"
     service nothing in this repo provides -- the very first command a reader
     ran failed unconditionally with a connection-refused error. It also gave
     `./testdata/basic/` as a bare relative path after a `go install ...@latest`
     line, which reads as "no clone needed" and isn't: run from outside a
     clone, that path does not exist, and even run from inside one at the
     wrong working directory, `testdata/basic` fails to build at all ("could
     not import github.com/cleat-team/cleat/cleat ... no required module
     provides package") because it depends on this repo's go.work to supply
     the SDK. testdata/hello's Greet makes no DurableCall, so it is the whole
     workflow -- nothing else has to run for the command to complete -- and
     the `git clone && cd` above makes the relative path resolve and puts the
     working directory where go.work is. Run in CI from a fresh clone,
     asserting completion rather than merely that the process started:
     cmd/cleat/readme_first_command_completes_test.go. -->

## What is Cleat

Cleat is a durable workflow engine that turns your existing PostgreSQL, MySQL, or
SQL Server database into an orchestration backend. Workflows are written in Go (or
Rust), compiled to WebAssembly, and stored directly in the database. A stateless
Go worker daemon polls the database, claims ready workflows, and drives execution
with deterministic replay, checkpointing, and failover -- no new infrastructure required.

Cleat ships with an embedded Svelte web UI for monitoring, a CLI for build/deploy/
management, and a WASM-free test framework for fast unit tests. It is self-hosted,
Apache 2.0 licensed, and designed for teams that already run a supported relational
database.

## Quick Start

This walkthrough runs from the root of a **checkout of this repository** — steps 2
and 3 read `migrations/` and `testdata/`, which are repo-relative. If you have not
cloned yet: `git clone https://github.com/cleat-team/cleat && cd cleat`.

```bash
# 0. Verify your toolchain (one command)
make setup

# 1. Build the CLI from THIS checkout, into ./bin.
#    In this walkthrough, build rather than `go install .../cmd/cleat@latest`:
#    the published CLI is v0.2.0, from a release branch this one has not
#    merged, and its `deploy` has no --db flag -- step 4 then fails with a
#    usage error instead of deploying. (That is a fact about THIS checkout
#    tracking ahead of the last release, not about the published CLI being
#    broken; for installing cleat outside a checkout, `@latest` is right --
#    see Installation below.)
#    Build into ./bin deliberately: `-o cleat` writes *inside* ./cleat/, which
#    is a directory in this repo, so ./cleat stays a directory and is not
#    runnable.
go build -o ./bin/cleat ./cmd/cleat
go build -o ./bin/cleat-worker ./cmd/cleat-worker

# 2. Start Postgres and apply the schema. `cleat deploy` (step 4) does not
#    migrate, and the worker refuses to start against an unmigrated database,
#    so this has to happen first. Run it through the migration runner rather
#    than a `psql -f` loop: only the runner records `schema_migrations`, and
#    without that table the worker reports the database as never migrated.
#    The OWNER DSN is required here -- the app role in step 5 has no DDL
#    rights. See docs/explanation/postgresql-schema.md.
export CLEAT_OWNER_DSN="postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable"
docker compose -f docker-compose.partner.yml up -d postgres
./bin/cleat-worker --migrate-only --db "$CLEAT_OWNER_DSN"

# 3. Compile a workflow package to WASM. The -o directory must sit OUTSIDE this
#    module: pointed inside it, the follow-on compile resolves the output
#    directory as a package path and fails with "main module
#    (github.com/cleat-team/cleat) does not contain package
#    github.com/cleat-team/cleat/out" (cleat#2473).
./bin/cleat build -o /tmp/cleat-build ./testdata/hello/
# Wrote /tmp/cleat-build/greet.wasm -- testdata/hello declares exactly one
# entry point (Greet), which step 7 does not have to name: the worker reads
# it from the WASM's own cleat.metadata when there is only one candidate.

# 4. Deploy to your database. The owner DSN is correct here.
./bin/cleat deploy --db "$CLEAT_OWNER_DSN" \
    --name hello /tmp/cleat-build/greet.wasm

# 5. Give the worker a connection it will accept. It REFUSES a superuser DSN,
#    because PostgreSQL never applies row-level security to a superuser. The
#    schema already creates `cleat_app` with its grants -- what it needs is
#    LOGIN and a password, which is this one statement (cleat#2468):
psql "$CLEAT_OWNER_DSN" -c "ALTER ROLE cleat_app LOGIN PASSWORD 'cleat_app'"
export CLEAT_APP_DSN="postgres://cleat_app:cleat_app@localhost:5432/cleat?sslmode=disable"

# 6. Start the worker daemon. --api-addr has NO default: without it the worker
#    opens no HTTP port and step 7 cannot connect. A fleet migrates once as a
#    deploy step (step 2) and then starts workers with no --migrate flag -- see
#    docs/operations/upgrading.md.
./bin/cleat-worker --db "$CLEAT_APP_DSN" --api-addr :8080 &

# 7. Mint an API key for the default tenant and trigger a workflow. The route
#    requires the key -- and note it is POST .../<name>/start, not POST
#    .../workflows (that route is GET-only and returns 405 on POST).
#    Greet's only parameter is a single string, so "input" is that string
#    directly, not an object -- an object would bind literally, as text.
./bin/cleat-worker --db "$CLEAT_APP_DSN" \
    --generate-api-key 00000000-0000-0000-0000-000000000000
export CLEAT_API_KEY='cleat_sk_...'   # paste the key the command printed
curl -X POST http://localhost:8080/api/workflows/hello/start \
    -H "Authorization: Bearer $CLEAT_API_KEY" \
    -d '{"input":"Ada"}'
```

<!-- Corrected 2026-09-27. This block was run verbatim, in both of its documented
     forms, against a checkout of a30e5769 and could not reach a running workflow.
     Every change above is a measured correction; see cleat#2473.

     `go install .../cmd/cleat@latest` resolved to v0.2.0, whose `deploy` accepts
     only -name and -task-queue -- `cleat deploy --db ...` exits 2. The tree's own
     CLI has -db, -dry-run and -max-history-length, and deploys successfully. The
     published version is not an older snapshot of this tree: it is a different
     lineage, which is why the interfaces differ.

     The `-o ./out` form could not be kept. It fails on BOTH CLIs with identical
     output, so it is not the version skew: with a `go.work` in scope that `use`s
     the module, the compile resolves the output directory as a package path.
     Reproduced from scratch by adding a three-line go.work to an otherwise
     passing project, and cleared by GOWORK=off in that project.

     `--migrate-on-start` moved out of step 6 for two independent reasons: on the
     owner DSN the worker refuses it (superuser), and on the app DSN it dies with
     "core database migrations failed" (no CREATE/ALTER rights). Step 2 uses
     --migrate-only as the owner instead, which is also what records
     schema_migrations.

     The previous text credited `--migrate-on-start` with applying the schema,
     which is true, and started deploy before it, which is the order that fails. -->

<!-- Corrected 2026-09-30 (cleat#2788, split from cleat#2469): steps 3/4/7 previously built,
     deployed and triggered testdata/basic's PlaceOrder, which makes a DurableCall to a
     "catalog" service nothing in this repository provides -- the exact defect cleat#1967 found
     and fixed in this file's very first snippet (the "try it" block, above "## What is Cleat"),
     by switching it to testdata/hello's dependency-free Greet. That fix never reached this
     section, so a worker actually run against these exact steps failed at step 7 with "service
     catalog.LookupItem not configured: no endpoint registered" -- reachable because nothing ran
     this section in CI until cmd/cleat/readme_quick_start_reaches_done_test.go, added in the
     same change. Fixed the same way as the "try it" block above: testdata/hello/Greet in place
     of testdata/basic/PlaceOrder, and no `entry_point` in step 7's body -- Greet is the WASM's
     only declared entry point, so the worker resolves it from cleat.metadata without being
     told, and naming it explicitly would require "input" to be an object (to merge
     `__entry_point` into), which conflicts with Greet's single-string parameter binding the
     whole input value as text. -->

<!-- Corrected 2026-08-09: `cleat build ./testdata/basic/` was previously

<!-- Corrected 2026-08-09: `cleat build ./testdata/basic/` was previously
     followed by `cleat deploy ... ./out/place_order.wasm`, a file `cleat
     build` never writes (it writes cancel_order.wasm -- the first entry
     point found, all three bundled inside). Step 6 previously POSTed to
     `/api/workflows` with a `def_name` body, which routes to the GET-only
     list handler and returns 405; the real route is
     `/api/workflows/<name>/start`, and the input field name for PlaceOrder
     is `userID` (camelCase, matching the Go parameter name), not
     `user_id`. Verified by building testdata/basic and reading
     cmd/cleat-worker/server.go's route table and cmd/cleat/main.go's
     runDeploy. -->

See the [Quick Start Tutorial](docs/tutorials/quick-start.md) for a complete
walkthrough with a real-world example.

## Key Features

- **Durable execution** -- deterministic replay via event history; workflows survive
  worker crashes, restarts, and network partitions.
- **Multi-DB backends** -- PostgreSQL 16+, MySQL 8.0+, SQL Server 2022+, each with an
  independent implementation of the full workflow store. Database-enforced tenant
  isolation (row-level security) exists on PostgreSQL and SQL Server -- FORCEd RLS
  policies on PostgreSQL, a native `SECURITY POLICY`/`FILTER PREDICATE` on SQL Server.
  MySQL has no row-level security feature at all, so it is documented single-tenant
  only rather than emulating isolation the database can't back up (see
  `docs/reference/multi-tenancy.md`).
  **All of the above is engine support, not CLI support**: the `cleat` CLI (`deploy`,
  `versions`, `rollback`, `schedule`, `lock`, `plugin`) only connects to PostgreSQL
  today, and refuses a MySQL or SQL Server connection string with an explicit error
  rather than a confusing driver failure. `cmd/deploy-workflow --driver mysql|mssql`
  is the one multi-dialect entry point, and it covers `deploy` only (see `tiers.yaml`).
- **Plugin system** -- extensible via LLM, Slack, webhooks, and custom plugins;
  plugins run in-process with lifecycle hooks.
- **WASM workflows** -- write in Go, Rust, Python, Java, or AssemblyScript, compile to
  WebAssembly. wasmtime is the only WASM backend cleat has, and it requires CGO --
  CPU/wall-clock/memory limits come from epoch interruption, fuel, and store limits.
  There is no fallback: a `CGO_ENABLED=0` build has no backend at all, and
  `cleat-worker` exits 1 at startup rather than running unfenced (check any worker
  with `cleat-worker --verify-backend`). wazero is still in the tree as
  `engine.Runtime`, but it executes guest code only for CLI and test tooling --
  `cleat run_embedded`, `cleatctl replay|debug`, `cleat-bench`, `cleat/wasmtest` --
  never for a worker. See `docs/explanation/security-model.md`.

  <!-- Corrected 2026-09-06: this read "wazero is a pure-Go, CGO-less fallback with
       no compute-bound fencing", and called wasmtime "the backend of record", which
       implies a second backend to be the record against. There is no second backend:
       `engine/backend_wazero.go` was deleted in #459 (2026-08-10) -- confirm with
       `ls engine/backend_wazero.go`. The claim survived here for four weeks, and it
       is the one a reader comparing cleat's portability would rely on: it advertises
       a pure-Go deployment path that does not exist, and understates the CGO
       requirement from "slower/unfenced without it" to "does not start without it". -->
- **Signals and human-in-the-loop** -- `AwaitSignals` pauses workflows for external
  input; signals are recorded in the event history for deterministic replay.
- **Saga / compensating transactions** -- structured rollback with `DurableDefer`,
  `DurableDeferFunc`, and `cleat.NewSaga()`.
- **Horizontal scaling** -- stateless workers, `SELECT ... FOR UPDATE SKIP LOCKED`
  claim model, scale out by adding worker processes.
- **CLI toolchain** -- `cleat build`, `vet`, `deploy`, `versions`, `rollback`, and
  cron `schedule` management.
- **Observability** -- embedded Svelte web UI, Prometheus metrics, structured logging.

## Documentation

| Section | Description |
|---------|-------------|
| [Tutorials](docs/tutorials/) | Step-by-step walkthroughs: quick start, first workflow, signals |
| [How-To Guides](docs/how-to/) | Practical guides: plugins, testing, deployment |
| [Reference](docs/reference/) | CLI reference, SDK API, worker configuration, [workflow lifecycle](docs/reference/workflow-lifecycle.md) |
| [Explanation](docs/explanation/) | Architecture, execution model, security, WASM compilation |
| [Operations](docs/operations/) | Production deployment, disaster recovery, upgrading |
| [Migration Guides](docs/migration/) | Migrating from Temporal, DBOS, Restate |
| [Contributor Guide](CONTRIBUTING.md) | Setting up a dev environment, coding standards, PR process |

Start at the [Documentation Home](docs/index.md) to find the right page for your goal.

## Installation

macOS and Linux (with [Homebrew](https://brew.sh)):

```bash
brew install cleat-team/tap/cleat
```

Installs `cleat`, `cleat-worker` and `cleat-gen` from source -- see
"macOS: `cleat-worker` needs a from-source install" below for why -- and the
tap is bumped automatically on every release (cleat#2068), so there is no
version to track by hand.

Anywhere with Go, or if you'd rather not add a tap:

```bash
# Install all CLI tools
go install github.com/cleat-team/cleat/cmd/cleat@latest
go install github.com/cleat-team/cleat/cmd/cleat-worker@latest
go install github.com/cleat-team/cleat/cmd/cleat-gen@latest
```

Or build from source: `git clone https://github.com/cleat-team/cleat.git && cd cleat && go install ./cmd/...`

> If you are working **inside** a checkout of this repository -- following the
> [Quick Start](#quick-start) above, or building workflows against the SDK in
> that tree -- build the CLI from that checkout rather than installing
> `@latest`. The checkout tracks ahead of the last release, so the two are not
> the same code, and the interfaces can differ. See the note in step 1 above.

### Linux: `.deb` package with a systemd unit

Starting with 0.3.0, tagged releases attach a `cleat-worker` `.deb` for
`amd64` and `arm64`, built and tested on Ubuntu 26.04:

```bash
curl -LO https://github.com/cleat-team/cleat/releases/download/vX.Y.Z/cleat-worker_X.Y.Z_linux_amd64.deb
sudo dpkg -i cleat-worker_X.Y.Z_linux_amd64.deb
```

It installs the binary to `/usr/bin/cleat-worker`, a `cleat` system user and
group, a disabled-by-default `cleat-worker.service` unit, and an env-file
template at `/etc/cleat/cleat-worker.env`. Set `CLEAT_DATABASE_URL` there
(and any other flags, via `CLEAT_WORKER_ARGS`), then:

```bash
sudo systemctl enable --now cleat-worker
```

**Measured glibc minimum: `GLIBC_2.34`, both `amd64` and `arm64`** (checked
2026-09-23 against a worker built the same way the release does):

```bash
objdump -T cleat-worker | grep -o 'GLIBC_[0-9.]*' | sort -Vu | tail -1
```

That's below Ubuntu 26.04's own glibc (2.43) despite the package being built
there -- the CGO surface this binary actually touches (Go's runtime plus
wasmtime's cdylib) doesn't request anything newer. Distros at or above 2.34
(RHEL 9, Ubuntu 22.04+, Debian 12+ among them) **may** work as a result, but
that's not tested or supported for 0.3.0 -- **Ubuntu 26.04 is the only
tested target.** Re-run the command above against whatever's published if
you're relying on the number for a specific release, since a future
dependency could raise it.

No `.rpm` for 0.3.0. Users on an untested or unsupported distro: the
container image (`ghcr.io/cleat-team/cleat-worker`), `go install` above, or
the Homebrew formula below.

### macOS: `cleat-worker` needs a from-source install

The release archives contain **no macOS `cleat-worker`**. The worker needs CGO
for the wasmtime runtime — wasmtime is the only WASM backend cleat has, and a
CGO-less build exits 1 at startup — and the release job runs on Linux, which
cannot link a CGO macOS binary. `cleat` and `cleat-gen` are unaffected and ship
for macOS as usual.

`brew install cleat-team/tap/cleat` (above) and `go install
.../cmd/cleat-worker@latest` both close that gap the same way: they compile
the worker with CGO on your own machine, where the Xcode Command Line Tools
that Homebrew and `go install` both already require guarantee a C toolchain.
The formula's test block additionally runs `cleat-worker --verify-backend`, so
a worker that cannot construct the backend fails the `brew install` rather
than being discovered later.

Working on the formula itself (`packaging/homebrew/Formula/cleat.rb.tmpl`)
rather than installing it: see "Releasing a Homebrew formula bump" in
`docs/project/release-process.md` for the `--HEAD --build-from-source` path
that builds it from `develop` without needing a release tag at all.

To check any `cleat-worker`, however you installed it:

```bash
cleat-worker --verify-backend      # exits 0 only if the wasmtime backend is live
```

## License

Apache 2.0. See [LICENSE](LICENSE) for details.
