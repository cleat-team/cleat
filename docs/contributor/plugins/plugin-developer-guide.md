# Plugin Developer's Guide

A cleat plugin is a Go package compiled into the worker binary. It adds
tables, HTTP endpoints, workflow functions, and background workers — all
sharing cleat's PostgreSQL, auth, tenant isolation, and observability.

This guide captures everything learned from building the reference blobstore
plugin.

## Quick Start: A Minimal Plugin

```go
package helloworld

import "github.com/cleat-team/cleat/internal/plugin"

func init() {
    plugin.Register(plugin.PluginInfo{
        Name:        "hello-world",
        Version:     "0.1.0",
        Description: "A minimal example plugin",
    }, func() plugin.Plugin {
        return &Plugin{}
    })
}

type Plugin struct {
    db   *sql.DB
}

func (p *Plugin) Info() plugin.PluginInfo {
    return plugin.PluginInfo{
        Name: "hello-world", Version: "0.1.0",
        Description: "A minimal example plugin",
    }
}

func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
    p.db = env.DB
    return nil
}
```

That's it. Two required methods: `Info()` and `Init()`. Everything else is
optional — the loader discovers capabilities via type assertion.

## The Plugin Lifecycle

```
1. init()            register(PluginInfo, constructor)
2. Discover()        constructor called, instance created
3. RunMigrations()   HasMigrations.Migrations() run in version order
4. InitAll()         Init(env) called on each plugin
5. RegisterRoutes()  HasRoutes.RegisterRoutes(mux) called
6. Middleware chain   HasMiddleware.Middleware(next) composed
7. Background start  HasBackground.Run(ctx) in goroutine
8. Host funcs reg    HasHostFunctions.RegisterHostFunctions(scope)
9. Worker starts     dispatch loop begins
10. Shutdown         ctx cancelled → Stop() called if Stoppable
```

Key: migrations run BEFORE Init. Your `Init()` can query tables your
migrations created.

## Registration

`plugin.Register()` takes two arguments:

```go
plugin.Register(
    plugin.PluginInfo{
        Name:        "my-plugin",     // unique, no "/" or null bytes
        Version:     "0.1.0",         // semver
        Description: "What it does",  // shown in /api/plugins
        Author:      "optional",
        Requires:    []string{"other-plugin"},  // topological sort
    },
    func() plugin.Plugin {
        return &MyPlugin{}  // constructor — allocate nothing
    },
)
```

The constructor should allocate nothing heavy. `Init()` does the real setup.

## Environment

`Environment` gives you raw infrastructure access:

```go
type Environment struct {
    DB     *sql.DB          // PostgreSQL connection pool
    Mux    *http.ServeMux   // register HTTP routes
    Config []byte           // your plugin's config section (JSON/YAML/whatever)
    Logger *slog.Logger     // structured logger
    Done   <-chan struct{}  // closes on shutdown
}
```

No wrappers, no abstractions. Use `env.DB` for `database/sql` queries.
Use `env.Mux` to register `http.HandlerFunc`s.

## Optional Interfaces

### HasMigrations — database tables

```go
func (p *Plugin) Migrations() []plugin.Migration {
    return []plugin.Migration{{
        Version: 1,
        Up: `CREATE TABLE IF NOT EXISTS my_table (
            tenant_id UUID NOT NULL,  -- ALWAYS include tenant_id
            -- your columns
            PRIMARY KEY (tenant_id, ...)
        );`,
        Down: `DROP TABLE IF EXISTS my_table;`,
    }}
}
```

Rules:
- Every table MUST have `tenant_id UUID NOT NULL` as the first column
- Use `IF NOT EXISTS` / `IF EXISTS` for idempotency
- Migrations run in a transaction per version
- Tracked in `plugin_migrations` table — never runs twice

### HasRoutes — HTTP endpoints

