# Plugin Build Order

Build plugins in this order. Each adds API surface, builds the platform
narrative, and validates the plugin API against a different pattern.

## Phase 1: Core Platform (makes cleat a complete backend)

### 1. blobstore ✅
Content-addressed blob storage, S3 + memory backends, liveness tracking.
Validates: HasMigrations, HasRoutes, HasBackground, HasHostFunctions (idempotent).

### 2. kvstore
Versioned JSONB key-value with optimistic concurrency. 30-minute build.
Thin API over a `kv_store` table.
Validates: HasRoutes (prefix listing), zero host functions needed.

### 3. eventstore  
Append-only event streams with SSE. 1-day build.
Thin API over an `event_stream (tenant_id, stream_id, sequence)` table.
Validates: HasRoutes (SSE streaming), HasBackground (no TTL needed).

### 4. jobqueue
Standalone job queue. 30-minute build.
Thin API over the existing workflow_instances table with a special `task_queue`.
Validates: HasCommands (CLI for enqueuing), reuses engine infrastructure.

### 5. notifications
Webhook delivery with retry and delivery tracking. 1-day build.
Stores webhook configs per tenant, delivers HTTP POST, records delivery status.
Validates: HasBackground (retry loop), HasRoutes (webhook CRUD).

### 6. scheduler (cron for user code, generalizes existing scheduler)
Generalizes the built-in cron to user-managed schedules. 1-day build.
Validates: HasCommands (schedule add/list/delete), HasRoutes (schedule API).

## Phase 2: Auth & Security (makes it multi-tenant ready)

### 7. rate-limiter
Per-tenant rate limiting using `ulule/limiter`. 1-day build.
Validates: HasMiddleware, adaptation of existing OSS library.

### 8. oauth-provider
OAuth2/OIDC authentication (Google, GitHub, Okta). 2-day build.
Validates: HasMiddleware, HasRoutes (callback URLs), HasMigrations (session table).

### 9. audit-log
Comprehensive audit trail of all API access. 1-day build.
Validates: HasMiddleware, HasRoutes (query API), HasBackground (retention cleanup).

## Phase 3: External Integrations (makes it extensible)

### 10. slack-notify
Send Slack messages from workflows. 30-minute build.
Validates: HasHostFunctions, adaptation of `slack-go/slack`.

### 11. webhook-ingest
Receive webhooks, deliver as workflow signals. 1-day build.
Validates: HasRoutes (inbound webhook), HasHostFunctions (signal delivery).

### 12. kafka-connect
Publish events to Kafka, consume messages as signals. 2-day build.
Validates: HasBackground (consumer loop), HasHostFunctions (produce), adaptation of `segmentio/kafka-go`.

### 13. scheduled-backup
pg_dump to S3 on a cron schedule. 1-day build.
Validates: HasBackground, HasCommands (CLI for manual backup/restore).

## Phase 4: Observability & Operations

### 14. pagerduty-alert
Create PagerDuty incidents from workflow failures. 1-day build.
Validates: HasHealth (alert health), HasHostFunctions.

### 15. datadog-export
Export workflow metrics to Datadog. 1-day build.
Validates: HasBackground (periodic export), adaptation of Datadog API.

### 16. feature-flags
Feature flag evaluation with rules and gradual rollout. 1-day build.
Validates: HasRoutes (flag CRUD), HasHostFunctions (evaluate from workflows).

## Effort Summary

| Phase | Plugins | Total effort |
|-------|---------|-------------|
| 1: Core | 5 remaining | ~5 days |
| 2: Auth | 3 | ~4 days |
| 3: Ext | 4 | ~5 days |
| 4: Obs | 3 | ~3 days |
| **Total** | **15 remaining** | **~17 days** |

All 15 remaining plugins are thin (30 min - 2 days each) because they're
adapters over existing OSS libraries or thin APIs over PostgreSQL tables —
the plugin API itself is 2 methods + optional interfaces.

## Build Strategy

Write plugins in Phase 1 order. After each plugin, ask: did the API get in
the way? If yes, fix the API before the next plugin. The only plugin already
committed is blobstore, so we have 1 migration burden if the API needs to
change.

Don't batch plugins into large subagent runs. Write one at a time, review
the API fit, then move to the next. Speed up once we've done 5 without
API changes.
