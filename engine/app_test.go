package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// ---------------------------------------------------------------------------
// Mock types
// ---------------------------------------------------------------------------

// mockPlugin implements plugin.Plugin with no optional interfaces.
type mockPlugin struct {
	info       plugin.PluginInfo
	initErr    error
	initCalled bool
	initEnv    *plugin.Environment
}

func (m *mockPlugin) Info() plugin.PluginInfo { return m.info }
func (m *mockPlugin) Init(ctx context.Context, env *plugin.Environment) error {
	m.initCalled = true
	m.initEnv = env
	return m.initErr
}

// mockPluginHF implements plugin.Plugin and plugin.HasHostFunctions.
type mockPluginHF struct {
	*mockPlugin
	hostFuncs    []hostFuncEntry
	hostFuncsErr error
}

type hostFuncEntry struct {
	opts plugin.FuncOptions
	fn   plugin.PluginFunc
}

func (m *mockPluginHF) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if m.hostFuncsErr != nil {
		return m.hostFuncsErr
	}
	for _, e := range m.hostFuncs {
		if err := scope.Register(e.opts, e.fn); err != nil {
			return err
		}
	}
	return nil
}

// mockPluginCloseable implements plugin.Plugin and CloseablePlugin.
type mockPluginCloseable struct {
	*mockPlugin
	closeErr error
	closed   bool
}

func (m *mockPluginCloseable) Close() error {
	m.closed = true
	return m.closeErr
}

// mockPluginFull implements plugin.Plugin, plugin.HasHostFunctions, and CloseablePlugin.
type mockPluginFull struct {
	*mockPlugin
	hostFuncs    []hostFuncEntry
	hostFuncsErr error
	closeErr     error
	closed       bool
}

func (m *mockPluginFull) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if m.hostFuncsErr != nil {
		return m.hostFuncsErr
	}
	for _, e := range m.hostFuncs {
		if err := scope.Register(e.opts, e.fn); err != nil {
			return err
		}
	}
	return nil
}

func (m *mockPluginFull) Close() error {
	m.closed = true
	return m.closeErr
}

// mockStoreFactory implements StoreFactory.
type mockStoreFactory struct {
	driverName string
	dialect    Dialect
}

func (f *mockStoreFactory) OpenStore(ctx context.Context, tenantID string, taskQueues ...string) (WorkflowStore, io.Closer, error) {
	return nil, io.NopCloser(nil), nil
}
func (f *mockStoreFactory) DriverName() string { return f.driverName }
func (f *mockStoreFactory) Dialect() Dialect   { return f.dialect }

// mockServiceCaller implements ServiceCaller.
type mockServiceCaller struct{}

func (c *mockServiceCaller) Call(ctx context.Context, service, operation, requestJSON string) (string, error) {
	return "", nil
}

// ---------------------------------------------------------------------------
// Validation tests
// ---------------------------------------------------------------------------

