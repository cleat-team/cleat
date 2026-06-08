package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// ---------------------------------------------------------------------------
// Mock types
// ---------------------------------------------------------------------------

type mockStoreFactory struct {
	name    string
	dialect Dialect
}

func (m *mockStoreFactory) OpenStore(_ context.Context, _ string, _ ...string) (WorkflowStore, io.Closer, error) {
	return nil, io.NopCloser(nil), nil
}

func (m *mockStoreFactory) DriverName() string { return m.name }
func (m *mockStoreFactory) Dialect() Dialect    { return m.dialect }

type mockServiceCaller struct{}

func (m *mockServiceCaller) Call(_ context.Context, _, _, _ string) (string, error) {
	return `{"ok":true}`, nil
}

type mockPlugin struct {
	info     plugin.PluginInfo
	initErr  error
	initFn   func(ctx context.Context, env *plugin.Environment) error
	initCalls *int32 // tracks number of Init calls
}

func (m *mockPlugin) Info() plugin.PluginInfo { return m.info }
func (m *mockPlugin) Init(ctx context.Context, env *plugin.Environment) error {
	if m.initCalls != nil {
		atomic.AddInt32(m.initCalls, 1)
	}
	if m.initFn != nil {
		return m.initFn(ctx, env)
	}
	return m.initErr
}

type mockCloseablePlugin struct {
	mockPlugin
	closeErr   error
	closeOrder *int32 // incremented on each Close call
	closeSeq   []int32
}

func (m *mockCloseablePlugin) Close() error {
	seq := atomic.AddInt32(m.closeOrder, 1)
	m.closeSeq = append(m.closeSeq, seq)
	return m.closeErr
}

type mockHostFuncPlugin struct {
	mockPlugin
	registerErr error
	registered  bool
}

func (m *mockHostFuncPlugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if m.registerErr != nil {
		return m.registerErr
	}
	m.registered = true
	return scope.Register(plugin.FuncOptions{Name: "testFunc"}, func(_ context.Context, _ string) (string, error) {
		return "ok", nil
	})
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestNewApp_Validation(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}

	t.Run("nil Runtime", func(t *testing.T) {
		_, err := NewApp(context.Background(), AppConfig{
			StoreFactory: store,
			ServiceCaller: caller,
		})
		if err == nil {
			t.Fatal("expected error for nil Runtime, got nil")
		}
		if err.Error() == "" || !contains(err.Error(), "Runtime") {
			t.Errorf("error %q should mention Runtime", err.Error())
		}
	})

	t.Run("nil StoreFactory", func(t *testing.T) {
		_, err := NewApp(context.Background(), AppConfig{
			Runtime:       &Runtime{},
			ServiceCaller: caller,
		})
		if err == nil {
			t.Fatal("expected error for nil StoreFactory, got nil")
		}
		if !contains(err.Error(), "StoreFactory") {
			t.Errorf("error %q should mention StoreFactory", err.Error())
		}
	})

	t.Run("nil ServiceCaller", func(t *testing.T) {
		_, err := NewApp(context.Background(), AppConfig{
			Runtime:      &Runtime{},
			StoreFactory: store,
		})
		if err == nil {
			t.Fatal("expected error for nil ServiceCaller, got nil")
		}
		if !contains(err.Error(), "ServiceCaller") {
			t.Errorf("error %q should mention ServiceCaller", err.Error())
		}
	})
}

func TestNewApp_Defaults(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	if app.Logger() == nil {
		t.Error("Logger is nil, want non-nil default")
	}
	if app.Registry() == nil {
		t.Error("Registry is nil, want non-nil default")
	}
	if app.StreamRegistry() == nil {
		t.Error("StreamRegistry is nil, want non-nil default")
	}
	// Mux() returns config.Mux (the original config value), not the
	// locally-created default mux. When no Mux is provided, this is nil.
	if app.Mux() != nil {
		t.Error("Mux is non-nil, want nil (Mux() returns config.Mux exactly)")
	}
}

func TestNewApp_Defaults_ProvidedLogger(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	logger := slog.New(slog.DiscardHandler)

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Logger:        logger,
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if app.Logger() != logger {
		t.Error("Logger was not the provided logger")
	}
}

func TestNewApp_Defaults_ProvidedRegistry(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	reg := NewPluginRegistry()

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Registry:      reg,
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if app.Registry() != reg {
		t.Error("Registry was not the provided registry")
	}
}

