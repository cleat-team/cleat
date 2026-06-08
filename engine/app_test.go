package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

type mockStoreFactory struct {
	driverName string
	dialect    Dialect
}

func (m *mockStoreFactory) OpenStore(_ context.Context, _ string, _ ...string) (WorkflowStore, io.Closer, error) {
	return &stubWorkflowStore{}, io.NopCloser(strings.NewReader("")), nil
}

func (m *mockStoreFactory) DriverName() string { return m.driverName }
func (m *mockStoreFactory) Dialect() Dialect   { return m.dialect }

var _ StoreFactory = (*mockStoreFactory)(nil)

type mockServiceCaller struct{}

func (m *mockServiceCaller) Call(_ context.Context, _, _, _ string) (string, error) {
	return `{"ok":true}`, nil
}

var _ ServiceCaller = (*mockServiceCaller)(nil)

type mockPlugin struct {
	info    plugin.PluginInfo
	initErr error
}

func (p *mockPlugin) Info() plugin.PluginInfo                         { return p.info }
func (p *mockPlugin) Init(_ context.Context, _ *plugin.Environment) error { return p.initErr }

var _ plugin.Plugin = (*mockPlugin)(nil)

type mockCloseablePlugin struct {
	mockPlugin
	closeErr error
	closed   *[]string
	name     string
}

func (p *mockCloseablePlugin) Close() error {
	if p.closed != nil {
		*p.closed = append(*p.closed, p.name)
	}
	return p.closeErr
}

var _ CloseablePlugin = (*mockCloseablePlugin)(nil)

type mockPluginWithHostFuncs struct {
	mockPlugin
	regErr error
	regFn  func(registry plugin.FuncRegistry) error
}

func (p *mockPluginWithHostFuncs) RegisterHostFunctions(registry plugin.FuncRegistry) error {
	if p.regFn != nil {
		return p.regFn(registry)
	}
	return p.regErr
}

var _ plugin.HasHostFunctions = (*mockPluginWithHostFuncs)(nil)

func TestNewApp_NilRuntime(t *testing.T) {
	_, err := NewApp(context.Background(), AppConfig{
		Runtime: nil, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
	})
	if err == nil { t.Fatal("expected error for nil Runtime") }
	if !strings.Contains(err.Error(), "AppConfig.Runtime is required") { t.Errorf("unexpected error: %v", err) }
}

func TestNewApp_NilStoreFactory(t *testing.T) {
	_, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: nil, ServiceCaller: &mockServiceCaller{},
	})
	if err == nil { t.Fatal("expected error for nil StoreFactory") }
	if !strings.Contains(err.Error(), "AppConfig.StoreFactory is required") { t.Errorf("unexpected error: %v", err) }
}

func TestNewApp_NilServiceCaller(t *testing.T) {
	_, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: nil,
	})
	if err == nil { t.Fatal("expected error for nil ServiceCaller") }
	if !strings.Contains(err.Error(), "AppConfig.ServiceCaller is required") { t.Errorf("unexpected error: %v", err) }
}

func TestNewApp_MinimalConfig(t *testing.T) {
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if app == nil { t.Fatal("expected non-nil app") }
	if app.Registry() == nil { t.Error("expected non-nil Registry") }
	if app.StreamRegistry() == nil { t.Error("expected non-nil StreamRegistry") }
	if app.Logger() == nil { t.Error("expected non-nil Logger") }
}

func TestNewApp_DefaultsApplied(t *testing.T) {
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if app.Logger() != slog.Default() { t.Error("nil Logger should default to slog.Default()") }
}

func TestNewApp_CustomLogger(t *testing.T) {
	custom := slog.New(slog.DiscardHandler)
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{}, Logger: custom,
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if app.Logger() != custom { t.Error("expected custom logger to be used") }
}