```go
func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
    mux.HandleFunc("GET /my-plugin/things", p.handleList)
    mux.HandleFunc("PUT /my-plugin/things/{id}", p.handlePut)
    return nil
}

func (p *Plugin) handleList(w http.ResponseWriter, r *http.Request) {
    // Extract tenant from auth middleware
    tid, ok := auth.TenantIDFromContext(r.Context())
    if !ok {
        http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
        return
    }
    // Query with tenant_id
    rows, _ := p.db.QueryContext(r.Context(), 
        `SELECT ... FROM my_table WHERE tenant_id = $1`, tid)
    // ...
}
```

Rules:
- Prefix routes with your plugin name: `/my-plugin/...`
- Extract tenant from `auth.TenantIDFromContext(r.Context())` — NOT from URL params
- Always filter queries by `tenant_id`

### HasHostFunctions — workflow-callable functions

```go
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
    scope.Register(plugin.FuncOptions{Name: "do_thing"}, p.doThing)
    scope.Register(plugin.FuncOptions{Name: "read_thing", Idempotent: true}, p.readThing)
    return nil
}

// PluginFunc signature: (ctx, inputJSON) -> (outputJSON, error)
func (p *Plugin) doThing(ctx context.Context, inputJSON string) (string, error) {
    // Access tenant + workflow from context
    cc := plugin.CallContextFromContext(ctx)
    if cc == nil {
        return "", fmt.Errorf("no call context")
    }
    
    // Parse input
    var input MyInput
    json.Unmarshal([]byte(inputJSON), &input)
    
    // Do work — write to DB, call external APIs, etc.
    _, err := p.db.ExecContext(ctx, 
        `INSERT INTO my_table (tenant_id, ...) VALUES ($1, ...)`, cc.TenantID)
    
    // Return JSON output
    output, _ := json.Marshal(MyOutput{...})
    return string(output), err
}
```

Rules:
- `CallContext` gives you `TenantID` and `WorkflowID` — the engine injects it
- Use `Idempotent: true` for read-only functions (S3 reads, cache lookups).
  The engine re-invokes these during replay instead of returning cached output.
- Use `Idempotent: false` (default) for side-effecting functions. The engine
  records input/output in event_history and returns cached output during replay.
