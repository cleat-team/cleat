# Quick start: maintenance history

> Moved out of `docs/tutorials/quick-start.md` by cleat#3791. These are the blockquote notes the page
> carried between its steps -- what an earlier version said, which issue found it, what was measured and
> when -- with only the leading `>` removed. They are a record, not instructions: every date and count
> in them is as of its own date, and several describe states that no longer hold (a v0.3.2 CLI, a
> pseudo-versioned SDK, a loop that no longer exists). The page keeps the fact each note carried that a
> reader needs, in a sentence, under the step it belongs to. The commands in the notes were not re-run
> when they were moved.

## Introduction

Corrected 2026-10-03 (cleat#2995). This previously required a checkout:
*"steps 2 and 3 read `docker-compose.partner.yml` and `migrations/` ... and
there is no packaged distribution of the migration files."* **The migration
half was false** — `cleat-worker` embeds its migration tree, and the refusal
quoted under step 3 says *"this binary ships"* — and only the compose file was
ever repo-relative, which step 2 no longer uses. Measured on an empty database:
`--migrate-only` with no `--migrations-dir` applied every migration from the
binary and left **122 tables**.

## Step 1: verifying the installation, and installing from a checkout

Corrected 2026-08-09: this previously said `cleat version`. There is no
`version` subcommand -- `cleat`'s top-level usage line lists
`build|vet|deploy|versions|rollback|dev|schedule|run|dag|plugin|lock|init`,
and none of them print a CLI version. (There is a `versions` subcommand,
but it lists deployed *workflow* versions from the database, not the CLI's
own version, and needs `--db` to do anything.) `cleat build --help` is
used here instead because it needs no database and exits 0.

Corrected 2026-09-27: this step previously said `go install
github.com/cleat-team/cleat/cmd/cleat@latest`. That resolves to a CLI from a
**different lineage** than this checkout -- the published v0.2.0 predates
nothing here, its `deploy` has no `--db` flag, so step 6 fails with a usage
error rather than deploying -- and it installs no `cleat-worker`, which step
7 needs. Build both from the tree you are working in. Measured on a30e5769;
see cleat#2473.

Also worth knowing: `go build -o cleat ./cmd/cleat` (without the `bin/`)
does not work in this repository. `cleat/` is a directory here -- the Go SDK
-- and `go build -o <existing-directory>` writes the binary *inside* it, so
that command exits 0, leaves `./cleat` a directory, and buries a 59 MB binary
in the SDK source tree. `go build ./cmd/cleat` with no `-o` refuses outright
with `build output "cleat" already exists and is a directory`.

## Step 2: Start Postgres

These are the same settings `docker-compose.partner.yml` uses in this
repository -- `postgres:16`, user and password `postgres`, database `cleat`
-- so if you *are* in a checkout, `docker compose -f
docker-compose.partner.yml up -d postgres` is equivalent and the rest of this
guide is unchanged. Corrected 2026-10-03: this step gave only the compose
form, which is the one thing that actually required a checkout (cleat#2995).

Verified 2026-10-04 (cleat#3027): the `docker run` above starts cleanly, and
`docker exec cleat-postgres pg_isready -U postgres` prints exactly the line
below it. Still needed -- there is no packaged Postgres, and this container is
what makes the rest of the guide checkout-free (and, in step 7, supplies the
`psql` a reader without a local client would otherwise lack).

## Step 3: Apply the database schema (why it is a separate step)

Added 2026-08-09. No step in this guide previously did this at all, and
without it, `cleat deploy` (step 6) fails with `relation "workflow_defs"
does not exist` -- `cleat deploy` queries `workflow_defs` directly and does
not apply migrations itself. This was previously mentioned only in
`docs/explanation/postgresql-schema.md`, unlinked from either quick start.

Corrected 2026-10-03: this note used to end *"(`cleat-worker`, started later
in step 7, does apply `migrations/postgres/*.sql` automatically on boot -- but
by then it's too late, because deploy already ran and already failed)"*.
**That is now false, and it was the sentence a reader could act on wrongly**
-- someone who believed the worker migrates on boot would reasonably skip
this step. Since cleat#2117 migration is an explicit deploy step:
`--migrate-only` applies it and exits, and a normal start only **verifies**
and refuses.

`--migrate-on-start` is the other flag that applies them at boot, so *"the
worker can migrate"* is true with a flag and false as written. It still cannot
be used here, and the reason is the split this guide has to live with:
measured 2026-10-03, `--migrate-on-start` on the **owner** DSN migrates (122
tables) and then **refuses to serve**, for the reason step 7's note quotes
verbatim below (the owner connection is a superuser, and PostgreSQL never
applies row-level security to a superuser) -- while on the **app** DSN it
cannot migrate at all, because `cleat_app` has neither schema nor database
`CREATE`. Migration is a separate step because the two roles are separate: one
has DDL and cannot serve, the other serves and has no DDL.

Measured against an empty database:

```
refusing to start: the database schema is behind this worker: the database has
no schema_migrations table: it has never been migrated, and all 10 migration(s)
this binary ships are pending.
A worker does not migrate the database on start. Run the migrations as a deploy step:

    cleat-worker --migrate-only --db <dsn> [--migrate-db <owner dsn>]
```

Note what the message says in passing: *"all 10 migration(s) **this binary
ships**"* -- the migrations travel inside the binary, which is why this step
needs no checkout (cleat#2995).

## Step 3: Apply the database schema (the migration-runner note and its re-verification)

Corrected 2026-09-27: this was a `for f in migrations/postgres/*.sql; do psql
... -f "$f"; done` loop. That loop builds the schema but **not**
`schema_migrations` -- only the migration *runner* writes that table -- and a
worker started against the result refuses with:

```
refusing to start: the database schema is behind this worker: the database has
no schema_migrations table: it has never been migrated, and all 3 migration(s)
this binary ships are pending.
```

which reads as *"you skipped this step"* at the moment you did it correctly.
Measured on a fresh database: the loop leaves **86 tables** and no
`schema_migrations`; `--migrate-only` leaves 3 rows in it.

Those two counts are from 2026-09-27 and have grown since, because a migration
set only ever gains members. Re-measured 2026-10-03 on an empty database with a
develop build: `--migrate-only` leaves **122 tables** and **10 rows**, and the
refusal above quotes the same 10. Read the pair as the *shape* of the
difference -- many tables, and the `schema_migrations` the loop never writes --
rather than as constants.

`--migrate-only` also runs as the owner. That matters for step 7: the worker
refuses a superuser connection, and the app role it wants instead has no DDL
rights, so it cannot migrate.

Re-verified 2026-10-04 (cleat#3027), against an empty database, and **the
step is still needed, for the reason given**. A bare `cleat-worker --db <dsn>`
start neither migrated nor served: it exited 1 with *"the database has no
schema_migrations table: it has never been migrated, and all 10 migration(s)
this binary ships are pending. A worker does not migrate the database on
start"*, and it left **0** tables behind. `--migrate-only` then left the 10
`schema_migrations` rows the worker checks for -- so the missing row is what
is checked, not the table count.

The table counts in the notes above are censuses and have drifted (130 here
against the 122 recorded 2026-10-03). Read them as the *shape* of the
difference, exactly as they ask.

## Step 4: Scaffold a project

Corrected 2026-08-09: this previously showed a `greet(name string)`
function with a `//go:export greet` comment. That does not match `cleat
init`'s "basic" template (`cmd/cleat/init.go`, `scaffoldBasic`) -- the real
entry point is `Hello`, marked with a `// @cleatEntry(name="hello")`
comment.

**Corrected 2026-09-27, and this one removed a step rather than adding one.**
The 2026-08-09 correction above also claimed `cleat init` writes no `go.mod`,
and this section carried a two-command workaround for that. **Both are now
false.** `cleat init` does write `go.mod` and `go.sum`, and it pins the SDK
correctly:

```
module my-workflow
go 1.27.0
require github.com/cleat-team/cleat/cleat v0.0.0-20260927035713-a30e57693613
```

Refined 2026-10-04 (cleat#3027): that version is a **pseudo-version**, and
today's is a **tag**. The SDK submodule
(`github.com/cleat-team/cleat/cleat`) had never been tagged when the note above
was written; `cleat/v0.3.1` and `cleat/v0.3.2` were cut on 2026-09-27 alongside
the release, and `cleat init` resolves the scaffold's dependencies with `go mod
tidy`, which now selects the tag:

```
module my-workflow
go 1.27.1
require github.com/cleat-team/cleat/cleat v0.3.2
```

So a scaffolded project builds against the **latest released SDK**, not the
tree the CLI was built from -- worth knowing if you are working on cleat
itself and expected the project to track your checkout. Confirm what your
toolchain sees with
`go list -m -versions github.com/cleat-team/cleat/cleat` (measured 2026-10-04:
`v0.3.1 v0.3.2`).

Following the old workaround is now actively harmful. `go mod init my-workflow`
fails outright (`go.mod already exists`), and `go get
github.com/cleat-team/cleat@latest` **succeeds while adding the wrong thing**:
it pulls in the *root* module at `v0.2.0 // indirect`, from the other lineage
described in step 1, into a project that never needed it. Nothing fails --
which is why it is worth deleting rather than leaving as harmless.

There is nothing to run here. `cleat init` produces a buildable project.

The sample above was also stale in a way that would not compile: it called
`h.DurableLog("hello", "greeting")`, and this SDK's `DurableLog` takes a
single message argument. The template emits `h.DurableLog("hello: greeting")`.

## Step 5: Build the workflow

Corrected 2026-09-30: this previously said `hello.wasm`. The `-o` flag
names an output *directory*, and the compiled binary inside it is named
after `cleat.yaml`'s `name:` field when the project has one -- every
`cleat init` scaffold does, set to the project name given in step 4 -- and
only falls back to the entry point's *source file* when there is no
manifest (see `wasmOutputName`, `cmd/cleat/main.go`; cleat#2692). README's
Quick Start builds `testdata/hello/` directly, which has no `cleat.yaml`,
so its artifact is `hello.wasm` under the fallback rule -- a different
fixture taking a different branch of the same rule, not a different rule.
Found and fixed building cleat#2802's CI guard, which runs this exact
scaffold-build-deploy sequence and caught the mismatch at the deploy step
(`no such file or directory` for the documented `hello.wasm`).

**Measured 2026-10-04 (cleat#3027), by building the v0.3.2 tag itself: the
name above is the TREE's, and a CLI from before cleat#2692 writes a different
one.** v0.3.2's `cleat init` writes `project:` where the tree writes `name:`,
so `wasmOutputName` falls back to the entry point and these same three
commands produce `workflow.wasm/hello.wasm`. Step 6, which names
`my-workflow.wasm`, then fails with `no such file or directory` -- the *same*
error the note above records the guard catching, reached from the other side.
At the time, step 1's Homebrew install was v0.3.2 (`cleat-team/homebrew-tap`'s
`Formula/cleat.rb`, last bumped 2026-09-27), so this was the reader's path
rather than an edge case.

**RESOLVED 2026-10-07 (cleat#3065): 0.4.0 shipped, carrying cleat#2692, and
the tap's `homebrew-bump` release job pushed a fresh render the same day**
(`cleat-team/homebrew-tap`'s `Formula/cleat.rb` now pins `v0.4.0.tar.gz`,
commit `dbfe08f29`, 2026-10-07T03:13Z). A `brew install cleat-team/tap/cleat`
today builds the v0.4.0 tag, which writes `name:` and produces
`my-workflow.wasm` exactly as this guide describes -- re-verified by building
that tag directly and running steps 4-6 against it. No reader-facing
divergence remains; this note stays as the record of what to check if a
future release ever regresses `cleat#2692` (`cleat.yaml`'s `name:` key) or
the tap falls behind again.

## Step 7: Start the worker

Corrected 2026-09-27: this used the owner DSN above. **The worker refuses
it**, and correctly:

```
refusing to start: this connection is not subject to row-level security, so
tenant isolation is not enforced:
  - the connecting role "postgres" is a superuser, and PostgreSQL never applies
    row-level security to a superuser
```

The schema already creates `cleat_app` with its grants, so what is missing is
only `LOGIN` and a password -- the one `ALTER ROLE` above. Note the role
cannot migrate: it has no DDL rights, so `--migrate-only` (step 3) stays on
the owner DSN.

Re-verified 2026-10-04 (cleat#3027), and **two things about the line above**.

**The `ALTER ROLE` is still needed, and the reason is checkable.** `cleat_app`
is created `NOLOGIN` on purpose (`migrations/postgres/001_schema.sql:29`), and
after step 3 alone the database reports `rolcanlogin = f` for it -- so without
this line the role cannot authenticate and the worker's app-DSN connection
fails. The step's premise holds.

**But `psql` is not a prerequisite this guide lists.** Step 1 names Go, Docker
and the CLI; on a machine without a PostgreSQL client the line above is
unrunnable, and the container from step 2 already has a client that needs
nothing extra:

```bash
docker exec cleat-postgres psql -U postgres -d cleat \
    -c "ALTER ROLE cleat_app LOGIN PASSWORD 'cleat_app'"
```

Equivalently `-U postgres -d cleat` are the same credentials the DSN carries,
because step 2's container *is* the server. CI's guard for this tutorial
already substitutes for `psql` on exactly this ground -- *"not installed on
every CI runner"* (`cmd/cleat/tutorial_quick_start_reaches_done_test.go`) --
which is where the hazard was recorded, and not where a reader meets it.

## Step 8: Run the workflow

Corrected 2026-10-04 (cleat#3027): this said the key is printed to **stderr**.
It is printed to **stdout** -- `fmt.Printf`, `cmd/cleat-worker/main.go:777` --
and the only thing on stderr is the worker's own startup log lines. Measured
on the same binary at the v0.3.2 tag and on the current tree, so the claim was
wrong on both rather than true before a change. It matters if you capture or
discard a stream: `2>/dev/null` keeps the key, `>/dev/null` loses it.
