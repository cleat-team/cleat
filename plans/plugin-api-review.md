# Plugin API — Skeptical Review

## Bugs (must fix before writing any plugin)

### B1: Compaction silently drops plugin_call events

`compaction.go` has no `EventTypePluginCall` in its event type code maps or its
switch/case in `extractCompactionState`. The `eventTypeToCode` map returns 0
(zero value = `EventCodeCall`) for unknown types. Plugin call events are
**misinterpreted as DurableCall events** during compaction — their output is
lost, and replay after compaction will fail with divergence errors.

**Fix**: Add `EventCodePluginCall = 10` and the bidirectional mappings. Add
`case EventTypePluginCall:` in the compaction switch that preserves
`PluginName`, `PluginFunc`, `PluginInput`, `PluginOutput`, `PluginError`.

### B2: Migrations run after Init — plugin can't use its own tables

In `cmd/cleat-worker/main.go`, `LoadAll` (which calls `Init`) runs before
`RunMigrations`. A plugin that creates tables in its migration can't query
them in `Init()` — the table doesn't exist yet.

**Fix**: Split the lifecycle. Discover which plugins have migrations, run
migrations first, then call Init. `LoadAll` should only call Init after
migrations complete. OR: add a two-phase init.

### B3: Plugin functions don't receive tenant context

`freshPluginCall` calls `fn(ctx, inputJSON)` with the raw WASM context. The
tenant ID from the workflow instance is not injected. Plugins that call
`plugin.TenantFromContext(ctx)` get `uuid.Nil`.

**Fix**: Inject tenant ID before calling the plugin function:
```go
ctx = plugin.WithTenant(ctx, s.tenantID)
outputJSON, err := fn(ctx, inputJSON)
```
Where `s.tenantID` comes from the workflow instance's `tenant_id` column.

## Design Issues (should fix)

### D1: Plugin constructor is called twice

`Register()` calls `constructor()` to get `Info()`. Then `LoadAll()` calls
`constructor()` again to get the instance. A plugin that allocates resources
in its constructor (connections, buffers) wastes them on the first call.

**Fix**: Store `PluginInfo` alongside the constructor in the registry:
```go
type registryEntry struct {
    info PluginInfo
    ctor func() Plugin
}
```
`Register` calls ctor once, stores info. `LoadAll` calls ctor again. The
double-construction is still there but the first call is acknowledged as
"just for info" and constructors should be cheap.

**Alternative**: Separate `PluginInfo()` into a package-level variable:
```go
var Info = plugin.PluginInfo{Name: "slack-notify", Version: "1.0.0"}
```
Then `Register` takes a `PluginInfo` and a constructor. No double construction.
But this changes the registration API — worth it.

### D2: "/" in plugin or function names causes lookup collisions

`PluginRegistry.Lookup` uses `pluginName + "/" + funcName` as the key.
A plugin named "a/b" with function "c" collides with plugin "a" with
function "b/c".

**Fix**: Use a separator that's not valid in identifiers:
```go
func (pr *PluginRegistry) lookupKey(pluginName, funcName string) string {
    return pluginName + "\x00" + funcName
}
```

### D3: FuncRegistry.Register return type is misleading

The interface says `Register(funcName string, fn PluginFunc) error` but the
adapter implementation always returns nil. Either remove the error return or
add duplicate function name detection.

**Fix**: Add duplicate detection in the adapter. If the same function name
is registered twice within a plugin, return an error:
```go
func (a *hostPluginRegistryAdapter) Register(funcName string, fn plugin.PluginFunc) error {
    if funcName == "" {
        return errors.New("function name must not be empty")
    }
    key := a.pluginName + "/" + funcName
    if _, exists := a.registry.funcs[key]; exists {
        return fmt.Errorf("function %q already registered for plugin %q", funcName, a.pluginName)
    }
    a.registry.Register(a.pluginName, funcName, host.PluginFunc(fn))
    return nil
}
```
This requires exposing a check method on `PluginRegistry` or making it
return an error from Register.

### D4: Environment.Config type implies JSON but configs may be YAML

`json.RawMessage` sends a strong signal that config is JSON. If the global
config file is YAML (which is common for infrastructure tools), plugins get
confused.

**Fix**: Change to `[]byte`. No format signal. Plugins parse however they want.

## Polish

### P1: No function name validation

A plugin can register a function named "" (empty), or "plugin_call" (which
would shadow the engine's own import), or "durable_call" (confusing). The
WASM boundary maps these to the `plugin_call` import with function name as
a string parameter, so they don't actually conflict with engine imports —
but empty names and names with "/" are still bugs.

**Fix**: Validate in the adapter's Register: reject empty, reject names
containing "/" and "\x00".

### P2: Test state leaks between test functions

`TestPanickingPlugin` and `TestFailingInitPlugin` register plugins in test
functions. These registrations persist across all subsequent tests. While
the current tests pass, this makes future tests fragile.

**Fix**: Add `Unregister(name string)` for tests. Call it in `t.Cleanup()`.

### P3: No end-to-end test of a real plugin

All tests are unit tests on the registry. There's no test that:
1. Registers a plugin with migrations, routes, host functions, and background
2. Loads it through the full `LoadAll` path
3. Verifies migrations run, routes are registered, host functions are callable

**Fix**: Add an integration test that loads a test plugin through the full
lifecycle.

## What's Good

- The `Plugin` interface is genuinely minimal (2 methods). This is correct.
- Optional interfaces via type assertion is the right pattern. Zero boilerplate.
- Raw stdlib types in Environment. No wrapper types. Correct decision.
- Scoped `FuncRegistry` eliminates the plugin-name-typo bug.
- Topological sort with cycle detection. Solid.
- Panic recovery in LoadAll prevents one bad plugin from taking down the worker.
- `plugin_call` as a single WASM import (not per-plugin imports). Correct design.
- Replay determinism via engine. Plugin authors don't think about it.

## Verdict

Three bugs must be fixed before writing the first real plugin. Four design
improvements are worth making now while the API surface is small. Three
polish items can wait but would improve the developer experience.
