# Plugin System

Cleat's plugin system extends the worker daemon with custom functionality.
Plugins are Go packages compiled into the worker binary, registered via
`init()`, and discovered at startup.

## Design Principle

Plugins receive raw access to infrastructure, not abstractions over it. The
`Environment` struct uses standard library types (`*sql.DB`, `*http.ServeMux`,
`*slog.Logger`) so plugin authors don't need to learn a new API.

## Plugin Interface

### Required Interface

Every plugin must implement `plugin.Plugin`:

```go
type Plugin interface {
    Info() PluginInfo
    Init(ctx context.Context, env *Environment) error
}
```

### PluginInfo

```go
type PluginInfo struct {
    Name        string   `json:"name"`
    Version     string   `json:"version"`
    Description string   `json:"description"`
    Author      string   `json:"author,omitempty"`
    Requires    []string `json:"requires,omitempty"`
}
```

The `Requires` field declares dependencies on other plugins. The loader sorts
plugins topologically using Kahn's algorithm before initialization.

### Environment

```go
type Environment struct {
    DB       *sql.DB
    Mux      *http.ServeMux
    Config   []byte
    Logger   *slog.Logger
    TenantID uuid.UUID
    Done     <-chan struct{}

    StartWorkflow func(ctx context.Context, defName string, input json.RawMessage) (runID string, err error)
    SignalWorkflow func(ctx context.Context, workflowID, signalName, payload string) error
}
```

- `StartWorkflow` -- starts a new workflow instance using the latest deployed
  version.
- `SignalWorkflow` -- delivers a signal to a running workflow instance.
- `Config` -- plugin-specific JSON configuration, loaded from
  `--plugin-config` flag.

## Optional Interfaces

Plugins can implement additional interfaces for extended functionality:

| Interface | Method(s) | Purpose |
|-----------|-----------|---------|
| `Stoppable` | `Stop(ctx)` | Cleanup on worker shutdown |
| `HasMigrations` | `Migrations()` | Plugin-specific database tables |
| `HasRoutes` | `RegisterRoutes(mux)` | HTTP endpoints under plugin namespace |
| `HasMiddleware` | `Middleware(next)` | Wrap the HTTP handler chain |
| `HasCommands` | `RegisterCommands()` | CLI subcommands |
| `HasBackground` | `Run(ctx)` | Long-running background goroutine |
| `HasHostFunctions` | `RegisterHostFunctions(registry)` | Functions callable from workflows |
| `HasHealth` | `Health()` | Health check endpoint |

### Plugin Lifecycle

```
Worker Startup
     |
     v
1. Register (via init() -- compile time)
     |
     v
2. Discover -- instantiate all registered plugins, sort by dependency
     |
     v
3. RunMigrations -- apply each plugin's database migrations in order
     |
     v
4. InitAll -- call Init() on each plugin, marking unhealthy on failure
     |
     v
5. RegisterRoutes -- call RegisterRoutes() for HasRoutes plugins
     |
     v
6. RegisterHostFunctions -- call RegisterHostFunctions() for HasHostFunctions plugins
     |
     v
7. Run() -- start background goroutines for HasBackground plugins
     |
     v
Worker Running
     |
     v
8. Stop() -- call Stop() for Stoppable plugins on shutdown
```

## Registration and Discovery

### Registration

Plugins register themselves at compile time via `init()`:

```go
func init() {
    plugin.Register(plugin.PluginInfo{
        Name:        "llm",
        Version:     "0.1.0",
        Description: "LLM inference plugin for cleat workflows",
        Author:      "Cleat Authors",
    }, func() plugin.Plugin {
        return &LLMPlugin{}
    })
}
```

The worker imports the plugins in `main.go`:

```go
import (
    _ "github.com/rcownie/cleat/plugins/llm"
    _ "github.com/rcownie/cleat/plugins/pgvector"
)
```

### Discovery

