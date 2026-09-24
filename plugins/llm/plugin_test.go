package llm

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "llm" {
		t.Errorf("expected Name 'llm', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected Version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Error("expected non-empty Description")
	}
}

func TestInit(t *testing.T) {
	p := &Plugin{}
	// The API key no longer lives in this JSON config (cleat#1992 part 1) --
	// see ProviderConfig's doc comment -- so this only checks the fields that
	// still do.
	cfg := `{"providers": {"openai": {"enabled": true}}}`
	secrets := newFakeProviderKeys(map[string]string{"openai": "sk-test"})
	env := &plugin.Environment{Config: []byte(cfg), DeploymentSecrets: secrets}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.httpClient == nil {
		t.Error("expected httpClient to be set")
	}
	if p.deploymentSecrets == nil {
		t.Error("expected deploymentSecrets to be set from env.DeploymentSecrets")
	}
	openaiCfg, ok := p.config.Providers["openai"]
	if !ok {
		t.Fatal("expected openai provider config")
	}
	if !openaiCfg.Enabled {
		t.Error("expected openai to be enabled")
	}
}

func TestInitNoConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() with no config returned error: %v", err)
	}
	if p.httpClient == nil {
		t.Error("expected httpClient to be set")
	}
}

func TestInitInvalidConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{Config: []byte(`not valid json`)}
	err := p.Init(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for invalid config, got nil")
	}
}

// ===========================================================================
// requires_deployment_key opt-out -- cleat-review's #2202 pass, GAP #2: an
// enabled provider unconditionally required "llm.providers.<provider>.api_key"
// at boot, which refused the worker for a keyless self-hosted base_url or a
// BYOK-only provider (cleat#1988) that would never look one up.
// ===========================================================================

// TestRequiredDeploymentSecretsOmitsAProviderThatOptsOut is the boot-time
// half: RequiredDeploymentSecrets must not demand a key for a provider that
// sets "requires_deployment_key": false, while an ordinary enabled provider
// (omitted field, defaults to required) still appears.
func TestRequiredDeploymentSecretsOmitsAProviderThatOptsOut(t *testing.T) {
	p := &Plugin{}
	cfg := []byte(`{"providers": {
		"openai": {"enabled": true},
		"vllm": {"enabled": true, "base_url": "http://localhost:8000", "requires_deployment_key": false}
	}}`)
	names, err := p.RequiredDeploymentSecrets(cfg)
	if err != nil {
		t.Fatalf("RequiredDeploymentSecrets: %v", err)
	}
	foundOpenAI, foundVLLM := false, false
	for _, n := range names {
		switch n {
		case "llm.providers.openai.api_key":
			foundOpenAI = true
		case "llm.providers.vllm.api_key":
			foundVLLM = true
		}
	}
	if !foundOpenAI {
		t.Errorf("expected llm.providers.openai.api_key to still be required (no opt-out), got names=%v", names)
	}
	if foundVLLM {
		t.Errorf("expected llm.providers.vllm.api_key to be OMITTED (requires_deployment_key: false), got names=%v", names)
	}
}

// TestProviderAPIKeySkipsLookupWhenNotRequired is the call-time half: a
// provider that opted out of a deployment key must never call
// deploymentSecrets.Get at all -- not "get and ignore a not-found", an
// outright skip, the same treatment ollama already gets.
func TestProviderAPIKeySkipsLookupWhenNotRequired(t *testing.T) {
	no := false
	p := &Plugin{
		config: Config{Providers: map[string]ProviderConfig{
			"vllm": {Enabled: true, BaseURL: "http://localhost:8000", RequiresDeploymentKey: &no},
		}},
		// No deploymentSecrets configured at all -- a lookup that ran despite
		// the opt-out would fail with "no deployment secret store configured"
		// rather than silently returning "", proving the skip actually
		// short-circuits before that point rather than merely swallowing the
		// error.
	}
	key, err := p.providerAPIKey(context.Background(), "vllm")
	if err != nil {
		t.Fatalf("providerAPIKey: %v", err)
	}
	if key != "" {
		t.Errorf("providerAPIKey for an opted-out provider = %q, want \"\"", key)
	}
}