func TestNewApp_CustomRegistry(t *testing.T) {
	customReg := NewPluginRegistry()
	customStreamReg := NewPluginStreamRegistry()
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
		Registry: customReg, StreamRegistry: customStreamReg,
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if app.Registry() != customReg { t.Error("expected custom registry") }
	if app.StreamRegistry() != customStreamReg { t.Error("expected custom stream registry") }
}

func TestNewApp_CustomMux(t *testing.T) {
	customMux := http.NewServeMux()
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{}, Mux: customMux,
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if app.Mux() != customMux { t.Error("expected custom mux") }
}

func TestNewApp_Mux_ReturnsNilWhenConfigMuxIsNil(t *testing.T) {
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if app.Mux() != nil { t.Error("Mux() should return nil when config.Mux is nil (known bug)") }
}

func TestNewApp_PluginInitSuccess(t *testing.T) {
	p := &mockPlugin{info: plugin.PluginInfo{Name: "test-plugin", Version: "1.0"}}
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
		Plugins: []plugin.Plugin{p},
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if len(app.plugins) != 1 { t.Errorf("expected 1 plugin, got %d", len(app.plugins)) }
}

func TestNewApp_PluginInitFailure(t *testing.T) {
	p := &mockPlugin{info: plugin.PluginInfo{Name: "bad-plugin", Version: "1.0"}, initErr: errors.New("plugin init failed")}
	_, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
		Plugins: []plugin.Plugin{p},
	})
	if err == nil { t.Fatal("expected error from plugin init failure") }
	if !strings.Contains(err.Error(), "plugin bad-plugin init") { t.Errorf("unexpected error: %v", err) }
}

func TestNewApp_PluginInitFailure_CleansUpPrevious(t *testing.T) {
	var closed []string
	p1 := &mockCloseablePlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "p1", Version: "1.0"}},
		name: "p1", closed: &closed,
	}
	p2 := &mockPlugin{info: plugin.PluginInfo{Name: "p2", Version: "1.0"}, initErr: errors.New("p2 init failed")}
	_, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
		Plugins: []plugin.Plugin{p1, p2},
	})
	if err == nil { t.Fatal("expected error from p2 init failure") }
	if len(closed) != 1 || closed[0] != "p1" { t.Errorf("expected p1 to be closed during cleanup, got %v", closed) }
}

func TestNewApp_PluginWithHostFunctions(t *testing.T) {
	reg := NewPluginRegistry()
	p := &mockPluginWithHostFuncs{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "host-fn-plugin", Version: "1.0"}},
		regFn: func(registry plugin.FuncRegistry) error {
			return registry.Register(plugin.FuncOptions{Name: "myFunc"}, func(_ context.Context, _ string) (string, error) { return "ok", nil })
		},
	}
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
		Registry: reg, Plugins: []plugin.Plugin{p},
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if !reg.Has("host-fn-plugin", "myFunc") { t.Error("expected host function to be registered") }
	if len(app.plugins) != 1 { t.Errorf("expected 1 plugin, got %d", len(app.plugins)) }
}

func TestNewApp_PluginWithHostFunctionsError(t *testing.T) {
	regErr := errors.New("register host functions failed")
	p := &mockPluginWithHostFuncs{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "host-fn-err", Version: "1.0"}},
		regErr: regErr,
	}
	_, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
		Plugins: []plugin.Plugin{p},
	})
	if err == nil { t.Fatal("expected error from host function registration failure") }
	if !strings.Contains(err.Error(), "plugin host-fn-err register host functions") { t.Errorf("unexpected error: %v", err) }
}

func TestApp_Close_Success(t *testing.T) {
	var closed []string
	p1 := &mockCloseablePlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "p1", Version: "1.0"}},
		name: "p1", closed: &closed,
	}
	p2 := &mockCloseablePlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "p2", Version: "1.0"}},
		name: "p2", closed: &closed,
	}
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
		Plugins: []plugin.Plugin{p1, p2},
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if err := app.Close(); err != nil { t.Errorf("Close failed: %v", err) }
	if len(closed) != 2 { t.Fatalf("expected 2 closed, got %d", len(closed)) }
	if closed[0] != "p2" || closed[1] != "p1" { t.Errorf("expected reverse close order [p2, p1], got %v", closed) }
}

