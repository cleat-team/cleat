# Cleat Plugin Architecture

## Strategy

"Cleat is your database and workflow engine. Plugins add everything else —
blob storage, event streaming, notifications, auth providers, monitoring
integrations — all on the same database you needed anyway."

This shifts cleat from a product (durable execution) to a platform (extensible
backend infrastructure on PostgreSQL). DBOS and Temporal sell a specific
capability. Cleat sells a platform where the core is free and the ecosystem
provides the long tail of functionality.

## Plugin Model: Compile-Time Registration

Plugins are Go packages compiled into the worker binary. They register
themselves in `init()` via a central registry. This avoids Go's fragile
runtime plugin system (`plugin.Open`) and keeps the build simple: add an
import, rebuild the worker.

### Example: Installing the blob store plugin

```go
// cmd/durable-worker/main.go (user's custom worker)
package main

import (
    "github.com/rcownie/cleat/cmd/durable-worker"
    
    // Built-in plugins
    _ "github.com/rcownie/cleat/plugins/blobstore"   // S3-backed blob storage
    _ "github.com/rcownie/cleat/plugins/eventstore"   // event streams + SSE
    _ "github.com/rcownie/cleat/plugins/jobqueue"     // standalone job queue
    _ "github.com/rcownie/cleat/plugins/kvstore"      // versioned KV store
    
    // Community plugins
    _ "github.com/example/cleat-plugin-slack"         // Slack notifications
    _ "github.com/example/cleat-plugin-datadog"       // Datadog metrics export
)

func main() {
    durable_worker.Run()
}
```

No config files. No plugin manifests. Just Go imports. The worker discovers
plugins from the registry at startup.

## Plugin Interface

Every plugin implements the `Plugin` interface:

```go
// internal/plugin/plugin.go

package plugin

import (
    "context"
    "database/sql"
    "net/http"
)

// PluginInfo describes a plugin for discovery and documentation.
type PluginInfo struct {
    Name        string   // "blobstore"
    Version     string   // "0.1.0"
    Description string   // "Content-addressed blob storage with S3 backend"
    Author      string   // "cleat"
    Requires    []string // optional: "kvstore" if this plugin depends on another
}

// Plugin is the interface all plugins implement.
type Plugin interface {
    // Info returns plugin metadata.
    Info() PluginInfo

    // Init is called at worker startup. The plugin can validate its
    // configuration, run migrations, and register hooks.
    Init(ctx context.Context, env *Environment) error

    // Stop is called at worker shutdown for cleanup.
    Stop(ctx context.Context) error
}

// Environment provides plugins with access to cleat infrastructure.
type Environment struct {
    DB          *sql.DB           // PostgreSQL connection pool
    TenantID    string            // worker's tenant ID (for single-tenant mode)
    Mux         *http.ServeMux    // register HTTP handlers
    Logger      Logger            // structured logger
    Metrics     MetricsRegistry   // register prometheus metrics
    CLI         CommandRegistry   // register CLI subcommands
    Migrations  MigrationRegistry // register schema migrations
    Config      ConfigAccessor    // read plugin-specific config
}

// Optional interfaces plugins can implement for specific capabilities.

// HTTPProvider: plugin wants to register HTTP handlers.
type HTTPProvider interface {
    Plugin
    RegisterRoutes(mux *http.ServeMux) error
}

// MigrationProvider: plugin has database migrations to run.
type MigrationProvider interface {
    Plugin
    Migrations() []Migration  // up + down SQL
}

// CLIProvider: plugin adds CLI subcommands.
type CLIProvider interface {
    Plugin
    RegisterCommands(registry CommandRegistry) error
}

// MetricsProvider: plugin exports Prometheus metrics.
type MetricsProvider interface {
    Plugin
    RegisterMetrics(registry MetricsRegistry) error
}

// BackgroundWorker: plugin runs background goroutines.
type BackgroundWorker interface {
    Plugin
    Run(ctx context.Context) error  // called in a goroutine, cancel ctx to stop
}

// ConfigProvider: plugin declares its configuration schema.
type ConfigProvider interface {
    Plugin
    ConfigSchema() ConfigSchema  // JSON Schema for validation
}

// HostFunctionProvider: plugin adds WASM host function imports.
type HostFunctionProvider interface {
    Plugin
    RegisterHostFunctions(builder HostModuleBuilder) error
}
```

## Plugin Registry

```go
// internal/plugin/registry.go

package plugin

import (
    "fmt"
    "sync"
)

var (
    registry   = make(map[string]func() Plugin)
    registryMu sync.Mutex
)

// Register registers a plugin constructor. Called in init().
func Register(constructor func() Plugin) {
    registryMu.Lock()
    defer registryMu.Unlock()
    
    p := constructor()
    info := p.Info()
    if _, exists := registry[info.Name]; exists {
        panic(fmt.Sprintf("plugin %q registered twice", info.Name))
    }
    registry[info.Name] = constructor
}

// LoadAll instantiates all registered plugins and calls Init on each,
// respecting dependency order.
func LoadAll(ctx context.Context, env *Environment) ([]Plugin, error) {
    // Topological sort by Requires, then Init each in order
}

// List returns metadata for all registered plugins (for /api/plugins endpoint).
func List() []PluginInfo
```