func TestNewApp_Validation(t *testing.T) {
	tests := []struct {
		name    string
		config  AppConfig
		wantErr string
	}{
		{
			name:    "nil Runtime",
			config:  AppConfig{Runtime: nil},
			wantErr: "Runtime",
		},
		{
			name: "nil StoreFactory",
			config: AppConfig{
				Runtime: &Runtime{},
			},
			wantErr: "StoreFactory",
		},
		{
			name: "nil ServiceCaller",
			config: AppConfig{
				Runtime:      &Runtime{},
				StoreFactory: &mockStoreFactory{},
			},
			wantErr: "ServiceCaller",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewApp(context.Background(), tt.config)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Default values tests
// ---------------------------------------------------------------------------

func TestNewApp_Defaults(t *testing.T) {
	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
	}

	app, err := NewApp(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}

	if app.Logger() != slog.Default() {
		t.Error("expected default logger when config.Logger is nil")
	}
	// App.Mux() returns config.Mux directly — when nil, it stays nil.
	// The local default mux is used for plugin Init but not persisted.
	if app.Mux() != nil {
		t.Error("expected nil mux from getter when config.Mux is nil")
	}
	if app.Registry() == nil {
		t.Error("expected non-nil default registry when config.Registry is nil")
	}
	if app.StreamRegistry() == nil {
		t.Error("expected non-nil default stream registry when config.StreamRegistry is nil")
	}
}

func TestNewApp_HealthTrackerSharing(t *testing.T) {
	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
	}

	app, err := NewApp(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}

	// StreamRegistry and Registry should share the same health tracker.
	// We verify by checking that the stream registry's health tracker
	// is the same object as the registry's health tracker.
	// This is tested indirectly: marking a plugin unhealthy in one
	// registry should be visible in the other.
	reg := app.Registry()
	sreg := app.StreamRegistry()

	// Get the health trackers via the exposed internal fields.
	// Both registries are in the engine package, so we can read unexported fields.
	if reg.healthTracker != sreg.healthTracker {
		t.Error("expected shared health tracker between registries")
	}
}

func TestNewApp_CustomLogger(t *testing.T) {
	custom := slog.New(slog.DiscardHandler)
	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
		Logger:        custom,
	}

	app, err := NewApp(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if app.Logger() != custom {
		t.Error("expected custom logger to be used")
	}
}

func TestNewApp_CustomMux(t *testing.T) {
	custom := http.NewServeMux()
	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
		Mux:           custom,
	}

	app, err := NewApp(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if app.Mux() != custom {
		t.Error("expected custom mux to be used")
	}
}

func TestNewApp_CustomRegistry(t *testing.T) {
	customReg := NewPluginRegistry()
	customStreamReg := NewPluginStreamRegistry()

	config := AppConfig{
		Runtime:        &Runtime{},
		StoreFactory:   &mockStoreFactory{},
		ServiceCaller:  &mockServiceCaller{},
		Registry:       customReg,
		StreamRegistry: customStreamReg,
	}

	app, err := NewApp(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if app.Registry() != customReg {
		t.Error("expected custom registry to be used")
	}
	if app.StreamRegistry() != customStreamReg {
		t.Error("expected custom stream registry to be used")
	}
}

// ---------------------------------------------------------------------------
// Plugin initialization tests
// ---------------------------------------------------------------------------

func TestNewApp_PluginInitSuccess(t *testing.T) {
	mp := &mockPlugin{info: plugin.PluginInfo{Name: "test-plugin"}}
	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
		Plugins:       []plugin.Plugin{mp},
	}

	app, err := NewApp(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if !mp.initCalled {
		t.Error("expected plugin Init to be called")
	}
	if len(app.plugins) != 1 || app.plugins[0] != mp {
		t.Error("expected plugin in app.plugins")
	}
}

func TestNewApp_PluginInitError(t *testing.T) {
	wantErr := errors.New("init boom")
	mp := &mockPluginCloseable{
		mockPlugin: &mockPlugin{
			info:    plugin.PluginInfo{Name: "bad-plugin"},
			initErr: wantErr,
		},
	}

	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
		Plugins:       []plugin.Plugin{mp},
	}

	app, err := NewApp(context.Background(), config)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bad-plugin") {
		t.Errorf("error %q should contain plugin name", err.Error())
	}
	if app != nil {
		t.Error("expected nil app on init error")
	}
	// closePlugins should NOT be called for the failing plugin because
	// it was never added to a.plugins.
	if mp.closed {
		t.Error("failing plugin should not be closed (never added to plugins list)")
	}
}

func TestNewApp_PluginInitError_CleansUpPrevious(t *testing.T) {
	// Plugin 1 succeeds, Plugin 2 fails. Plugin 1 should be closed.
	p1 := &mockPluginCloseable{
		mockPlugin: &mockPlugin{info: plugin.PluginInfo{Name: "ok-plugin"}},
	}
	p2 := &mockPlugin{
		info:    plugin.PluginInfo{Name: "bad-plugin"},
		initErr: errors.New("boom"),
	}

	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
		Plugins:       []plugin.Plugin{p1, p2},
	}

	_, err := NewApp(context.Background(), config)
	if err == nil {
		t.Fatal("expected error")
	}
	if !p1.closed {
		t.Error("previously initialized plugin should be closed on subsequent init failure")
	}
}

func TestNewApp_HasHostFunctions(t *testing.T) {
	mockFn := func(ctx context.Context, inputJSON string) (string, error) {
		return "ok", nil
	}

	mp := &mockPluginHF{
		mockPlugin: &mockPlugin{info: plugin.PluginInfo{Name: "hf-plugin"}},
		hostFuncs: []hostFuncEntry{
			{
				opts: plugin.FuncOptions{Name: "myFunc", Idempotent: false},
				fn:   mockFn,
			},
		},
	}

	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
		Plugins:       []plugin.Plugin{mp},
	}

	app, err := NewApp(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}

	// Verify function was registered in the registry.
	gotFn, idempotent, ok := app.Registry().Lookup("hf-plugin", "myFunc")
	if !ok {
		t.Fatal("expected function to be registered")
	}
	if idempotent {
		t.Error("expected non-idempotent registration")
	}

	// The function is wrapped (recovery wrapper). Call it to verify delegation.
	result, err := gotFn(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if result != "ok" {
		t.Errorf("expected 'ok', got %q", result)
	}
}

