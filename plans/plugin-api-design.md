# Plugin API Design: Adapting Existing Open-Source Projects

## Principle: Thin Wrapper, Not Framework

The plugin API should be the minimum possible surface that lets an existing
Go library run inside the cleat worker. It should NOT be a framework that
forces plugins into cleat's coding patterns.

A good plugin is a thin adapter: import the OSS library, wire it to cleat's
infrastructure in `Init()`, register routes or host functions, done. If
adapting an existing library takes more than ~100 lines of glue, the plugin
API is wrong.

## Anti-Principles

Things the plugin API must NOT do:

1. **Don't force a config format.** The OSS library might use Viper, env vars,
   its own YAML structure, or a builder pattern. Give the plugin its config
   section as raw bytes and let it parse however it wants.

2. **Don't force an HTTP router.** The OSS library might bring its own
   `http.Handler` (e.g., an OAuth proxy, a metrics exporter, a webhook
   receiver). Give the plugin a place to mount handlers — don't make it
   use cleat's internal router.

3. **Don't force a logger.** The OSS library might use `zap`, `zerolog`,
   `logrus`, or `slog`. Give the plugin a standard `*slog.Logger` and let
   it adapt to whatever the library needs.

4. **Don't force a metrics library.** The OSS library might already register
   its own Prometheus metrics via the global `prometheus.MustRegister()`.
   Don't make it use a cleat metrics abstraction — just expose the `/metrics`
   endpoint it can register on.

5. **Don't force a database abstraction.** The plugin might need raw SQL for
   a migration, or it might use an ORM, or it might not use the database at
   all. Give it `*sql.DB` and get out of the way.

6. **Don't require optional interfaces.** A plugin that just adds a host
   function shouldn't need to implement `HTTPProvider` with empty methods.
   Optional interfaces should be truly optional — the loader checks for them,
   the plugin doesn't declare them.

7. **Don't invent a plugin SDK that wraps standard library types.** If the
   standard library has `*sql.DB`, `*http.ServeMux`, `*slog.Logger` — use
   those. Don't create `cleat.DB`, `cleat.Router`, `cleat.Logger` wrappers.
   Every wrapper is one more thing a plugin author has to learn.

## The Minimal Plugin Interface

```go
// Plugin is the only required interface.
type Plugin interface {
    Info() PluginInfo
    Init(ctx context.Context, env *Environment) error
}
```

`Stop` is optional. If a plugin needs cleanup, it implements `Stoppable`:

```go
type Stoppable interface {
    Stop(ctx context.Context) error
}
```

That's it. Two methods required. Everything else is optional and discovered
by the loader via type assertion.

## Environment: Raw Infrastructure Access

```go
type Environment struct {
    // DB is the PostgreSQL connection pool. Plugins can create tables,
    // run queries, start transactions — same access as core cleat code.
    DB *sql.DB

    // Mux is the root HTTP mux. Plugins mount handlers directly.
    // They can also mount subrouters (chi, gin, etc.) if they want.
    // Plugin routes are namespaced by convention: /plugins/<name>/...
    Mux *http.ServeMux

    // Config is the plugin's raw configuration section, typically
    // parsed from cleat.yaml or the worker config file.
    Config json.RawMessage

    // Logger is a standard library slog.Logger. Plugins can adapt
    // it to zap/zerolog/logrus if their OSS dependency needs that.
    Logger *slog.Logger

    // TenantID is the worker's tenant for single-tenant mode.
    // Empty in multi-tenant mode.
    TenantID string

    // Stop is a channel that closes when the worker is shutting down.
    // Plugins can select on this instead of using context.
    Done <-chan struct{}
}
```

No wrappers. No abstractions. Raw standard library types plus a few cleat-
specific fields (TenantID, Done). A plugin author who knows Go already knows
this API.

## Optional Interfaces (Discovered by Loader)

The loader type-asserts each plugin against these interfaces. If the plugin
implements it, the loader calls it. If not, nothing happens. Zero boilerplate.