## Example: Blob Store Plugin

```go
// plugins/blobstore/plugin.go

package blobstore

import (
    "github.com/rcownie/cleat/internal/plugin"
)

func init() {
    plugin.Register(func() plugin.Plugin {
        return &BlobStorePlugin{}
    })
}

type BlobStorePlugin struct {
    db     *sql.DB
    config BlobStoreConfig
}

func (p *BlobStorePlugin) Info() plugin.PluginInfo {
    return plugin.PluginInfo{
        Name:        "blobstore",
        Version:     "0.1.0",
        Description: "Content-addressed blob storage with S3/GCS backend",
        Author:      "cleat",
    }
}

func (p *BlobStorePlugin) Init(ctx context.Context, env *plugin.Environment) error {
    p.db = env.DB
    
    // Read config
    if err := env.Config.Unmarshal("blobstore", &p.config); err != nil {
        return err
    }
    
    // Run migrations (blob_index, blob_content tables)
    // Register HTTP routes (/blobs/*)
    // Start background TTL cleanup goroutine
    return nil
}

// HTTPProvider
func (p *BlobStorePlugin) RegisterRoutes(mux *http.ServeMux) error {
    mux.HandleFunc("PUT /blobs/{key}", p.handleUpload)
    mux.HandleFunc("GET /blobs/{key}", p.handleDownload)
    mux.HandleFunc("DELETE /blobs/{key}", p.handleDelete)
    mux.HandleFunc("GET /blobs", p.handleList)
    mux.HandleFunc("GET /blobs/{key}/metadata", p.handleMetadata)
    return nil
}

// MigrationProvider
func (p *BlobStorePlugin) Migrations() []plugin.Migration {
    return []plugin.Migration{
        {Version: 1, Up: createBlobIndexSQL, Down: dropBlobIndexSQL},
    }
}

// BackgroundWorker
func (p *BlobStorePlugin) Run(ctx context.Context) error {
    return p.runTTLCleanup(ctx) // delete expired blobs every hour
}
```

## Worker Startup Sequence

```go
// cmd/durable-worker/main.go (simplified)

func Run() {
    // 1. Parse flags
    // 2. Open database
    // 3. Create plugin environment
    env := &plugin.Environment{
        DB:       db,
        Mux:      http.NewServeMux(),
        Logger:   logger,
        Metrics:  metricsRegistry,
        Config:   configAccessor,
        Migrations: migrationRegistry,
    }
    
    // 4. Load all plugins (imported via init() registration)
    plugins, err := plugin.LoadAll(ctx, env)
    
    // 5. Run migrations (core + all plugins, ordered by dependency)
    migrationRunner.Run(ctx, db, env.Migrations.All())
    
    // 6. Register core routes (workflows, schedules, health)
    registerCoreRoutes(env.Mux, store)
    
    // 7. Register plugin routes (after core, so plugins can't shadow)
    for _, p := range plugins {
        if hp, ok := p.(plugin.HTTPProvider); ok {
            hp.RegisterRoutes(env.Mux)
        }
    }
    
    // 8. Start background workers
    for _, p := range plugins {
        if bw, ok := p.(plugin.BackgroundWorker); ok {
            go bw.Run(ctx)
        }
    }
    
    // 9. Start HTTP server
    server := &http.Server{Handler: authMiddleware(env.Mux)}
    go server.ListenAndServe()
    
    // 10. Start dispatch loop
    worker.Run(ctx)
}
```

## What Plugins Can Do

| Capability | Interface | Example |
|-----------|-----------|---------|
| Add HTTP endpoints | `HTTPProvider` | `/blobs/*`, `/events/*`, `/jobs/*` |
| Add DB tables + migrations | `MigrationProvider` | `blob_index`, `event_stream` |
| Add CLI commands | `CLIProvider` | `cleat blob upload`, `cleat events tail` |
| Add Prometheus metrics | `MetricsProvider` | `cleat_blobs_uploaded_total` |
| Run background tasks | `BackgroundWorker` | TTL cleanup, notification delivery |
| Add WASM host functions | `HostFunctionProvider` | `cleat_slack_notify`, `cleat_s3_upload` |
| Declare config schema | `ConfigProvider` | S3 bucket name, Slack webhook URL |
| Depend on other plugins | `Info().Requires` | `slack-notify` requires `kvstore` |

## Plugin Catalog (Built-In + Community)

### Tier 1: Core Plugins (maintained in cleat repo)

| Plugin | What it does | Effort |
|--------|-------------|--------|
| `blobstore` | S3/GCS-backed blob storage with content-addressed dedup, metadata queries, TTLs | 2-3 weeks |
| `eventstore` | Append-only event streams with SSE, queryable by time range | 1 week |
| `jobqueue` | Standalone job queue (thin API over workflow_instances) | 0.5 weeks |
| `kvstore` | Versioned JSONB key-value store with optimistic concurrency | 0.5 weeks |
| `notifications` | Webhook delivery, retry, and delivery status tracking | 1 week |

