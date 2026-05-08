# Zero-downtime deployment

This guide covers deploying new cleat worker versions without interrupting
running workflows. The strategy uses a blue/green deployment model: a new
worker pool starts alongside the old pool, the old pool is drained, and the
new pool handles all work before the old pool is removed.

## Prerequisites

Before attempting a zero-downtime deployment, ensure:

- [ ] You have at least two worker instances (or enough capacity to run two
      pools side by side)
- [ ] Workers expose the admin API (`--api-addr` flag)
- [ ] Workers have adequate termination grace period (configured in your
      orchestrator)
- [ ] The draining worker's heartbeat interval allows timely reclaim of
      in-flight instances (default: 30 seconds)
- [ ] You have tested the procedure in a staging environment

## Blue/green deployment procedure

### Step 1: Deploy the new worker pool alongside the old

Start a new set of worker instances with the updated binary (or container
image) alongside the existing pool. Both pools connect to the same database.

```bash
# Old pool (blue) -- currently handling all workflows
cleat-worker --db "$DATABASE_URL" --concurrency 20 --api-addr :8080

# New pool (green) -- starts alongside the old pool
cleat-worker-v2 --db "$DATABASE_URL" --concurrency 20 --api-addr :8081
```

In a Kubernetes environment, deploy the new pool as a separate deployment:

```yaml
# green-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cleat-worker-green
spec:
  replicas: 3
  selector:
    matchLabels:
      app: cleat-worker
      version: green
  template:
    metadata:
      labels:
        app: cleat-worker
        version: green
    spec:
      containers:
      - name: cleat-worker
        image: cleat/worker:2.0.0
        args:
        - --db=$(CLEAT_DATABASE_URL)
        - --concurrency=20
        - --api-addr=:8080
        ports:
        - containerPort: 8080
```

```bash
kubectl apply -f green-deployment.yaml
```

### Step 2: New workers connect to the same database

Both the blue and green pools connect to the same PostgreSQL database. They
share the same `workflow_defs`, `workflow_instances`, and `event_history`
tables. This is safe because:

- **Claim arbitration**: workers use `SELECT ... FOR UPDATE SKIP LOCKED` to
  claim instances. Each instance is claimed by exactly one worker, regardless
  of which pool the worker belongs to.
- **No coordination needed**: workers are stateless and do not communicate with
  each other. The database is the sole source of truth for workflow state.