```go
// HasMigrations: plugin needs database tables.
type HasMigrations interface {
    Migrations() []Migration
}

type Migration struct {
    Version int
    Up      string   // SQL to run
    Down    string   // SQL to roll back (optional, for cleat plugin reset)
}

// HasRoutes: plugin exposes HTTP endpoints.
type HasRoutes interface {
    RegisterRoutes(mux *http.ServeMux) error
}

// HasCommands: plugin adds CLI subcommands.
type HasCommands interface {
    RegisterCommands(cmd *cobra.Command) error  // or stdlib flag.FlagSet
}

// HasBackground: plugin runs a background goroutine.
type HasBackground interface {
    Run(ctx context.Context) error
}

// HasHostFunctions: plugin adds WASM imports callable from workflows.
type HasHostFunctions interface {
    RegisterHostFunctions(builder HostModuleBuilder) error
}

// HasHealth: plugin reports its health status.
type HasHealth interface {
    Health() error  // nil = healthy, error = unhealthy with reason
}
```

## Concrete Adaptation Examples

### Example 1: Slack Notifications (adapting an existing Slack library)

Existing OSS library: `github.com/slack-go/slack` (4K+ stars, mature)

```go
package slacknotify

import (
    "github.com/rcownie/cleat/internal/plugin"
    "github.com/slack-go/slack"
)

func init() { plugin.Register(func() plugin.Plugin { return &Plugin{} }) }

type Plugin struct {
    client *slack.Client
    db     *sql.DB
}

type config struct {
    BotToken string `json:"bot_token"`
}

func (p *Plugin) Info() plugin.PluginInfo {
    return plugin.PluginInfo{
        Name:        "slack-notify",
        Version:     "0.1.0",
        Description: "Send Slack messages from workflows",
    }
}

func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
    var cfg config
    if err := json.Unmarshal(env.Config, &cfg); err != nil {
        return fmt.Errorf("slack-notify: invalid config: %w", err)
    }
    p.client = slack.New(cfg.BotToken)
    p.db = env.DB
    return nil
}

func (p *Plugin) RegisterHostFunctions(builder plugin.HostModuleBuilder) error {
    builder.Register("slack_send_message", func(
        ctx context.Context,
        m api.Module,
        channelPtr, channelLen uint32,
        textPtr, textLen uint32,
    ) uint64 {
        // Read channel and text from WASM memory
        channel := plugin.ReadWasmString(m, channelPtr, channelLen)
        text := plugin.ReadWasmString(m, textPtr, textLen)
        
        // Use the existing OSS library — no wrapping, just call it
        _, _, err := p.client.PostMessageContext(ctx, channel, 
            slack.MsgOptionText(text, false))
        
        if err != nil {
            return plugin.EncodeError(err)
        }
        return plugin.EncodeOK()
    })
    return nil
}
```

The adaptation is 60 lines. The OSS library is used directly. No wrapping.
No abstraction layer. The plugin is just glue between cleat's WASM host
function interface and slack-go's API.

### Example 2: Rate Limiter (adapting ulule/limiter)

Existing OSS library: `github.com/ulule/limiter/v3` (2K+ stars)

```go
package ratelimiter

import (
    "net/http"
    "github.com/rcownie/cleat/internal/plugin"
    "github.com/rcownie/cleat/internal/auth"
    "github.com/ulule/limiter/v3"
    "github.com/ulule/limiter/v3/drivers/store/memory"
)

func init() { plugin.Register(func() plugin.Plugin { return &Plugin{} }) }

type Plugin struct {
    limiter *limiter.Limiter
}

func (p *Plugin) Info() plugin.PluginInfo {
    return plugin.PluginInfo{
        Name: "rate-limiter", Version: "0.1.0",
        Description: "Per-tenant rate limiting",
    }
}

func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
    // Use the OSS library exactly as documented
    store := memory.NewStore()
    rate := limiter.Rate{Period: 1 * time.Minute, Limit: 1000}
    p.limiter = limiter.New(store, rate)
    return nil
}

// Plugin mounts itself as HTTP middleware
func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
    // This is a bit awkward — we need middleware, not a route.
    // This reveals a gap: plugins need a way to inject middleware.
    // See "Open Questions" below.
    return nil
}
```

### Example 3: Kafka Connect (adapting segmentio/kafka-go)

Existing OSS library: `github.com/segmentio/kafka-go` (7K+ stars)