`plugin.Discover()` instantiates all registered plugin constructors in
dependency order (topological sort via Kahn's algorithm). Circular dependencies
are detected and reported as errors.

`plugin.LoadAll()` is a convenience that calls `Discover()` followed by
`RunMigrations()` and `InitAll()`.

### Dependency Ordering

Plugins declare dependencies via `PluginInfo.Requires`:

```go
plugin.PluginInfo{
    Name:     "pgvector",
    Requires: []string{"llm"},  // pgvector depends on llm
}
```

The loader ensures:
1. All dependencies exist (error if missing).
2. No circular dependencies exist (error if detected).
3. Plugins are initialized after their dependencies.

## Host Functions

### HasHostFunctions Interface

Plugins implementing `HasHostFunctions` can register functions callable from
workflow code. These functions are automatically recorded in the event history
and replayed deterministically -- plugin authors do not need to handle replay.

```go
type HasHostFunctions interface {
    Plugin
    RegisterHostFunctions(scope FuncRegistry) error
}
```

### Registration

```go
func (p *LLMPlugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
    return scope.Register(plugin.FuncOptions{
        Name:       "complete",
        Idempotent: false, // LLM calls are not idempotent
    }, p.handleComplete)
}

func (p *LLMPlugin) handleComplete(ctx context.Context, inputJSON string) (string, error) {
    var req CompleteRequest
    json.Unmarshal([]byte(inputJSON), &req)
    // ... make LLM API call ...
    return json.Marshal(resp), nil
}
```

### Streaming Host Functions

Plugins can also register streaming functions:

```go
type PluginStreamFunc func(ctx context.Context, inputJSON string) (<-chan StreamEvent, error)

type StreamEvent struct {
    Index   int    `json:"i"`
    Content string `json:"c"`
    Finish  bool   `json:"f"`
}
```

Streaming functions return chunks progressively. Each chunk is recorded in the
event history, enabling deterministic replay of streaming data.

### How Host Functions Relate to DurableCall

When a workflow calls `h.PluginCall("llm", "complete", inputJSON)`, the engine:

1. Looks up the function in the plugin registry (scoped by plugin name).
2. Records the call in event history (request).
3. Executes the plugin function.
4. Records the result in event history (response, or error).
5. On replay, returns the cached response from event history without
   re-executing the plugin function.

This is the same checkpoint/replay model used for `DurableCall`. The difference
is that plugin functions are compiled into the worker binary rather than being
external HTTP/gRPC services.

### Idempotent Functions

If a function is marked `Idempotent: true`, the engine may call it multiple
times during replay without recording the result in event history. This is
appropriate for read-only functions (e.g., query state, lookups).

## Plugin HTTP Routes

Plugins implementing `HasRoutes` can add HTTP endpoints under the worker's
HTTP server:

```go
func (p *WebhookPlugin) RegisterRoutes(mux *http.ServeMux) error {
    mux.HandleFunc("/api/plugins/webhook/receive", p.handleWebhook)
    return nil
}
```

Plugin routes are registered before the built-in API routes. The middleware
chain (from `HasMiddleware` plugins) wraps all handlers.

## Plugin Database Migrations

Plugins implementing `HasMigrations` can create their own database tables:

```go
func (p *EventSubPlugin) Migrations() []plugin.Migration {
    return []plugin.Migration{
        {
            Version: 1,
            Up: `CREATE TABLE IF NOT EXISTS event_subscriptions (
                id TEXT PRIMARY KEY,
                workflow_name TEXT NOT NULL,
                event_type TEXT NOT NULL,
                callback_url TEXT NOT NULL
            )`,
            Down: `DROP TABLE IF EXISTS event_subscriptions`,
        },
    }
}
```

Migrations are applied during `plugin.RunMigrations()`, in dependency order.

## Built-in Plugins

The project includes these built-in plugins:

| Plugin | Package | Description |
|--------|---------|-------------|
| LLM | `plugins/llm` | LLM inference host functions for AI agent workflows |
| pgvector | `plugins/pgvector` | Vector store operations via pgvector PostgreSQL extension |

## Calling Plugins from Workflows

From the Go SDK, plugins are called via `HostCalls`:

```go
// Synchronous call
response, err := h.PluginCall("llm", "complete", requestJSON)

// Streaming call
chunk, err := h.PluginCallStreaming("llm", "complete_stream", requestJSON)
```

The plugin name and function name are scoped -- `PluginCall("llm", "complete", ...)`
calls the `complete` function registered by the `llm` plugin.