func TestApp_Close_NoCloseablePlugins(t *testing.T) {
	p := &mockPlugin{info: plugin.PluginInfo{Name: "plain", Version: "1.0"}}
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
		Plugins: []plugin.Plugin{p},
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if err := app.Close(); err != nil { t.Errorf("Close should not error on non-closeable plugins: %v", err) }
}

func TestApp_Close_ReturnsLastError(t *testing.T) {
	err1 := errors.New("err1")
	err2 := errors.New("err2")
	var closed []string
	p1 := &mockCloseablePlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "p1", Version: "1.0"}},
		name: "p1", closeErr: err1, closed: &closed,
	}
	p2 := &mockCloseablePlugin{
		mockPlugin: mockPlugin{info: plugin.PluginInfo{Name: "p2", Version: "1.0"}},
		name: "p2", closeErr: err2, closed: &closed,
	}
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{},
		Plugins: []plugin.Plugin{p1, p2},
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	closeErr := app.Close()
	if closeErr != err1 { t.Errorf("expected last error (err1 from p1, closed last in reverse), got %v", closeErr) }
	if len(closed) != 2 { t.Errorf("expected both Close() called, got %d", len(closed)) }
}

func TestApp_Registry(t *testing.T) {
	reg := NewPluginRegistry()
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{}, Registry: reg,
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if app.Registry() != reg { t.Error("expected custom registry") }
}

func TestApp_StreamRegistry(t *testing.T) {
	sr := NewPluginStreamRegistry()
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{}, StreamRegistry: sr,
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if app.StreamRegistry() != sr { t.Error("expected custom stream registry") }
}

func TestApp_Logger(t *testing.T) {
	custom := slog.New(slog.DiscardHandler)
	app, err := NewApp(context.Background(), AppConfig{
		Runtime: &Runtime{}, StoreFactory: &mockStoreFactory{}, ServiceCaller: &mockServiceCaller{}, Logger: custom,
	})
	if err != nil { t.Fatalf("NewApp failed: %v", err) }
	if app.Logger() != custom { t.Error("expected custom logger") }
}

func TestAppFuncRegistry_Register(t *testing.T) {
	reg := NewPluginRegistry()
	afr := &appFuncRegistry{pluginName: "test-plugin", registry: reg}
	err := afr.Register(plugin.FuncOptions{Name: "myFn", Idempotent: false}, func(_ context.Context, _ string) (string, error) { return "result", nil })
	if err != nil { t.Fatalf("Register failed: %v", err) }
	if !reg.Has("test-plugin", "myFn") { t.Error("expected function to be registered (non-idempotent)") }
}

func TestAppFuncRegistry_RegisterIdempotent(t *testing.T) {
	reg := NewPluginRegistry()
	afr := &appFuncRegistry{pluginName: "test-plugin", registry: reg}
	err := afr.Register(plugin.FuncOptions{Name: "idemFn", Idempotent: true}, func(_ context.Context, _ string) (string, error) { return "idempotent", nil })
	if err != nil { t.Fatalf("Register failed: %v", err) }
	if !reg.Has("test-plugin", "idemFn") { t.Error("expected function to be registered (idempotent)") }
	_, idempotent, _ := reg.Lookup("test-plugin", "idemFn")
	if !idempotent { t.Error("expected function to be idempotent") }
}

func TestAppFuncRegistry_Register_DuplicateName(t *testing.T) {
	reg := NewPluginRegistry()
	afr := &appFuncRegistry{pluginName: "test-plugin", registry: reg}
	fn := func(_ context.Context, _ string) (string, error) { return "", nil }
	_ = afr.Register(plugin.FuncOptions{Name: "dup", Idempotent: false}, fn)
	err := afr.Register(plugin.FuncOptions{Name: "dup", Idempotent: false}, fn)
	if err == nil { t.Error("expected error on duplicate registration") }
}