func TestNewApp_HasHostFunctionsError(t *testing.T) {
	// Plugin 1 succeeds, Plugin 2 has HasHostFunctions that fails.
	p1 := &mockPluginCloseable{
		mockPlugin: &mockPlugin{info: plugin.PluginInfo{Name: "ok-plugin"}},
	}
	p2 := &mockPluginHF{
		mockPlugin:   &mockPlugin{info: plugin.PluginInfo{Name: "hf-err-plugin"}},
		hostFuncsErr: errors.New("register boom"),
	}

	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
		Plugins:       []plugin.Plugin{p1, p2},
	}

	_, err := NewApp(context.Background(), config)
	if err == nil {
		t.Fatal("expected error from RegisterHostFunctions")
	}
	if !strings.Contains(err.Error(), "hf-err-plugin") {
		t.Errorf("error %q should contain plugin name", err.Error())
	}
	if !p1.closed {
		t.Error("previously initialized plugin should be closed on host function registration failure")
	}
}

func TestNewApp_MultiplePlugins(t *testing.T) {
	p1 := &mockPlugin{info: plugin.PluginInfo{Name: "plugin-1"}}
	p2 := &mockPlugin{info: plugin.PluginInfo{Name: "plugin-2"}}

	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
		Plugins:       []plugin.Plugin{p1, p2},
	}

	app, err := NewApp(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if len(app.plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(app.plugins))
	}
	if !p1.initCalled {
		t.Error("plugin 1 Init not called")
	}
	if !p2.initCalled {
		t.Error("plugin 2 Init not called")
	}
}

func TestNewApp_PluginConfig(t *testing.T) {
	pluginConfig := json.RawMessage(`{"key":"value"}`)
	mp := &mockPluginHF{
		mockPlugin: &mockPlugin{info: plugin.PluginInfo{Name: "cfg-plugin"}},
	}

	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
		Plugins:       []plugin.Plugin{mp},
		PluginConfigs: map[string]json.RawMessage{
			"cfg-plugin": pluginConfig,
		},
	}

	_, err := NewApp(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}

	if string(mp.initEnv.Config) != string(pluginConfig) {
		t.Errorf("expected config %q, got %q", pluginConfig, mp.initEnv.Config)
	}
	if mp.initEnv.Logger == nil {
		t.Error("expected logger in environment")
	}
}

// ---------------------------------------------------------------------------
// Close tests
// ---------------------------------------------------------------------------

func TestApp_Close(t *testing.T) {
	mp := &mockPluginCloseable{
		mockPlugin: &mockPlugin{info: plugin.PluginInfo{Name: "closeable"}},
	}

	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
		Plugins:       []plugin.Plugin{mp},
	}

	app, err := NewApp(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}

	err = app.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !mp.closed {
		t.Error("expected Close to be called on closeable plugin")
	}
}

func TestClosePlugins_ReverseOrder(t *testing.T) {
	var closeOrder []string

	c1 := &closeOrderTracker{name: "plugin-1", order: &closeOrder}
	c2 := &closeOrderTracker{name: "plugin-2", order: &closeOrder}

	app := &App{
		plugins: []plugin.Plugin{c1, c2},
	}

	err := app.closePlugins()
	if err != nil {
		t.Fatal(err)
	}

	if len(closeOrder) != 2 {
		t.Fatalf("expected 2 closes, got %d", len(closeOrder))
	}
	// Reverse init order: plugin-2 (last) should close first.
	if closeOrder[0] != "plugin-2" {
		t.Errorf("expected plugin-2 to close first, got %q", closeOrder[0])
	}
	if closeOrder[1] != "plugin-1" {
		t.Errorf("expected plugin-1 to close second, got %q", closeOrder[1])
	}
}

type closeOrderTracker struct {
	name  string
	order *[]string
}

func (c *closeOrderTracker) Info() plugin.PluginInfo {
	return plugin.PluginInfo{Name: c.name}
}
func (c *closeOrderTracker) Init(ctx context.Context, env *plugin.Environment) error {
	return nil
}
func (c *closeOrderTracker) Close() error {
	*c.order = append(*c.order, c.name)
	return nil
}