```go
package kafkaconnect

import (
    "github.com/segmentio/kafka-go"
    "github.com/rcownie/cleat/internal/plugin"
)

type Plugin struct {
    writer *kafka.Writer
    reader *kafka.Reader
}

func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
    var cfg struct {
        Brokers []string `json:"brokers"`
        Topic   string   `json:"topic"`
    }
    json.Unmarshal(env.Config, &cfg)
    
    p.writer = &kafka.Writer{
        Addr:  kafka.TCP(cfg.Brokers...),
        Topic: cfg.Topic,
    }
    return nil
}

func (p *Plugin) Stop(ctx context.Context) error {
    return p.writer.Close()  // graceful drain
}

// Host function: workflow can publish to Kafka
func (p *Plugin) RegisterHostFunctions(builder plugin.HostModuleBuilder) error {
    builder.Register("kafka_publish", func(/* WASM params */) uint64 {
        // Read message from WASM memory, call p.writer.WriteMessages
        return plugin.EncodeOK()
    })
    return nil
}

// Background: consume Kafka messages and deliver as workflow signals
func (p *Plugin) Run(ctx context.Context) error {
    for {
        msg, err := p.reader.ReadMessage(ctx)
        if err != nil { break }
        // Deliver as signal to target workflow
    }
    return nil
}
```

## What These Examples Reveal About the API

### 1. Host functions need WASM memory helpers

Every plugin that adds host functions needs to read/write strings from WASM
linear memory. The plugin package should export these helpers:

```go
package plugin

// ReadWasmString reads a UTF-8 string from WASM linear memory.
func ReadWasmString(m api.Module, ptr, len uint32) string

// WriteWasmString writes a UTF-8 string to WASM linear memory.
func WriteWasmString(m api.Module, ptr, maxLen uint32, s string) uint32

// EncodeOK returns a packed i64 representing success (errCode=0).
func EncodeOK() uint64

// EncodeError returns a packed i64 representing an error.
func EncodeError(err error) uint64
```

### 2. Plugins need middleware injection, not just route registration

The current `RegisterRoutes(mux *http.ServeMux)` only allows adding routes.
But plugins like rate limiters, auth providers, and audit loggers need to
wrap the entire handler chain. Solution: add a `HasMiddleware` interface:

```go
type HasMiddleware interface {
    // Middleware returns a middleware that wraps the entire API handler.
    // Multiple plugins' middleware are composed in registration order.
    Middleware(next http.Handler) http.Handler
}
```

### 3. Plugins need tenant context, not just tenant ID

The `Environment.TenantID` field gives the worker's tenant. But for
multi-tenant plugins, each HTTP request carries a different tenant (from the
auth middleware). Plugins need access to the request-scoped tenant:

```go
package plugin

// TenantFromContext extracts the tenant ID from a request context.
// Returns uuid.Nil if no tenant is authenticated.
func TenantFromContext(ctx context.Context) uuid.UUID
```

### 4. Some plugins only need a subset of Environment

A plugin that only adds a host function doesn't need `*sql.DB`, `*http.ServeMux`,
or `*slog.Logger`. It needs the WASM module builder and config. The Environment
struct is a superset — plugins ignore what they don't need. This is fine.
A large struct is better than requiring plugins to implement interfaces for
things they don't use.

## What Should NOT Be in the Plugin API