### Tier 2: Community Plugins (maintained by others)

| Plugin | What it does |
|--------|-------------|
| `slack-notify` | Send Slack messages from workflows via `h.SlackNotify(channel, text)` |
| `email-send` | Send email (SMTP/SendGrid) from workflows |
| `datadog-export` | Export workflow metrics to Datadog |
| `pagerduty-alert` | Create PagerDuty incidents from workflow failures |
| `kafka-connect` | Publish events to Kafka, consume Kafka messages as signals |
| `s3-upload` | WASM host function for direct S3 upload from workflow code |
| `oauth-provider` | Add OAuth2/OIDC authentication (Google, GitHub, Okta) |
| `rate-limiter` | Per-tenant rate limiting on API endpoints |
| `audit-log` | Comprehensive audit trail of all API access |
| `webhook-ingest` | Receive webhooks and deliver as workflow signals |
| `scheduled-backup` | pg_dump-based backup to S3 on a cron schedule |
| `feature-flags` | Feature flag evaluation with rules and gradual rollout |

## Strategic Implications

### 1. Cleat becomes a platform, not a product

Temporal sells a workflow engine. DBOS sells a workflow engine. Cleat sells a
platform where the workflow engine is the core but plugins provide the long
tail. This is the same strategy that made WordPress, VSCode, and Caddy
successful — the core is free and useful, the ecosystem makes it essential.

### 2. The PostgreSQL dependency becomes an asset

Every plugin uses the same PostgreSQL database, the same auth middleware, the
same tenant isolation, the same metrics. The database isn't a cost of the
workflow engine — it's the foundation of an entire backend platform. "You
needed PostgreSQL anyway. Now it does everything."

### 3. Community growth through plugin authorship

Writing a cleat plugin is ~200 lines of Go. The bar is low. As plugins
accumulate, cleat becomes useful to people who don't even need durable
execution — they adopt for the blob store or event streams, and workflows
are a natural extension.

### 4. Commercial model: open-source core, paid plugins

The core workflow engine is open source. The core plugins (blobstore,
eventstore, etc.) are open source. Premium plugins (advanced auth, compliance,
enterprise connectors) can be commercial. This is the GitLab / Mattermost
model — open-source for adoption, paid for enterprise features.

### 5. Defensibility through ecosystem

Temporal can't easily replicate a plugin ecosystem — they're a centralized
product. DBOS could, but they've positioned as a focused execution engine.
If cleat has 30+ plugins solving real problems, switching costs increase.

## Risks

### Plugin quality variance
Community plugins will vary in quality. Unlike VS Code (which has a review
process), cleat plugins are Go code compiled into the worker — a bad plugin
can crash the process.

**Mitigation**: Plugin isolation via `recover()` in the plugin loader. Health
endpoint reports plugin status. Sandboxing via separate goroutines with panic
recovery.

### API stability burden
Once plugins exist, the `Plugin` interface and `Environment` struct become
public API. Breaking changes break plugins.

**Mitigation**: Version the plugin API (`internal/plugin/v1`, `v2`). Start
with `v1alpha1` to signal instability. Only promote to `v1` after 3+ plugins
exist and the interface has proven itself.

### Naming conflicts
Two plugins might both want `/events` or `cleat_events_total`.

**Mitigation**: Plugin route prefixing (`/plugins/blobstore/blobs`). Metric
name validation at registration time (reject duplicates). First-registered
wins with a warning.

### Dependency hell
If plugin A requires plugin B v1.2.0 and plugin C requires B v1.0.0, you have
a Go module conflict.

**Mitigation**: Plugins live in the same Go module (no version conflicts).
Community plugins specify minimum cleat version, not plugin versions.
Dependencies are "soft" — a plugin checks for its dependency at Init time
and returns a clear error if missing.

## Implementation Plan

| Week | Deliverable |
|------|-------------|
| 1 | `internal/plugin/` — interface, registry, loader with dependency ordering, `Environment` struct |
| 2 | `plugins/blobstore/` — first plugin, validates the interface design |
| 3 | `plugins/eventstore/`, `plugins/kvstore/` — second + third plugins, proves interface generality |
| 4 | `plugins/jobqueue/`, `plugins/notifications/` — fills out the built-in catalog |
| 5 | Plugin documentation, example plugin template, `cleat plugin new` scaffolding CLI |
| 6 | Plugin health dashboard in web UI, `/api/plugins` endpoint for discovery |

## Verification

- `go build ./cmd/durable-worker/` with all built-in plugins imported: compiles
- `go build ./cmd/durable-worker/` with zero plugins imported: compiles, core works
- Add a plugin that panics in Init: worker starts, panicking plugin is disabled, health endpoint reports it
- Two plugins declare the same route: second registration is rejected with clear error
- Plugin adds a migration: migration runs, table exists, second startup is idempotent
