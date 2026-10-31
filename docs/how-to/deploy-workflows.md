# How to deploy workflows

## Overview

Deploying a cleat workflow is a two-step process: build your workflow source code to a WebAssembly (WASM) binary, then insert that binary into the database so workers can load and execute it.

## Step 1: Build the workflow

Use the `cleat build` command to analyze, transform, and compile your workflow package to WASM:

```bash
# Build with the default Go compiler.
cleat build -o ./out ./path/to/workflow/

# Build for other language targets.
cleat build -o ./out --target rust ./path/to/rust-workflow/
cleat build -o ./out --target python ./path/to/python-workflow/
```

What `cleat build` does:

1. Loads and analyzes the Go package (or other language source)
2. Builds a call graph and computes the durable closure
3. Verifies `HostCalls` threading through all code paths
4. Generates WASM imports for host functions and a host adapter
5. Generates WASM exports for each workflow entry point
6. Compiles to a `.wasm` binary with embedded metadata (name, version, ABI version, plugin dependencies)

The output is a `.wasm` file in the directory specified by `-o`:

```
$ cleat build -o ./out ./examples/order/
  Analyzing package ./examples/order/...
  Found 12 functions, 1 entry point(s), 5 in cleat closure.
  Durable leaves: DurableCall, DurableSleep, SetQueryState
  Verifying HostCalls threading... OK
  Generating WASM imports (3 host functions used)... OK
  Generating host adapter... OK
  Generating WASM exports (1 entry point(s))... OK
  Build directory: /tmp/cleat-build-12345/
  Compiling WASM module (GOOS=wasip1 GOARCH=wasm)... OK
  Wrote /tmp/cleat-build-12345/place_order.wasm (2.3 MB)
  Embedded metadata: place_order v1 (ABI v1)
```