func TestClosePlugins_LastError(t *testing.T) {
	wantErr := errors.New("close boom")
	p1 := &mockPluginFull{
		mockPlugin: &mockPlugin{info: plugin.PluginInfo{Name: "err-plugin"}},
		closeErr:   wantErr,
	}
	p2 := &mockPluginFull{
		mockPlugin: &mockPlugin{info: plugin.PluginInfo{Name: "ok-plugin"}},
	}

	app := &App{
		plugins: []plugin.Plugin{p1, p2},
	}

	err := app.closePlugins()
	if err != wantErr {
		t.Errorf("expected last error %v, got %v", wantErr, err)
	}
	// Both should still be closed even if first one errors.
	if !p2.closed {
		t.Error("second plugin should still be closed after first errors")
	}
	if !p1.closed {
		t.Error("first plugin should be closed even though it errors")
	}
}

func TestClosePlugins_NoErrors(t *testing.T) {
	p1 := &mockPluginFull{
		mockPlugin: &mockPlugin{info: plugin.PluginInfo{Name: "p1"}},
	}
	p2 := &mockPluginFull{
		mockPlugin: &mockPlugin{info: plugin.PluginInfo{Name: "p2"}},
	}

	app := &App{
		plugins: []plugin.Plugin{p1, p2},
	}

	err := app.closePlugins()
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if !p1.closed || !p2.closed {
		t.Error("expected both plugins to be closed")
	}
}

func TestClosePlugins_NoCloseablePlugins(t *testing.T) {
	// mockPlugin doesn't implement CloseablePlugin.
	p1 := &mockPlugin{info: plugin.PluginInfo{Name: "basic-1"}}
	p2 := &mockPlugin{info: plugin.PluginInfo{Name: "basic-2"}}

	app := &App{
		plugins: []plugin.Plugin{p1, p2},
	}

	err := app.closePlugins()
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestClosePlugins_EmptyList(t *testing.T) {
	app := &App{
		plugins: nil,
	}
	if err := app.closePlugins(); err != nil {
		t.Errorf("expected nil error for empty list, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// appFuncRegistry tests
// ---------------------------------------------------------------------------

func TestAppFuncRegistry_Register(t *testing.T) {
	pr := NewPluginRegistry()
	scope := &appFuncRegistry{
		pluginName: "test-plugin",
		registry:   pr,
	}

	fn := func(ctx context.Context, inputJSON string) (string, error) {
		return "hello", nil
	}

	err := scope.Register(plugin.FuncOptions{Name: "myFunc", Idempotent: false}, fn)
	if err != nil {
		t.Fatal(err)
	}

	if !pr.Has("test-plugin", "myFunc") {
		t.Error("expected function to be registered")
	}

	gotFn, idempotent, ok := pr.Lookup("test-plugin", "myFunc")
	if !ok {
		t.Fatal("expected function to be found")
	}
	if idempotent {
		t.Error("expected idempotent=false")
	}
	// The registered fn is wrapped. Call the wrapper to ensure it delegates.
	result, err := gotFn(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if result != "hello" {
		t.Errorf("expected 'hello', got %q", result)
	}
}

func TestAppFuncRegistry_RegisterIdempotent(t *testing.T) {
	pr := NewPluginRegistry()
	scope := &appFuncRegistry{
		pluginName: "test-plugin",
		registry:   pr,
	}

	fn := func(ctx context.Context, inputJSON string) (string, error) {
		return "idem", nil
	}

	err := scope.Register(plugin.FuncOptions{Name: "idemFunc", Idempotent: true}, fn)
	if err != nil {
		t.Fatal(err)
	}

	_, idempotent, ok := pr.Lookup("test-plugin", "idemFunc")
	if !ok {
		t.Fatal("expected function to be found")
	}
	if !idempotent {
		t.Error("expected idempotent=true")
	}
}

// ---------------------------------------------------------------------------
// Accessor tests
// ---------------------------------------------------------------------------

func TestApp_Accessors(t *testing.T) {
	customLogger := slog.New(slog.DiscardHandler)
	customMux := http.NewServeMux()

	config := AppConfig{
		Runtime:       &Runtime{},
		StoreFactory:  &mockStoreFactory{},
		ServiceCaller: &mockServiceCaller{},
		Logger:        customLogger,
		Mux:           customMux,
	}

	app, err := NewApp(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}

	if app.Registry() == nil {
		t.Error("Registry() returned nil")
	}
	if app.StreamRegistry() == nil {
		t.Error("StreamRegistry() returned nil")
	}
	if app.Logger() != customLogger {
		t.Error("Logger() returned wrong logger")
	}
	if app.Mux() != customMux {
		t.Error("Mux() returned wrong mux")
	}
}