func TestNewApp_Defaults_ProvidedStreamRegistry(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	sr := NewPluginStreamRegistry()

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:        &Runtime{},
		StoreFactory:   store,
		ServiceCaller:  caller,
		StreamRegistry: sr,
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if app.StreamRegistry() != sr {
		t.Error("StreamRegistry was not the provided stream registry")
	}
}

func TestNewApp_Defaults_ProvidedMux(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	mux := http.NewServeMux()

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Mux:           mux,
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if app.Mux() != mux {
		t.Error("Mux was not the provided mux")
	}
}

func TestNewApp_PluginInit_Success(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	var initCalls int32
	p := &mockPlugin{
		info:      plugin.PluginInfo{Name: "test-plugin", Version: "1.0"},
		initCalls: &initCalls,
	}

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Plugins:       []plugin.Plugin{p},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if app == nil {
		t.Fatal("NewApp returned nil app")
	}
	if initCalls != 1 {
		t.Errorf("Init was called %d times, want 1", initCalls)
	}
}

func TestNewApp_PluginInit_Failure(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	initErr := errors.New("plugin init failed")
	p := &mockPlugin{
		info:    plugin.PluginInfo{Name: "bad-plugin", Version: "1.0"},
		initErr: initErr,
	}

	_, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Plugins:       []plugin.Plugin{p},
	})
	if err == nil {
		t.Fatal("expected error for failing plugin init, got nil")
	}
	if !errors.Is(err, initErr) {
		t.Errorf("error %v should wrap %v", err, initErr)
	}
}

func TestNewApp_PluginInit_Failure_ClearsPrevious(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}

	// First plugin succeeds, second fails.
	// The first plugin (if Closeable) should have Close called.
	var closeOrder int32
	p1 := &mockCloseablePlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "ok-plugin", Version: "1.0"}},
		closeOrder: &closeOrder,
	}

	initErr := errors.New("second plugin failed")
	p2 := &mockPlugin{
		info:    plugin.PluginInfo{Name: "bad-plugin", Version: "1.0"},
		initErr: initErr,
	}

	_, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Plugins:       []plugin.Plugin{p1, p2},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// closePlugins should call Close on p1 during cleanup.
	if atomic.LoadInt32(&closeOrder) != 1 {
		t.Error("Close was not called on already-initialized plugin during cleanup")
	}
	if !contains(err.Error(), "bad-plugin") {
		t.Errorf("error %q should mention plugin name", err.Error())
	}
}

func TestNewApp_HasHostFunctions(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	p := &mockHostFuncPlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "host-func-plugin", Version: "1.0"}},
	}

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Plugins:       []plugin.Plugin{p},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if app == nil {
		t.Fatal("NewApp returned nil app")
	}
	if !p.registered {
		t.Error("RegisterHostFunctions was not called")
	}
}

func TestNewApp_HasHostFunctions_Error(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	regErr := errors.New("register host functions error")
	p := &mockHostFuncPlugin{
		mockPlugin:  mockPlugin{info: plugin.PluginInfo{Name: "bad-host-func", Version: "1.0"}},
		registerErr: regErr,
	}

	_, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Plugins:       []plugin.Plugin{p},
	})
	if err == nil {
		t.Fatal("expected error for HasHostFunctions failure, got nil")
	}
	if !errors.Is(err, regErr) {
		t.Errorf("error %v should wrap %v", err, regErr)
	}
}

func TestNewApp_PluginConfig(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}

	var receivedConfig []byte
	p := &mockPlugin{
		info: plugin.PluginInfo{Name: "config-plugin", Version: "1.0"},
		initFn: func(_ context.Context, env *plugin.Environment) error {
			receivedConfig = env.Config
			return nil
		},
	}

	_, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Plugins:       []plugin.Plugin{p},
		PluginConfigs: map[string]json.RawMessage{
			"config-plugin": json.RawMessage(`{"key":"value"}`),
		},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if string(receivedConfig) != `{"key":"value"}` {
		t.Errorf("plugin received config %q, want %q", receivedConfig, `{"key":"value"}`)
	}
}

func TestApp_Close(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	var closeOrder int32
	p := &mockCloseablePlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "closable", Version: "1.0"}},
		closeOrder: &closeOrder,
	}

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Plugins:       []plugin.Plugin{p},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	if err := app.Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
	if atomic.LoadInt32(&closeOrder) != 1 {
		t.Error("Close was not called on CloseablePlugin")
	}
}