- **Version compatibility**: both worker versions must be within the same major
  version (see [Compatibility during coexistence](#compatibility-during-coexistence)).

```bash
# Both pools point at the same database URL
export DATABASE_URL="postgres://user:pass@db-host:5432/cleat?sslmode=require"

# Blue pool (old)
cleat-worker --db "$DATABASE_URL" --concurrency 20

# Green pool (new)
cleat-worker-v2 --db "$DATABASE_URL" --concurrency 20
```

### Step 3: Set old workers to drain

Once the new pool is healthy and accepting connections, instruct the old pool
to stop claiming new work. Use the admin API endpoint or send SIGTERM.

#### Using the admin API

The admin API provides a `/api/admin/drain` endpoint. When a worker receives a
`POST` to this endpoint, it immediately stops claiming new instances from the
database but continues executing its currently claimed in-flight workflows.

```bash
# Drain a specific worker (by address)
curl -X POST http://blue-worker-1:8080/api/admin/drain

# Drain all workers in the blue pool (using orchestrator-aware addressing)
for worker in blue-worker-1 blue-worker-2 blue-worker-3; do
    curl -X POST "http://$worker:8080/api/admin/drain"
done
```

#### Using SIGTERM

SIGTERM triggers the same drain behavior: the worker stops claiming new
instances and waits for in-flight workflows to complete before exiting.

```bash
# Send SIGTERM to all blue pool workers
pkill -TERM cleat-worker-v1
```

In Kubernetes, update the blue deployment's selector or scale it to zero:

```bash
# Option A: Scale blue deployment to zero
kubectl scale deployment cleat-worker-blue --replicas=0

# Option B: Remove the blue deployment
kubectl delete deployment cleat-worker-blue
```

#### What draining does

When a worker enters drain mode (either from the API or SIGTERM):

1. **Stop claiming**: the worker stops participating in the claim loop. It no
   longer runs `SELECT ... FOR UPDATE SKIP LOCKED` to acquire new instances.
2. **Continue execution**: the worker continues executing its currently claimed
   in-flight workflow instances. Each workflow runs to completion, fails, or
   suspends (awaiting a signal or timer).
3. **Heartbeat continues**: the worker continues to heartbeat for its claimed
   instances, preventing the reaper from reclaiming them prematurely.
4. **Release on completion**: as each in-flight workflow completes, the worker
   releases it by updating the instance status and clearing `assigned_to`.
5. **Exit after drain**: when using SIGTERM, the worker exits after all
   in-flight workflows complete or an internal timeout elapses. The admin API
   drain does not automatically exit the process.

Monitor drain progress:

```bash
# Check how many instances each worker still has
curl http://blue-worker-1:8080/health
# Response: {"status":"draining","workflows_running":3,...}
```

### Step 4: Wait for in-flight workflows to complete

Monitor the draining workers until they report zero running workflows.

```bash
# Poll until all blue workers report 0 running workflows
while curl -s http://blue-worker-1:8080/health | grep -q '"workflows_running":[1-9]'; do
    echo "Waiting for blue-worker-1 to drain..."
    sleep 5
done
```

For a large number of in-flight workflows, the drain can take minutes or hours,
depending on:

- **Workflow duration**: short-lived workflows drain quickly
- **Workflow with long timers**: workflows in `DurableSleep(24 * time.Hour)`
  do not complete quickly. These workflows are reassigned to the green pool
  after their heartbeat expires (see [Handling long-running workflows](#handling-long-running-workflows)).
- **Stuck workflows**: workflows awaiting external signals with long timeouts
  may not complete during the drain window

Monitor the health endpoint for each draining worker:

```json
{
    "status": "draining",
    "workflows_running": 5,
    "last_heartbeat": "2025-03-01T12:00:00Z",
    "database_connected": true
}
```

When `workflows_running` reaches 0, the drain is complete.

#### Handling long-running workflows

Workflows with long timers or signal waits may not complete within a reasonable
drain window. For these workflows, you have two options:

**Option A: Wait for heartbeat expiry**

When the draining worker exits (after SIGTERM) or is stopped, its claimed
instances eventually miss heartbeats. The reaper reclaims them after the
heartbeat interval (default: 30 seconds) plus a grace period. The green pool
then picks them up and replays them from event history.

To facilitate this, configure a conservative drain timeout:

```bash
# SIGTERM with a generous drain timeout allows long-running workflows
# to complete naturally. Workflows that don't finish are reclaimed.
kill -TERM $(pgrep cleat-worker-v1)

# After the worker exits, the reaper in the green pool handles the rest
```

**Option B: Pre-drain with sticky worker removal**

Before starting the drain, reset the `sticky_worker_id` for instances pinned
to the old workers. This allows the green pool to claim them immediately:

```sql
UPDATE workflow_instances
SET sticky_worker_id = NULL
WHERE sticky_worker_id IN (
    SELECT DISTINCT assigned_to
    FROM workflow_instances
    WHERE assigned_to LIKE 'blue-worker-%'
);
```

After this, the green pool can claim these instances on the next claim cycle.

### Step 5: Remove old workers

Once all blue workers report zero running workflows, remove them:

```bash
# If using direct process management:
# The workers may already have exited after SIGTERM.
# Verify no blue workers remain:
pgrep cleat-worker-v1 || echo "Blue pool fully drained"

# If using Kubernetes:
kubectl delete deployment cleat-worker-blue

# If using systemd:
systemctl stop cleat-worker-blue
systemctl disable cleat-worker-blue
```

After removal, only the green pool (new version) is running:

```bash
# Verify the green pool is healthy
curl http://green-worker-1:8080/health
# Response: {"status":"ok","workflows_running":42,...}
```

### Step 6: Verify the deployment

```bash
# Check worker version
curl http://green-worker-1:8080/health | jq '.version'

# Verify workflows are progressing
curl http://green-worker-1:8080/metrics | grep cleat_workflows_completed_total

# Check no instances are stuck
curl http://green-worker-1:8080/api/workflows?status=running
```

## Rollback procedure

If the new worker version has a problem, roll back by reversing the blue/green
roles:

### Step 1: Restart the old worker binary

Start new instances of the previous worker version. These are the "blue"
workers coming back online:

```bash
# Start the old binary (or deploy old container image)
cleat-worker-v1 --db "$DATABASE_URL" --concurrency 20 --api-addr :8080
```

In Kubernetes:

```bash
kubectl scale deployment cleat-worker-blue --replicas=3
```

### Step 2: Drain the new pool

Use the admin API or SIGTERM to drain the green pool:

```bash
# Drain each green worker
for worker in green-worker-1 green-worker-2 green-worker-3; do
    curl -X POST "http://$worker:8080/api/admin/drain"
done

# Or send SIGTERM to the green pool
pkill -TERM cleat-worker-v2
```

### Step 3: Wait for drain

Monitor the green workers until all in-flight workflows have been reclaimed
or completed:

```bash
while curl -s http://green-worker-1:8080/health | grep -q '"workflows_running":[1-9]'; do
    sleep 5
done
```

The reaper in the blue pool will reclaim any instances that the green workers
release (either by completing or by heartbeat expiry).

### Step 4: Remove the new pool

```bash
# Remove the green deployment
kubectl delete deployment cleat-worker-green
```

### Step 5: Verify

```bash
curl http://blue-worker-1:8080/health
# Response: {"status":"ok","workflows_running":42,...}
```

## Compatibility during coexistence

### Worker binary compatibility

During the blue/green coexistence window, both worker versions connect to the
same database and execute workflows. The following must hold:

| Component | Compatibility requirement |
|-----------|--------------------------|
| Worker binary | Same major version. Minor/patch differences are safe within a major version. |
| Database schema | Must be compatible with the oldest worker in the pool. Migrate before starting the green pool. |
| WASM modules | WASM blobs are versioned in `workflow_defs`. Each instance runs the version recorded in `def_version`. The host call interface is backward compatible within a major version. |
| CLI flags | New flags are ignored by old workers (they fail on unknown flags). Use a separate configuration for each pool if needed. |

### Mixing worker versions

During the deployment window, the two pools handle a mix of workflow instances:

- **Blue pool (old)**: executes the instances it claimed before draining,
  plus any new instances claimed before it entered drain mode. Uses the old
  binary.
- **Green pool (new)**: executes newly claimed instances. Can also claim
  instances released by the blue pool during drain (if the instance's
  heartbeat expires before the blue worker finishes).

From the database's perspective, there is no distinction between pools. Each
instance is claimed by whichever worker wins the `SELECT ... FOR UPDATE SKIP
LOCKED` race.

### New workflows during deployment

New workflow instances created during the deployment window (via API or cron)
are claimed by whichever pool has capacity. Since both pools share the same
queue:

- If the blue pool has not yet entered drain mode, blue claims new instances
- If the blue pool is draining, only the green pool claims new instances
- This is transparent to the caller -- the API response includes the
  `workflow_id`, and the caller does not need to know which pool executes it

## Kubernetes-specific considerations

### Pod lifecycle

Use preStop hooks and proper terminationGracePeriodSeconds:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cleat-worker
spec:
  template:
    spec:
      terminationGracePeriodSeconds: 120
      containers:
      - name: cleat-worker
        image: cleat/worker:1.0.0
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - |
                # Signal drain to the worker via the admin API
                curl -X POST http://localhost:8080/api/admin/drain
                # Wait for drain to complete or timeout
                sleep 60
```

### Service routing

If you route traffic to workers via a Service (e.g., for the web UI or API),
ensure the Service selector matches only the active pool:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: cleat-worker
spec:
  selector:
    app: cleat-worker
    version: green  # Only route to the active pool
```

During deployment, update the selector from `blue` to `green` after draining
confirms success, or keep a separate Service for each pool.

### Rollout strategies

For simple zero-downtime deployments, consider using a rolling update strategy
instead of blue/green if your deployment model allows it:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cleat-worker
spec:
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0  # Ensures no downtime
```

A rolling update replaces pods one at a time. Each old pod receives SIGTERM
from the kubelet, drains its workflows, and is replaced by a new pod. This is
simpler than blue/green but offers a slower rollback story (you must re-deploy
the old image).

## Monitoring during deployment

Monitor the following during a zero-downtime deployment:

```bash
# Workflow throughput (should remain steady)
curl http://green-worker-1:8080/metrics | grep cleat_workflows_started_total

# Worker health (both pools)
curl http://blue-worker-1:8080/health
curl http://green-worker-1:8080/health

# Stale instances (should be near zero)
curl http://green-worker-1:8080/metrics | grep cleat_workflows_reclaimed

# Database connection pool
curl http://green-worker-1:8080/health | jq '.database_connected'
```

## Related guides

- [Upgrading cleat](upgrading.md) -- worker binary upgrades, schema migrations,
  PostgreSQL upgrades, and rollback procedures
- [Disaster recovery](disaster-recovery.md) -- recovery from full database
  restore, RPO/RTO, and cross-region failover
- [Deploying to production](deploying-to-production.md) -- configuration,
  monitoring, health checks, and graceful shutdown
