# Deploying the clew-lifecycle Workflow

## Prerequisites

- Go 1.25+ (at `/usr/local/go/bin/go`)
- cleat repo at `/localssd/rcownie/cleat`
- clew repo at `/localssd/rcownie/clew`
- cleat worker running with PostgreSQL (Neon)
- Database URL in `CLEAT_DB_URL` or `DATABASE_URL`

## Build

The workflow is a Go WASI/WASM binary, built with the standard Go toolchain
targeting wasip1. Build with the cleat CLI:

```sh
cd /localssd/rcownie/cleat
go build -o /tmp/cleat ./cmd/cleat/

# Build from the testdata copy (preferred — no replace directives needed):
export PATH="/usr/local/go/bin:/usr/local/bin:$PATH"
/tmp/cleat build -o workflow.wasm testdata/clew-lifecycle/
```

Or build from the original workflow directory with GOWORK:

```sh
mkdir -p /tmp/clew-build/workflow
cp /localssd/rcownie/clew/workflows/tasklifecycle/*.go /tmp/clew-build/workflow/
cp /localssd/rcownie/clew/workflows/tasklifecycle/cleat.yaml /tmp/clew-build/workflow/
cat > /tmp/clew-build/go.work << 'EOF'
go 1.25.7
use (
    /tmp/clew-build/workflow
    /localssd/rcownie/cleat
)
EOF
GOWORK=/tmp/clew-build/go.work /tmp/cleat build -o workflow.wasm /tmp/clew-build/workflow/
```

**Expected output**: `workflow.wasm`.

### Rebuilding the worker (plugin changes)

The clew-executor plugin is compiled into the worker binary, not the workflow
WASM. When the executor plugin changes (in `plugins/clewexecutor/`), rebuild
and restart the worker — the workflow WASM does NOT need to be rebuilt:

```sh
cd /localssd/rcownie/cleat
go build -o bin/cleat-worker ./cmd/cleat-worker/
```

## Deploy

Build and deploy the WASM using the cleat CLI (built from source):

```sh
cd /localssd/rcownie/cleat
go build -o /tmp/cleat ./cmd/cleat/

# Deploy to a new version name:
source .env  # sets CLEW_DATABASE_URL
/tmp/cleat deploy --db "$CLEW_DATABASE_URL" --name clew-lifecycle-v<N> workflow.wasm
```

### Version naming convention (Gap D)

**Always use a new version name** for each deploy. The database auto-increments a version number internally, but re-deploying to an existing name will fail with a version mismatch error because the WASM metadata version and DB version get out of sync.

Naming convention: `clew-lifecycle-v<NNN>` where NNN increments with each deploy.
Current deployed name: `clew-lifecycle-v104`. Next deploy: `clew-lifecycle-v105`.

DO NOT re-deploy to an existing name (e.g., `clew-lifecycle-final`).

## Worker restart (Gap E)

The cleat worker caches workflow WASM in memory at startup. A newly deployed workflow version is NOT picked up until the worker restarts.

### When restart is needed
- After every workflow deploy (the worker won't see the new version without a restart).

### How to restart
```sh
# Find and kill existing workers:
pkill cleat-worker

# Restart (example — adjust paths as needed):
cd /localssd/rcownie/cleat
./bin/cleat-worker \
  --db 'postgresql://neondb_owner:npg_uaBC4ecPGI3l@ep-raspy-cloud-aq9zafvh.c-8.us-east-1.aws.neon.tech/neondb?sslmode=require' \
  --api-addr :8080 \
  --require-auth=false &
```

### Verify the new workflow is active
```sh
# Check the worker dashboard:
curl -s http://localhost:8080/ | head -20

# Or check workflow definitions API:
curl -s http://localhost:8080/api/workflows | grep clew-lifecycle
```

**Warning**: In-flight workflow instances are lost on worker restart. Coordinate timing — let active task instances reach stable states before restarting.

## Dispatch target update

After deploying under a new name, update the dispatch target in `clew-run.sh`:

In `/localssd/rcownie/clew/src/clew-run.sh`, change the workflow name in the API endpoint:

```sh
# Old:
POST "$CLEW_WORKER_URL/api/workflows/clew-lifecycle-v103/start"

# New:
POST "$CLEW_WORKER_URL/api/workflows/clew-lifecycle-v104/start"
```

## Smoke test

After deploy + restart + dispatch update, verify end-to-end:

```sh
cd /localssd/rcownie/clew

# 1. Create a throwaway test task:
src/new-task.sh clew-NNN "Smoke test for workflow v104" --priority 5 --budget 1

# 2. Dispatch via the workflow:
src/clew-run.sh clew-NNN --execute

# 3. Watch STATUS.md progress through phases:
grep "^\\*\\*Phase:" task_state/clew-NNN/STATUS.md

# Expected progression: queued → exploring → planning → plan_review
#   → implementing → impl_review → done

# 4. Verify review artifacts contain outcome markers:
grep -r "OUTCOME:" task_state/clew-NNN/artifacts/

# 5. Verify ReviewOutcome in session.json:
grep "review_outcome" task_state/clew-NNN/session.json
```

## Known issues

### Gap D: Version proliferation on re-deploy
Each deploy requires a new version name. Re-deploying to an existing name fails due to a DB version / WASM metadata mismatch. Always increment the name.

### Gap E: Worker WASM caching requires restart
The worker loads all workflow WASM into memory at startup. New versions aren't picked up until restart. In-flight workflow instances are lost on restart.

### Gap F: No standalone cleat CLI
There is no standalone `cleat` binary for build/deploy. The WASM is built with the standard Go toolchain (`GOOS=wasip1 GOARCH=wasm go build`) from the cleat repo's toolchain. Deployment uses the `deploy-workflow` Go tool built from `cmd/deploy-workflow/`.