- Output MUST be valid JSON — the WASM boundary expects it
- Keep output small for non-idempotent functions (it's stored in event_history)
- Return errors as `(string, error)` — the engine records the error in history

### HasMiddleware — request wrapping

```go
func (p *Plugin) Middleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Before: check rate limit, audit, etc.
        next.ServeHTTP(w, r)
        // After: record metrics
    })
}
```

Middleware is composed in registration order. Use for rate limiting, audit
logging, auth checking — anything that wraps every request.

### HasBackground — background work

```go
func (p *Plugin) Run(ctx context.Context) error {
    ticker := time.NewTicker(1 * time.Hour)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return nil  // clean shutdown
        case <-ticker.C:
            p.doPeriodicWork(ctx)
        }
    }
}
```

Rules:
- Respect `ctx.Done()` — return nil for clean shutdown
- Log errors, don't return them (they kill the goroutine)
- Don't panic — the worker recovers but disables your plugin

### HasHealth — health reporting

```go
func (p *Plugin) Health() error {
    if err := p.db.Ping(); err != nil {
        return fmt.Errorf("database unreachable: %w", err)
    }
    return nil
}
```

Return nil if healthy, error if not. Reported on `/healthz` and `/api/plugins`.

### Stoppable — cleanup

```go
func (p *Plugin) Stop(ctx context.Context) error {
    return p.client.Close()  // drain connections
}
```

Called during graceful shutdown. Not called if the process is killed.

### HasCommands — CLI subcommands

```go
func (p *Plugin) RegisterCommands() []plugin.Command {
    return []plugin.Command{{
        Name:        "my-plugin-do-thing",
        Description: "Do the thing from the command line",
        Run: func(args []string) error {
            // Do CLI work
            return nil
        },
    }}
}
```

## Patterns Learned from the Blobstore Plugin

### 1. Logical deletion for shared data

When data might be accessed by in-flight workflows, delete logically:

```sql
UPDATE blob_index SET deleted_at = now() WHERE key = $1;
```

Physical cleanup happens in background after all workflows release their refs:

```sql
DELETE FROM blob_content WHERE sha256 IN (
    SELECT c.sha256 FROM blob_content c
    WHERE c.ref_count <= 0
    AND NOT EXISTS (SELECT 1 FROM workflow_blob_refs r WHERE r.sha256 = c.sha256)
    AND NOT EXISTS (SELECT 1 FROM blob_index i WHERE i.sha256 = c.sha256 AND i.deleted_at IS NULL)
);
```

### 2. Content-addressing for deduplication

Use SHA-256 to identify content. Multiple keys can share the same bytes.
Track references:

```sql
INSERT INTO blob_content (sha256, size, ref_count) VALUES ($1, $2, 1)
ON CONFLICT (sha256) DO UPDATE SET ref_count = blob_content.ref_count + 1;
```

### 3. Pluggable backends via Go interfaces

Define a `Backend` interface for pluggable implementations:

```go
type Backend interface {
    Put(ctx context.Context, sha256 string, data []byte, contentType string) error
    Get(ctx context.Context, sha256 string) ([]byte, error)
    Delete(ctx context.Context, sha256 string) error
}
```

Select backend in `Init()` based on config. Memory for dev, S3 for production.

### 4. Config in env.Config, not env vars

Parse your config from `env.Config`:

```go
type Config struct {
    Backend string `json:"backend"`
    Bucket  string `json:"bucket"`
}

func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
    if len(env.Config) > 0 {
        json.Unmarshal(env.Config, &p.config)
    }
    // Apply defaults
    if p.config.Backend == "" {
        p.config.Backend = "memory"
    }
    return nil
}
```

### 5. Clean up stale references

If you track workflow references, clean up stale ones periodically:

```go
func (p *Plugin) cleanupStaleRefs(ctx context.Context) error {
    _, err := p.db.ExecContext(ctx, `
        DELETE FROM workflow_blob_refs 
        WHERE workflow_id NOT IN (
            SELECT id FROM workflow_instances WHERE status IN ('ready', 'running')
        )
    `)
    return err
}
```

## What NOT to Do

- **Don't import `internal/host` from a plugin** — causes import cycles. All
  host interaction is through `plugin.Environment` and `plugin.FuncRegistry`.
- **Don't register routes without a plugin prefix** — `/my-plugin/...` not `/...`.
- **Don't hardcode tenant IDs** — read them from context.
- **Don't use `plugin.TenantFromContext` in HTTP handlers** — use
  `auth.TenantIDFromContext` (auth middleware sets a different context key).
- **Don't allocate resources in the constructor** — the constructor is called
  during `Discover()` before `RunMigrations()`. Allocate in `Init()`.
- **Don't store large outputs from non-idempotent host functions** — they're
  stored in event_history. Use `Idempotent: true` for functions that return
  large data.
- **Don't return errors from background `Run()`** — the goroutine exits and
  your plugin is disabled. Log and continue.

## Testing Your Plugin

```go
// plugins/my-plugin/my_plugin_test.go
package myplugin

import (
    "context"
    "testing"
    "github.com/cleat-team/cleat/internal/plugin"
)

func TestInfo(t *testing.T) {
    p := &Plugin{}
    info := p.Info()
    if info.Name != "my-plugin" {
        t.Errorf("expected name 'my-plugin', got %q", info.Name)
    }
}

func TestInit(t *testing.T) {
    p := &Plugin{}
    env := &plugin.Environment{Logger: nil}
    if err := p.Init(context.Background(), env); err != nil {
        t.Fatalf("Init failed: %v", err)
    }
}
```

For integration tests that need a database, use a test helper that creates
tables, runs the plugin's migrations, and verifies endpoint behavior.

## Importing Your Plugin

To activate a plugin, import it in the worker binary:

```go
// cmd/cleat-worker/main.go
import (
    _ "github.com/cleat-team/cleat/plugins/blobstore"
    _ "github.com/cleat-team/cleat/plugins/my-plugin"
)
```

Or build a custom worker with only the plugins you need. To remove a plugin,
remove the import and rebuild. The plugin's tables remain in the database
(safe — they won't be queried without the plugin importing them).
