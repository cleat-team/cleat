package main

// The worker's startup check for required deployment secrets (cleat#1992
// part 1). See checkRequiredDeploymentSecrets (setup.go) for why it runs
// after the plugin Init loop rather than inside it.
//
// Both directions, per the coordinator's review: present means start,
// missing means refuse. A known-positive matters here specifically, per
// CLAUDE.md's "Is this result real?" -- a check with no case that can fail
// is not a check.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

type fakeDeploymentSecretGetter struct {
	values map[string]string
}

func (f *fakeDeploymentSecretGetter) GetDeploymentSecret(ctx context.Context, name string) (string, error) {
	if v, ok := f.values[name]; ok {
		return v, nil
	}
	return "", errors.New("deployment secret not found")
}

// requiringPlugin is a minimal plugin.Plugin + plugin.HasRequiredDeploymentSecrets
// for exercising checkRequiredDeploymentSecrets without a real plugin.
type requiringPlugin struct {
	name  string
	names []string
	err   error
}

func (p *requiringPlugin) Info() plugin.PluginInfo                                 { return plugin.PluginInfo{Name: p.name} }
func (p *requiringPlugin) Init(ctx context.Context, env *plugin.Environment) error { return nil }
func (p *requiringPlugin) RequiredDeploymentSecrets(config []byte) ([]string, error) {
	return p.names, p.err
}

func healthyLoadedPlugin(p plugin.Plugin) *plugin.LoadedPlugin {
	return &plugin.LoadedPlugin{Plugin: p, Healthy: true}
}

func TestCheckRequiredDeploymentSecretsRefusesWhenMissing(t *testing.T) {
	p := &requiringPlugin{name: "email-notify", names: []string{"email.sendgrid_api_key"}}
	store := &fakeDeploymentSecretGetter{values: map[string]string{}}

	err := checkRequiredDeploymentSecrets(context.Background(), []*plugin.LoadedPlugin{healthyLoadedPlugin(p)}, nil, store)
	if err == nil {
		t.Fatal("expected a refusal when the required deployment secret is not set")
	}
	if !strings.Contains(err.Error(), "email.sendgrid_api_key") {
		t.Errorf("error does not name the missing secret: %v", err)
	}
	if !strings.Contains(err.Error(), "email-notify") {
		t.Errorf("error does not name the plugin: %v", err)
	}
}

func TestCheckRequiredDeploymentSecretsStartsWhenPresent(t *testing.T) {
	p := &requiringPlugin{name: "email-notify", names: []string{"email.sendgrid_api_key"}}
	store := &fakeDeploymentSecretGetter{values: map[string]string{"email.sendgrid_api_key": "sk-real"}}

	if err := checkRequiredDeploymentSecrets(context.Background(), []*plugin.LoadedPlugin{healthyLoadedPlugin(p)}, nil, store); err != nil {
		t.Fatalf("expected no error when every required secret resolves, got: %v", err)
	}
}

// TestCheckRequiredDeploymentSecretsIgnoresAnUnhealthyPlugin proves the
// check does not itself decide enablement -- Init already did, via
// ErrNotConfigured -- by giving it a plugin whose secret would fail the
// lookup and marking it unhealthy: a pass here would be worthless if the
// check could not tell a disabled plugin from an enabled-but-broken one.
func TestCheckRequiredDeploymentSecretsIgnoresAnUnhealthyPlugin(t *testing.T) {
	p := &requiringPlugin{name: "email-notify", names: []string{"email.sendgrid_api_key"}}
	lp := healthyLoadedPlugin(p)
	lp.Healthy = false
	store := &fakeDeploymentSecretGetter{values: map[string]string{}}

	if err := checkRequiredDeploymentSecrets(context.Background(), []*plugin.LoadedPlugin{lp}, nil, store); err != nil {
		t.Fatalf("an unhealthy plugin's required secrets must not be checked, got: %v", err)
	}
}

// plainPlugin implements plugin.Plugin only -- not
// plugin.HasRequiredDeploymentSecrets -- covering the common case: most
// plugins never opt into this check at all.
type plainPlugin struct{ name string }

func (p *plainPlugin) Info() plugin.PluginInfo                                 { return plugin.PluginInfo{Name: p.name} }
func (p *plainPlugin) Init(ctx context.Context, env *plugin.Environment) error { return nil }

func TestCheckRequiredDeploymentSecretsIgnoresAPluginThatDoesNotImplementIt(t *testing.T) {
	// The type assertion inside the function under test must fail closed to
	// "skip", not panic or refuse, for the ordinary case of a plugin that
	// carries no required deployment secrets at all.
	lp := &plugin.LoadedPlugin{Plugin: &plainPlugin{name: "scheduler"}, Healthy: true}
	store := &fakeDeploymentSecretGetter{values: map[string]string{}}

	if err := checkRequiredDeploymentSecrets(context.Background(), []*plugin.LoadedPlugin{lp}, nil, store); err != nil {
		t.Fatalf("a plugin that does not implement HasRequiredDeploymentSecrets must be skipped, got: %v", err)
	}
}

func TestCheckRequiredDeploymentSecretsPropagatesADeterminationError(t *testing.T) {
	p := &requiringPlugin{name: "llm", err: errors.New("invalid config")}
	store := &fakeDeploymentSecretGetter{values: map[string]string{}}

	err := checkRequiredDeploymentSecrets(context.Background(), []*plugin.LoadedPlugin{healthyLoadedPlugin(p)}, nil, store)
	if err == nil {
		t.Fatal("expected an error when RequiredDeploymentSecrets itself errors")
	}
	if !strings.Contains(err.Error(), "llm") || !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("error does not carry the plugin name and underlying cause: %v", err)
	}
}