| Don't include | Why | Alternative |
|---------------|-----|-------------|
| `cleat.DB` wrapper | Makes OSS libraries that use `*sql.DB` harder to adapt | Raw `*sql.DB` |
| `cleat.Config` struct | Forces config format. Libraries have their own config patterns | `json.RawMessage` |
| `cleat.Logger` interface | Forces logger abstraction. zap/zerolog/logrus users hate this | `*slog.Logger` (stdlib) |
| `cleat.Metrics` wrapper | Prometheus global registry already exists. Don't wrap it | Let plugins call `prometheus.MustRegister()` |
| Plugin manifest file | Another config format to learn. Go imports are the manifest | `init()` registration |
| Plugin version negotiation | Premature complexity. Let it fail at compile time | Go module versions |
| Plugin sandboxing | Premature for v1. Trust plugin code (it's compiled in) | N/A for now |
| Hot reload | Massive complexity for marginal value | Restart the worker |

## Open Questions

### Q1: How does plugin config reach the worker?

Option A: Single `cleat.yaml` with plugin sections:
```yaml
plugins:
  blobstore:
    backend: s3
    bucket: my-bucket
  slack-notify:
    bot_token: xoxb-...
```

Option B: `--plugin-config slack-notify=config.yaml` flag per plugin.

Option C: Environment variables: `CLEAT_PLUGIN_SLACK_BOT_TOKEN=xoxb-...`

**Recommendation**: Option A for file-based config, Option C for secrets.
Secrets in config files are bad practice. API tokens should be env vars
or a secret manager.

### Q2: How are plugin migrations versioned and ordered?

Plugin A has migration v1, v2. Plugin B has migration v1. The migration
runner needs to track which migrations ran for which plugins.

```sql
CREATE TABLE plugin_migrations (
    plugin_name  TEXT NOT NULL,
    version      INTEGER NOT NULL,
    applied_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (plugin_name, version)
);
```

Core cleat migrations run first, then plugins in dependency order.

### Q3: Can plugins depend on other plugins?

Yes, via `PluginInfo.Requires []string`. The loader topologically sorts
plugins before calling `Init()`. A plugin that requires another gets the
dependency's `Init()` called first.

But: plugins can't directly call each other's APIs at compile time (that
would create import cycles). They can communicate via:
- Shared database tables (plugin B reads plugin A's table)
- HTTP (plugin B calls plugin A's routes)
- Host functions (workflow code orchestrates both plugins)

### Q4: What happens when a plugin panics?

The loader wraps `Init()` in a `recover()`. A panicking plugin is disabled
and reported on the `/healthz` endpoint with details. Other plugins and the
core worker continue running. A plugin that panics in `Run()` has its
goroutine restarted once, then disabled.

### Q5: How does plugin discovery work for the web UI?

`GET /api/plugins` returns JSON with all registered plugins, their versions,
health status, and documentation links. The web UI can render a plugin
catalog from this endpoint.

### Q6: Should we support WASM plugins (plugins compiled to WASM)?

Long-term, yes — it would allow plugins in any language. But for v1, Go
compile-time plugins are simpler, type-safe, and sufficient. WASM plugins
can be added later without breaking the Go plugin API (both implement
the same `Plugin` interface, just loaded differently).

## Design Validation: Can These Real Projects Be Adapted?

| OSS Project | Stars | What it does | Adaptation difficulty | Plugin lines |
|-------------|-------|-------------|----------------------|-------------|
| `slack-go/slack` | 4K | Slack API client | Trivial — wrap in host function | ~60 |
| `segmentio/kafka-go` | 7K | Kafka client | Easy — Init/Stop lifecycle, host function for produce, Run for consume | ~150 |
| `ulule/limiter` | 2K | Rate limiter | Easy — Middleware injection | ~80 |
| `oauth2-proxy/oauth2-proxy` | 8K | OAuth reverse proxy | Medium — needs session store, tenant mapping | ~200 |
| `casbin/casbin` | 17K | Authorization | Easy — Middleware, per-tenant policies in DB | ~120 |
| `go-acme/lego` | 7K | Let's Encrypt | Medium — TLS config on HTTP server | ~150 |
| `google/go-github` | 10K | GitHub API | Trivial — Host function wrapping API calls | ~50 |
| `prometheus/client_golang` | 5K | Prometheus metrics | Trivial — Global registry, env.Mux handles /metrics | ~30 |
| `grafana/grafana-plugin-sdk-go` | 200 | Grafana data source | Medium — Would need a cleat-to-Grafana bridge | ~300 |
| `nats-io/nats.go` | 5K | NATS client | Easy — Same pattern as Kafka | ~150 |
| `redis/go-redis` | 20K | Redis client | Easy — Init/Stop, host functions for cache ops | ~100 |
| `minio/minio-go` | 2K | S3 client | Easy — Used by blobstore plugin internally | ~200 |

Every one of these is adaptable in under 300 lines. Most under 150. The
plugin API doesn't need to be complex — it just needs to get out of the way.

## Recommendation

Ship the plugin API with:
1. The `Plugin` interface (2 methods)
2. The `Environment` struct (6 fields, all raw stdlib types)
3. Six optional interfaces (`HasMigrations`, `HasRoutes`, `HasCommands`,
   `HasBackground`, `HasHostFunctions`, `HasHealth`)
4. `HasMiddleware` for request wrapping
5. WASM memory helpers (`ReadWasmString`, `WriteWasmString`, `EncodeOK`,
   `EncodeError`)
6. `TenantFromContext` for multi-tenant request handling
7. `plugin_migrations` table for tracking per-plugin schema versions

That's it. ~300 lines of Go interfaces and a startup sequence. No framework.
No SDK. Just infrastructure access.