// TestProviderAPIKeyStillLooksUpByDefault is the negative control: omitting
// requires_deployment_key must still look the key up (today's behavior for
// every enabled non-ollama provider), so the two tests above are exercising
// a real opt-out rather than a providerAPIKey that stopped looking anything
// up at all.
func TestProviderAPIKeyStillLooksUpByDefault(t *testing.T) {
	p := &Plugin{
		config:            Config{Providers: map[string]ProviderConfig{"openai": {Enabled: true}}},
		deploymentSecrets: newFakeProviderKeys(map[string]string{"openai": "sk-configured"}),
	}
	key, err := p.providerAPIKey(context.Background(), "openai")
	if err != nil {
		t.Fatalf("providerAPIKey: %v", err)
	}
	if key != "sk-configured" {
		t.Errorf("providerAPIKey = %q, want %q", key, "sk-configured")
	}
}

// TestInitWarnsOnLeftoverProviderAPIKey covers legacyProviderConfig's WARN:
// an api_key left over under providers.<name> in --plugin-config from before
// cleat#1992 part 1 does nothing (ProviderConfig has no field for it any
// more) and used to do so silently.
func TestInitWarnsOnLeftoverProviderAPIKey(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{"providers":{"openai":{"enabled":true,"api_key":"sk-leftover-plaintext"}}}`),
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "providers.openai.api_key") {
		t.Errorf("expected a WARN naming the leftover providers.openai.api_key, got log output: %q", got)
	}
	if !strings.Contains(got, "set-deployment-secret") {
		t.Errorf("expected the WARN to name the replacement command, got log output: %q", got)
	}
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("expected the leftover-key message at WARN level, got log output: %q", got)
	}
}

// TestInitNoWarnWithoutLeftoverProviderAPIKey is the negative control: a
// config with no api_key field at all must not mention one.
func TestInitNoWarnWithoutLeftoverProviderAPIKey(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{"providers":{"openai":{"enabled":true}}}`),
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if got := buf.String(); strings.Contains(got, "api_key") {
		t.Errorf("did not expect an api_key WARN with no leftover key present, got log output: %q", got)
	}
}

func TestRegisterRoutes(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes() returned error: %v", err)
	}

	tests := []struct {
		method string
		path   string
	}{
		{"GET", "/api/llm/health"},
		{"GET", "/api/llm/models"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

func TestRegisterHostFunctions(t *testing.T) {
	p := &Plugin{}
	scope := &testFuncRegistry{funcs: make(map[string]plugin.FuncOptions)}
	if err := p.RegisterHostFunctions(scope); err != nil {
		t.Fatalf("RegisterHostFunctions() returned error: %v", err)
	}
	for _, name := range []string{"chat", "embed", "list_models"} {
		if _, ok := scope.funcs[name]; !ok {
			t.Errorf("expected function %q to be registered", name)
		}
	}
}

func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil scope, got nil")
	}
}

// testFuncRegistry is a minimal in-memory registry for testing.
type testFuncRegistry struct {
	funcs map[string]plugin.FuncOptions
}

func (r *testFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	r.funcs[opts.Name] = opts
	return nil
}

func TestPluginRegistration(t *testing.T) {
	plugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("Discover() returned error: %v", err)
	}
	found := false
	for _, lp := range plugins {
		if lp.Plugin.Info().Name == "llm" {
			found = true
			break
		}
	}
	if !found {
		t.Error("llm plugin not found after Discover")
	}
}

// errorRegistry returns an error on any Register call.
type errorRegistry struct {
	plugin.FuncRegistry
}

func (r *errorRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	return fmt.Errorf("register error: %s", opts.Name)
}

func TestRegisterHostFunctionsRegisterError(t *testing.T) {
	p := &Plugin{}
	scope := &errorRegistry{}
	err := p.RegisterHostFunctions(scope)
	if err == nil {
		t.Fatal("expected error from Register, got nil")
	}
}

// errorStreamRegistry returns an error only on RegisterStream.
type errorStreamRegistry struct {
	*testStreamRegistry
}

func (r *errorStreamRegistry) RegisterStream(opts plugin.FuncOptions, fn plugin.PluginStreamFunc) error {
	return fmt.Errorf("register stream error: %s", opts.Name)
}

func TestRegisterHostFunctionsStreamError(t *testing.T) {
	p := &Plugin{}
	scope := &errorStreamRegistry{newTestStreamRegistry()}
	err := p.RegisterHostFunctions(scope)
	if err == nil {
		t.Fatal("expected error from RegisterStream, got nil")
	}
	if !strings.Contains(err.Error(), "register stream error") {
		t.Errorf("expected register stream error, got: %v", err)
	}
}