The output filename is derived from `cleat.yaml`'s own `name:` field when the
workflow has one -- `place_order.wasm` here assumes a `cleat.yaml` with
`name: place_order` next to the source (cleat#2692). With no `cleat.yaml`, or
one with no usable `name:`, it falls back to the entry point's own SOURCE
FILE instead: a `PlaceOrder` entry point living in, say, `order.go` then
produces `order.wasm` (cleat#2407). `--name` on `deploy` is independent of
either -- it need not match the artifact's filename at all.

## Step 2: Deploy the WASM binary

Use `cleat deploy` to insert the built WASM binary into the `workflow_defs` database table:

```bash
# Deploy with a specific name.
cleat deploy --db "$CLEAT_DATABASE_URL" --name place_order ./out/place_order.wasm

# Deploy to a specific task queue.
cleat deploy --db "$CLEAT_DATABASE_URL" --name place_order --task-queue high-memory ./out/place_order.wasm
```

The deploy command:

1. Reads the WASM file and extracts embedded metadata
2. Connects to PostgreSQL via `--db` or `CLEAT_DATABASE_URL`
3. Auto-assigns the next version number (`SELECT COALESCE(MAX(version), 0) + 1`)
4. Inserts a row into `workflow_defs` with the WASM bytes, version, ABI compatibility info, and plugin dependencies

### Dry-run preview

`cleat deploy` needs a database -- either `--db` or `CLEAT_DATABASE_URL` -- to
actually deploy. Without one, it refuses rather than silently doing nothing:

```bash
$ cleat deploy --name place_order ./out/place_order.wasm
Error: no database configured. Set CLEAT_DATABASE_URL or --db to deploy, or pass --dry-run to preview without one.
$ echo $?
1
```

Pass `--dry-run` to preview what would be deployed without connecting to a
database at all -- the same name and behavior as `cleat plugin install
--dry-run`. It exits 0 whether or not a database is configured, so it is safe
to run in a script that has neither:

```bash
$ cleat deploy --dry-run --name place_order ./out/place_order.wasm
Would deploy workflow "place_order" (version 1) from ./out/place_order.wasm (2457600 bytes) to queue "default"
  Metadata: place_order v1 (ABI: 1, min ver: 1)
Dry run: no changes were made.
```

> The size is printed in **raw bytes** (`%d`), not the `formatSize` MB/GB form that
> `cleat build` uses above — this block showed `2.3 MB`, which is what `build`
> prints and `deploy` does not (cleat#3027).

### Database role and tenant

`workflow_defs` carries row-level security, so `deploy` needs a connection
that can satisfy it. Use the same `cleat_app` role the worker itself runs as
-- the worker refuses a superuser or `BYPASSRLS` connection outright and its
own error messages name `cleat_app` as the role to use instead, so a DSN that
gets the worker running will also deploy.

`deploy` sets the RLS tenant context for its own transaction from the global
`--tenant` flag (default: the single-tenant default), the same way `cleat
lock` resolves it -- flag, then `CLEAT_TENANT_ID`, then the default. Pass it
before the subcommand: `cleat --tenant <tenant-uuid> deploy --db "$CLEAT_DATABASE_URL" ...`.

### Version management

Each `cleat deploy` creates a new version. Versions are auto-incremented integer values:

```bash
# Deploy v1.
cleat deploy --db "$CLEAT_DATABASE_URL" --name place_order ./out/place_order.wasm
# Deployed workflow "place_order" version 1

# Deploy v2 after making changes.
cleat build -o ./out ./path/to/workflow/
cleat deploy --db "$CLEAT_DATABASE_URL" --name place_order ./out/place_order.wasm
# Deployed workflow "place_order" version 2
```

### Listing versions

```bash
cleat --db "$CLEAT_DATABASE_URL" versions place_order
# 2
# 1
```

### Rollback

```bash
cleat --db "$CLEAT_DATABASE_URL" rollback place_order 1
# Rolled back "place_order" to version 1.
# New runs will use version 1 until 'cleat rollback --clear place_order'.
```

A rollback is a **pin, not a one-off**: it holds that version for new runs until
you clear it, and the tool's own message names the command that does. This
section used to stop at *"New instances will use version 1"*, which reads as
finished — the tool says `New runs`, and says what ends the pin (cleat#3027).

The WASM blob IS the version. Rolling back changes which WASM binary new workflow instances execute. Existing in-flight instances continue with the version they started on.

## Step 3: Deploy via REST API

When the worker runs with `--api-addr`, you can deploy workflows programmatically via the `POST /api/definitions` endpoint. `--require-auth` defaults to true, so most deployments need an API key (`cleat-worker --generate-api-key <tenant-id> --db "$CLEAT_DATABASE_URL"`) in the `Authorization` header:

```bash
# Encode the WASM binary as base64 and POST.
WASM_B64=$(base64 -w0 ./out/place_order.wasm)

curl -X POST http://localhost:8080/api/definitions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $CLEAT_API_KEY" \
  -d "{
    \"name\": \"place_order\",
    \"wasm_bytes_base64\": \"$WASM_B64\"
  }"
```

There is no `namespace` or `task_queue` field: neither is a concept this endpoint knows about. The version is the one the binary is stamped with (`cleat build --version N`, default `1`); leave `version` out and that stamp is used. A `version` that disagrees with the stamp is refused with a `400` naming both numbers, and so is a binary stamped `0`, because a worker only runs a binary under the version it is stamped with: a row numbered anything else is a version that never runs. A binary that carries no version stamp at all is given the next free number. The response is a `201` with the version:

```json
{
  "created": true,
  "name": "place_order",
  "version": 1,
  "plugin_deps": null
}
```

## Plugin dependencies are checked at deploy

A definition can declare the plugins it needs (`plugin_deps`: plugin name to version constraint). A deploy
**refuses** one that names a plugin no worker has ever registered, with `422` on the REST API and a
non-zero exit from the commands, naming **every** missing plugin and which declaration named it (the WASM
binary's embedded `plugin_deps`, the definition's own, or both). Nothing is written when it refuses
(cleat#3600).

"Registered" means a worker recorded a plugin of that **name** when it started. That record is kept
(nothing deletes from it), so a deploy does not fail across a rolling restart or a maintenance window, and
it catches a typo or a plugin that was never installed anywhere. It does not check versions, and it does not
check that a worker carrying the plugin is up now: a worker still checks its own loaded plugins and their
versions when it claims a run, and releases a run it cannot serve. So a deploy that passes is not a promise
that a run will start.

Because the record is written by workers, a **first** deploy that names a plugin must come after a worker
carrying that plugin has started once, or must say it is deliberate:

    cleatctl deploy workflow <name> <wasm> --allow-missing-plugins
    deploy-workflow --allow-missing-plugins ...
    cleat deploy --allow-missing-plugins ...
    POST /api/definitions   {..., "allow_missing_plugins": true}

An override changes **only the deploy that carries it**, and it is **recorded, not just logged**: before the
definition is written, one row goes into `admin.plugin_dep_overrides` (the tenant, the definition and version,
who asked, which path took it, and each missing plugin with its constraint and which declaration named it), and
it is also logged at `WARN`. That table is append-only for the application role on PostgreSQL and SQL Server. It is kept after the tenant is dropped, as an audit record is. If
the row cannot be written the deploy is **refused**: there is no override without a record of it. The row says the
check was bypassed, not that the deploy then succeeded. An override covers a missing plugin only: it does not turn
"the registry could not be read" into "allowed". **A library install has no override**: it refuses a bundle whose
members name an unregistered plugin (`422`, naming each plugin and the member that declared it) and writes
nothing, because an install goes into a running system.

`cleatctl deploy` carries the previous version's `plugin_deps` forward, so a redeploy is refused when one of
those plugin names has not been registered by any worker **since migration `008` was applied to that
database**; a plugin that registered once stays registered, even after it is retired. A definition that
declares no plugin dependencies is not affected. Nothing the repository ships declares any.

**Sharded deployments.** The registry, and the override record, live in the worker's own database, which for a
sharded worker is the **first shard's**: the same database worker registration writes and where the schema's
other administrative tables are. A deploy asks that one registry once, before the first shard is written, then
writes every shard or none: a refusal leaves nothing on any shard. A name registered only in another shard's
registry does not count. A command-line deploy (`cleatctl`, `cleat deploy`, `deploy-workflow`) reads the registry of the
database it is given, so **a CLI run against a non-first shard's DSN reads that shard's (empty) registry** and refuses
every declared plugin; point it at the first shard's DSN, or use `--allow-missing-plugins`, which is recorded. Library
install is not available on a sharded store yet.

**Upgrading.** The registry starts empty, and only a worker running a version that carries it writes to it: an
older worker writes nothing, and the worker table has no plugin column to fill it from. So from the moment
migration `008` runs until the first upgraded worker starts, every deploy or install that declares
`plugin_deps` is refused. **Upgrade at least one worker before deploying definitions that declare
`plugin_deps`, or pass `--allow-missing-plugins`** (deploy only; an install has no override, so install after
an upgraded worker has started). During a rolling upgrade an older worker still claims and releases runs
exactly as before: the check a worker makes against its own loaded plugins when it claims a run is unchanged,
and the registry plays no part in it.

## Locking child workflow versions

If your workflow calls child workflows, pin their versions at build time for reproducible deployments:

```bash
# Resolve child versions from the database and write a lock file.
cleat --db "$CLEAT_DATABASE_URL" build -o ./out ./path/to/workflow/

# Or write/refresh it as its own step.
cleat lock --db "$CLEAT_DATABASE_URL" ./path/to/workflow/
```

This generates a `cleat.lock` file that pins each child workflow to a specific version. During deployment, the lock file ensures the parent is paired with the correct child versions.

> Corrected 2026-10-03 (cleat#3027). This read *"Or **manually** create/update the
> lock file"* above the command that does it. `cleat lock` is a subcommand
> (`cmd/cleat/main.go`'s `runLock`, with `--update` to re-resolve from the
> database), so nothing here is manual -- and the wording implied a lock file a
> reader maintains by hand, which is the shape this audit looks for: a step
> justified by a limitation the tool does not have.

## Installing a workflow library

A **workflow library** is a set of immutable, versioned workflows the cleat operator publishes once,
and a tenant installs by reference instead of deploying the same code itself (cleat#3540). It is
PostgreSQL and SQL Server only; MySQL answers `501`.

    POST /api/libraries/{library}/install
    {"bundle_version": "1.2.0"}

It is called with the tenant's **own** API key and writes only that tenant's definitions: one row per
member of the bundle, naming the member it came from and holding no bytes of its own. What it does:

- **One transaction for the whole bundle.** Every member is installed and the install is recorded, or
  nothing is written. A half-installed upgrade cannot exist.
- **An upgrade is the same call with a different `bundle_version`.** It refreshes the tenant's
  definitions from their members, records the new bundle, and rewrites when. It does not delete a version
  an earlier bundle installed, because that version's runs replay it. Installing the bundle you already
  hold changes nothing, and does not re-enable a version you disabled.
- **It never replaces your own code.** If the tenant already deployed a definition under a name and version
  the bundle installs, or holds one from a different library, the install stops with `409` and writes nothing.
- **Exposure is resolved as a deploy resolves it.** A member that declares `public` is refused (`400`) for a
  tenant the operator has not opted in.

Every published library is installable by every tenant. A `404` means that library has no such bundle.

> A **sharded** deployment answers `501` to an install: serving many tenants on a sharded worker is not
> supported yet. A library can still be **published to every shard**, ready for when it is; see
> [Publishing to a sharded deployment](#publishing-to-a-sharded-deployment).

## Publishing a workflow library

Publishing is the **operator's** act, and the database makes that true: only a login that holds the
`cleat_library_publisher` role can write the library schema (and that role can insert and read but never
update). Connect with such a login; the superuser login works too, and the application role does not.
PostgreSQL and SQL Server only.

    cleatctl --db "$PUBLISHER_DSN" library publish acme 1.2.0 checkout.wasm pay.wasm
    cleatctl --db "$PUBLISHER_DSN" library list acme

- **A member is named by its own stamp.** Each binary's `cleat.metadata` carries its `workflow_name` and
  `workflow_version`; publish reads them from the file and lets nothing on the command line rename or
  renumber it. A binary with no readable stamp, or a `<file>.schema.json` that is not valid JSON, is refused.
- **A member's name starts with its library's name and a dot.** In the example above the stamps must say
  `acme.checkout` and `acme.pay`, and publish refuses a member named `checkout`, `acme.` (nothing after the
  dot), `Acme.checkout` (the prefix is case-sensitive) or `other.checkout` (another library's prefix). The
  refusal names the prefix and a name that would pass; set the workflow's name in its stamp and rebuild. The
  prefix is what keeps a library's workflows and a tenant's own apart (owner decision D59, cleat#3629), so a
  **library name cannot contain a dot** either: it would make two libraries' prefixes overlap.
- **A published prefix is reserved, in both directions.** Once a library is published, a tenant's own deploy of
  any name that starts with `<library>.` is refused (`409` on `POST /api/definitions`, a non-zero exit from
  `cleat deploy` and `cleatctl deploy workflow`), at any version, whether or not the library holds that name.
  An install is refused (`409`) when the tenant already holds workflows of its own under the prefix, and the
  error names them. Both are what stop a tenant's `acme.checkout` version 2 from silently shadowing an installed
  `acme.checkout` version 1, because a start without a version runs the highest one whatever its origin. A row
  a tenant already holds under a prefix (possible only for a library published before this rule) is not
  touched: it keeps running, and only a later deploy or install is refused until it is renamed or removed. The
  comparison is the database's own, and on PostgreSQL and SQL Server it is exact: `ACME.checkout` is a different
  name from `acme.checkout` and is not under the `acme.` prefix. (SQL Server compared names
  case-insensitively until migration `011`, cleat#3635, owner decision D60.)
- **A bundle is a consistent set.** Every `child_versions` pin in a member's stamp must name another member
  of the *same* bundle at that exact version, so a tenant that installs the bundle can never resolve a child it
  does not contain. A name appears once per bundle.
- **Members and bundles are immutable.** Publishing a `(library, name, version)` that already exists with the
  same bytes and the same entry-point schemas (the `.schema.json` beside the binary, which the bytes do not
  carry) is a no-op; with different bytes it is refused, naming both digests, and with the same bytes and
  different schemas it is refused saying so (publish the changed build under a new version). Publishing a bundle version that exists with exactly the same members is a no-op, which
  is how a publish that stopped partway is repaired; with a different member set it is refused and says what
  differs.
- **One transaction.** The members, the bundle and its listing are all written, or none are.
- **`library list [<library>] [--json]`** shows each bundle, who published it and when, and its members with
  their digest and size. It never prints the bytes.
- A plugin a member declares in `plugin_deps` is not checked at publish: it is checked, against the registry of
  plugins workers have registered, when a tenant installs.

### Publishing to a sharded deployment

A sharded worker keeps a library's members in the same database as the tenant rows that reference them, so a
bundle has to be published to **every shard**. Give `--db` the **admin shard** (the first entry in the worker's
`--shards-file`, see [`--shards-file`](../reference/worker-config.md#--shards-file)) and each other shard once
with `--replicate-to`, in the worker's order, each with **that shard's own publisher login**. The role is granted
per database, and the worker's own connection strings are not publisher logins:

    cleatctl --db "$SHARD1_PUBLISHER_DSN" library publish acme 1.2.0 checkout.wasm pay.wasm \
      --replicate-to "$SHARD2_PUBLISHER_DSN" --replicate-to "$SHARD3_PUBLISHER_DSN"

- **Every shard is opened and pinged before the first write**, so a mistyped or unreachable DSN, or a shard that
  is another kind of database, is found with nothing changed.
- **The admin shard is written first**, and a failure there changes nothing anywhere.
- **Every other shard is then written**, and one failing does not stop the rest. There is no transaction across
  databases, so a publish can reach some shards and not others. The error names the shards written, the ones
  that failed and the ones not tried, and **running the same command again completes it**: a shard that already
  holds the bundle is left as it is.
- **A conflict is not repaired by running it again.** A shard that already holds *different* content under the
  same member or bundle key refuses it, because published content is immutable. The error says so; publish the
  changed build under a new version.
- Shards are called `shard-1`, `shard-2`, ... in every message, with the host and database each one means printed
  beside an error. A connection string's user and password are never printed.
- `cleatctl` cannot check that `--db` really is the admin shard or that the shards are in the worker's order:
  the shards file is the authority. `library list` reads the one database `--db` names, so run it against each
  shard's publisher DSN to see what that shard holds.

## Production deployment checklist

Before deploying to production:

- [ ] Use a database connection string with `sslmode=require`
- [ ] Verify the WASM binary size is reasonable (monitor with `ls -lh`)
- [ ] Test the workflow with `cleat run --input <json> <package>` before deploying
- [ ] Run `cleat vet` to catch validation issues early
- [ ] Pin child workflow versions with a lock file
- [ ] Have a rollback plan (`cleat rollback` to the previous version)

## Next steps

- See the [zero-downtime deployment guide](../operations/zero-downtime-deploy.md) for blue/green worker pool replacement
- See the [production ops guide](../operations/deploying-to-production.md) for monitoring, scaling, and configuration
- See the [disaster recovery guide](../operations/disaster-recovery.md) for recovery procedures

> All three links were dead (cleat#3027). The first resolved to
> `docs/how-to/zero-downtime-deploy.md`; the other two resolved to `docs/guide/`,
> which does not exist in this tree at all. All three targets live under
> `docs/operations/`.