func TestApp_Close_Error(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	var closeOrder int32
	closeErr := fmt.Errorf("close failed")
	p := &mockCloseablePlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "closable", Version: "1.0"}},
		closeErr:   closeErr,
		closeOrder: &closeOrder,
	}

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Plugins:       []plugin.Plugin{p},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	err = app.Close()
	if !errors.Is(err, closeErr) {
		t.Errorf("Close error = %v, want %v", err, closeErr)
	}
}

func TestApp_Close_ReverseOrder(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	var closeOrder int32

	p1 := &mockCloseablePlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "first", Version: "1.0"}},
		closeOrder: &closeOrder,
	}
	p2 := &mockCloseablePlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "second", Version: "1.0"}},
		closeOrder: &closeOrder,
	}

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Plugins:       []plugin.Plugin{p1, p2},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	app.Close()

	// Close is called in reverse order: p2 (seq=1), p1 (seq=2).
	if len(p1.closeSeq) != 1 || p1.closeSeq[0] != 2 {
		t.Errorf("first plugin close seq = %v, want [2]", p1.closeSeq)
	}
	if len(p2.closeSeq) != 1 || p2.closeSeq[0] != 1 {
		t.Errorf("second plugin close seq = %v, want [1]", p2.closeSeq)
	}
}

func TestApp_Close_NonCloseable(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	p := &mockPlugin{info: plugin.PluginInfo{Name: "plain", Version: "1.0"}}

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Plugins:       []plugin.Plugin{p},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	// Non-CloseablePlugin should not cause panic or error.
	if err := app.Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
}

func TestApp_Close_LastError(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	var closeOrder int32
	err1 := fmt.Errorf("error 1")
	err2 := fmt.Errorf("error 2")

	p1 := &mockCloseablePlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "p1", Version: "1.0"}},
		closeErr:   err1,
		closeOrder: &closeOrder,
	}
	p2 := &mockCloseablePlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "p2", Version: "1.0"}},
		closeErr:   err2,
		closeOrder: &closeOrder,
	}

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
		Plugins:       []plugin.Plugin{p1, p2},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	err = app.Close()
	// closePlugins closes in reverse order: p2 then p1.
	// p2 closes first (error 2), then p1 closes (error 1 overwrites).
	// Last error wins.
	if !errors.Is(err, err1) {
		t.Errorf("Close error = %v, want %v (last error)", err, err1)
	}
}

func TestApp_Accessors(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}
	mux := http.NewServeMux()
	logger := slog.New(slog.DiscardHandler)
	reg := NewPluginRegistry()
	sr := NewPluginStreamRegistry()

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:        &Runtime{},
		StoreFactory:   store,
		ServiceCaller:  caller,
		Mux:            mux,
		Logger:         logger,
		Registry:       reg,
		StreamRegistry: sr,
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	if app.Registry() != reg {
		t.Error("Registry accessor mismatch")
	}
	if app.StreamRegistry() != sr {
		t.Error("StreamRegistry accessor mismatch")
	}
	if app.Logger() != logger {
		t.Error("Logger accessor mismatch")
	}
	if app.Mux() != mux {
		t.Error("Mux accessor mismatch")
	}
}

func TestAppFuncRegistry_Register(t *testing.T) {
	reg := NewPluginRegistry()
	scope := &appFuncRegistry{pluginName: "test", registry: reg}

	err := scope.Register(plugin.FuncOptions{Name: "func1"}, func(_ context.Context, _ string) (string, error) {
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if !reg.Has("test", "func1") {
		t.Error("function was not registered in PluginRegistry")
	}
}

func TestAppFuncRegistry_RegisterIdempotent(t *testing.T) {
	reg := NewPluginRegistry()
	scope := &appFuncRegistry{pluginName: "test", registry: reg}

	err := scope.Register(plugin.FuncOptions{Name: "idemFunc", Idempotent: true}, func(_ context.Context, _ string) (string, error) {
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Verify it was registered and marked idempotent.
	_, idempotent, ok := reg.Lookup("test", "idemFunc")
	if !ok {
		t.Error("function was not registered in PluginRegistry")
	}
	if !idempotent {
		t.Error("function was not registered as idempotent")
	}
}

func TestApp_EmptyPlugins(t *testing.T) {
	store := &mockStoreFactory{name: "pgx", dialect: DialectPostgres}
	caller := &mockServiceCaller{}

	app, err := NewApp(context.Background(), AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  store,
		ServiceCaller: caller,
	})
	if err != nil {
		t.Fatalf("NewApp with empty plugins: %v", err)
	}
	if app == nil {
		t.Fatal("NewApp returned nil")
	}
	if l := len(app.plugins); l != 0 {
		t.Errorf("plugins len = %d, want 0", l)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
