# How to deploy workflows

## Overview

Deploying a cleat workflow is a two-step process: build your workflow source code to a WebAssembly (WASM) binary, then insert that binary into the database so workers can load and execute it.

## Step 1: Build the workflow

Use the `cleat build` command to analyze, transform, and compile your workflow package to WASM:

```bash
# Build with the default Go compiler.
cleat build -o ./out ./path/to/workflow/

# Build with TinyGo (produces smaller binaries).
cleat build -o ./out --target tinygo ./path/to/workflow/

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

## Step 2: Deploy the WASM binary

Use `cleat deploy` to insert the built WASM binary into the `workflow_defs` database table:

```bash
# Deploy with a specific name and namespace.
cleat deploy --db "$DATABASE_URL" --name place_order --namespace staging ./out/place_order.wasm

# Deploy to a specific task queue.
cleat deploy --db "$DATABASE_URL" --name place_order --task-queue high-memory ./out/place_order.wasm
```

The deploy command:

1. Reads the WASM file and extracts embedded metadata
2. Connects to PostgreSQL via `--db` or `CLEAT_DATABASE_URL`
3. Auto-assigns the next version number (`SELECT COALESCE(MAX(version), 0) + 1`)
4. Inserts a row into `workflow_defs` with the WASM bytes, version, ABI compatibility info, and plugin dependencies

### Version management

Each `cleat deploy` creates a new version. Versions are auto-incremented integer values:

```bash
# Deploy v1.
cleat deploy --db "$DATABASE_URL" --name place_order ./out/place_order.wasm
# Deployed workflow "place_order" version 1

# Deploy v2 after making changes.
cleat build -o ./out ./path/to/workflow/
cleat deploy --db "$DATABASE_URL" --name place_order ./out/place_order.wasm
# Deployed workflow "place_order" version 2
```

### Listing versions

```bash
cleat versions --db "$DATABASE_URL" place_order
# 2
# 1
```

### Rollback

```bash
cleat rollback --db "$DATABASE_URL" place_order 1
# Rolled back "place_order" to version 1.
# New instances will use version 1.
```

The WASM blob IS the version. Rolling back changes which WASM binary new workflow instances execute. Existing in-flight instances continue with the version they started on.

## Step 3: Deploy via REST API

When the worker runs with `--api-addr`, you can deploy workflows programmatically via the `POST /api/definitions` endpoint:

```bash
# Encode the WASM binary as base64 and POST.
WASM_B64=$(base64 -w0 ./out/place_order.wasm)

curl -X POST http://localhost:8080/api/definitions \
  -H "Content-Type: application/json" \
  -d "{
    \"name\": \"place_order\",
    \"namespace\": \"staging\",
    \"wasm_base64\": \"$WASM_B64\",
    \"task_queue\": \"default\"
  }"
```

The API response includes the assigned version:

```json
{
  "name": "place_order",
  "version": 1,
  "namespace": "staging",
  "task_queue": "default",
  "status": "deployed"
}
```

## Locking child workflow versions

If your workflow calls child workflows, pin their versions at build time for reproducible deployments:

```bash
# Resolve child versions from the database and write a lock file.
cleat build -o ./out --db "$DATABASE_URL" ./path/to/workflow/

# Or manually create/update the lock file.
cleat lock --db "$DATABASE_URL" ./path/to/workflow/
```

This generates a `cleat.lock` file that pins each child workflow to a specific version. During deployment, the lock file ensures the parent is paired with the correct child versions.

## Production deployment checklist

Before deploying to production:

- [ ] Specify a namespace for environment isolation (`--namespace production`)
- [ ] Use a database connection string with `sslmode=require`
- [ ] Verify the WASM binary size is reasonable (monitor with `ls -lh`)
- [ ] Test the workflow with `cleat run --input <json> <package>` before deploying
- [ ] Run `cleat vet` to catch validation issues early
- [ ] Pin child workflow versions with a lock file
- [ ] Have a rollback plan (`cleat rollback` to the previous version)

## Next steps

- See the [zero-downtime deployment guide](zero-downtime-deploy.md) for blue/green worker pool replacement
- See the [production ops guide](../guide/deploying-to-production.md) for monitoring, scaling, and configuration
- See the [disaster recovery guide](../guide/disaster-recovery.md) for recovery procedures
