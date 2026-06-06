package pluginharness

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// TestPluginEnv is a complete plugin test environment. It holds all of the
// infrastructure a plugin may need: database, mock servers, host function
// registries, and the loaded plugin instances.
//
// Create one with NewTestPluginEnv or NewTestPluginEnvInMemory, and always
// call Close when done (typically via defer).
type TestPluginEnv struct {
	T           *testing.T
	Ctx         context.Context
	Cancel      context.CancelFunc
	PluginDB    plugin.PluginDB
	Dialect     plugin.Dialect
	Registry    *engine.PluginRegistry
	StreamReg   *engine.PluginStreamRegistry
	MockServers *MockServers
	Plugins     []*plugin.LoadedPlugin

	// schemaName and db hold the raw DB connection used during setup so
	// that Close can clean up the schema and close the connection.
	schemaName string
	db         *sql.DB
}

// NewTestPluginEnv creates a fully configured TestPluginEnv backed by a real
// database. It:
//  1. Opens the test database and creates a unique schema
//  2. Runs core migrations
//  3. Discovers and initialises all registered plugins
//  4. Runs plugin migrations
//  5. Seeds plugin configuration tables
//  6. Builds the host registries
//  7. Starts all mock servers
//
// The caller must call Close() when finished (typically via defer).
func NewTestPluginEnv(t *testing.T, ctx context.Context, db *sql.DB, dialect plugin.Dialect, schemaName string) *TestPluginEnv {
	t.Helper()

	// Wrap the raw *sql.DB in a PluginDB adapter.
	pluginDB := &engine.SQLDBAdapter{DB: db}

	// In-memory (no connection managed by us) — we just use the provided db.
	env := &TestPluginEnv{
		T:        t,
		Ctx:      ctx,
		Dialect:  dialect,
		PluginDB: pluginDB,
		db:       db,
	}

	// Run core migrations.
	RunCoreMigrations(t, db, dialect, schemaName)

	// Discover and initialise plugins.
	loadedPlugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("NewTestPluginEnv: Discover: %v", err)
	}

	envCfg := &plugin.Environment{
		DB:      pluginDB,
		Dialect: dialect,
	}
	plugin.InitAll(ctx, envCfg, loadedPlugins)
	env.Plugins = loadedPlugins

	// Run plugin migrations.
	RunPluginMigrations(t, db, dialect, loadedPlugins)

	// Seed config tables.
	SeedPluginConfig(t, db, dialect)

	// Build host registries.
	pr := engine.NewPluginRegistry()
	psr := engine.NewPluginStreamRegistry()

	// Register host functions.
	for _, lp := range loadedPlugins {
		if !lp.Healthy {
			continue
		}
		pluginName := lp.Plugin.Info().Name
		if hf, ok := lp.Plugin.(plugin.HasHostFunctions); ok {
			adapter := &hostFuncAdapter{
				pluginName: pluginName,
				registry:   pr,
			}
			if err := hf.RegisterHostFunctions(adapter); err != nil {
				t.Fatalf("NewTestPluginEnv: %s RegisterHostFunctions: %v", pluginName, err)
			}
		}
		if sf, ok := lp.Plugin.(pluginStreamPlugin); ok {
			streamAdapter := &streamFuncAdapter{
				pluginName: pluginName,
				registry:   psr,
			}
			if err := sf.RegisterStreamHostFunctions(streamAdapter); err != nil {
				t.Fatalf("NewTestPluginEnv: %s RegisterStreamHostFunctions: %v", pluginName, err)
			}
		}
	}

	env.Registry = pr
	env.StreamReg = psr

	// Start mock servers.
	env.MockServers = StartMockServers()

	t.Log("NewTestPluginEnv: environment ready")
	env.Diagnostic()

	return env
}

// NewTestPluginEnvInMemory creates a minimal TestPluginEnv with no database
// connection. Mock servers are started and plugins are discovered and
// initialised. Plugins that depend on a database will fail to initialise,
// which is acceptable for Layer 1 (unit) testing.
//
// The caller must call Close() when finished.
func NewTestPluginEnvInMemory(t *testing.T) *TestPluginEnv {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	env := &TestPluginEnv{
		T:      t,
		Ctx:    ctx,
		Cancel: cancel,
		// PluginDB and db stay nil — no database available.
	}

	// Discover plugins.
	loadedPlugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("NewTestPluginEnvInMemory: Discover: %v", err)
	}

	envCfg := &plugin.Environment{
		DB:      nil,
		Dialect: plugin.DialectPostgres,
	}
	plugin.InitAll(ctx, envCfg, loadedPlugins)
	env.Plugins = loadedPlugins

	// Build registries.
	pr := engine.NewPluginRegistry()
	psr := engine.NewPluginStreamRegistry()

	for _, lp := range loadedPlugins {
		if !lp.Healthy {
			continue
		}
		pluginName := lp.Plugin.Info().Name
		if hf, ok := lp.Plugin.(plugin.HasHostFunctions); ok {
			adapter := &hostFuncAdapter{
				pluginName: pluginName,
				registry:   pr,
			}
			if err := hf.RegisterHostFunctions(adapter); err != nil {
				t.Fatalf("NewTestPluginEnvInMemory: %s RegisterHostFunctions: %v", pluginName, err)
			}
		}
		if sf, ok := lp.Plugin.(pluginStreamPlugin); ok {
			streamAdapter := &streamFuncAdapter{
				pluginName: pluginName,
				registry:   psr,
			}
			if err := sf.RegisterStreamHostFunctions(streamAdapter); err != nil {
				t.Fatalf("NewTestPluginEnvInMemory: %s RegisterStreamHostFunctions: %v", pluginName, err)
			}
		}
	}

	env.Registry = pr
	env.StreamReg = psr

	// Start mock servers.
	env.MockServers = StartMockServers()

	t.Log("NewTestPluginEnvInMemory: environment ready")
	env.Diagnostic()

	return env
}

// Close stops mock servers, cleans up the database schema (if any), closes the
// database connection, and cancels the context.
func (e *TestPluginEnv) Close() {
	e.T.Helper()

	StopMockServers(e.MockServers)

	if e.db != nil && e.schemaName != "" {
		CleanupTestDB(e.T, e.db, e.Dialect, e.schemaName)
	} else if e.db != nil {
		e.db.Close()
	}

	if e.Cancel != nil {
		e.Cancel()
	}

	e.T.Log("TestPluginEnv: closed")
}

// Diagnostic prints the current state of the environment to the test log.
func (e *TestPluginEnv) Diagnostic() {
	e.T.Helper()

	dbStatus := func(d plugin.Dialect) string {
		if e.Dialect == d && e.PluginDB != nil {
			return "CONFIGURED"
		}
		return "NOT SET"
	}

	pluginCount := 0
	if e.Plugins != nil {
		pluginCount = len(e.Plugins)
	}

	mockStatus := "INACTIVE"
	if e.MockServers != nil {
		mockStatus = "ACTIVE"
	}

	e.T.Logf("=== Plugin Harness Environment ===")
	e.T.Logf("Databases: PostgreSQL [%s] / MySQL [%s] / MSSQL [%s]",
		dbStatus(plugin.DialectPostgres),
		dbStatus(plugin.DialectMySQL),
		dbStatus(plugin.DialectMSSQL))
	e.T.Logf("Plugin Registry: %d plugins loaded", pluginCount)
	e.T.Logf("Mock Servers: LLM, PagerDuty, Slack, Kafka [%s]", mockStatus)
	e.T.Logf("==================================")
}
