package engine

// scopedDeploymentSecrets (plugin_secrets.go): a plugin that declares
// plugin.HasDeploymentSecretPrefix gets a DeploymentSecrets that refuses to
// Get anything outside its own prefix, rather than the one unscoped adapter
// every plugin otherwise shares. cleat-review's #2202 pass, "least privilege
// at almost no cost". This proves the refusal actually fires, both
// directions -- a name inside the prefix must still work.

import (
	"context"
	"errors"
	"testing"
)

type fakeInnerDeploymentSecrets struct {
	values map[string]string
}

func (f *fakeInnerDeploymentSecrets) Get(ctx context.Context, name string) (string, error) {
	if v, ok := f.values[name]; ok {
		return v, nil
	}
	return "", errors.New("deployment secret not found")
}

func TestScopedDeploymentSecretsRefusesANameOutsideItsPrefix(t *testing.T) {
	inner := &fakeInnerDeploymentSecrets{values: map[string]string{
		"email.sendgrid_api_key":       "sg-real",
		"llm.providers.openai.api_key": "sk-real",
	}}
	scoped := NewScopedPluginDeploymentSecrets(inner, "email.")

	// Known-positive first: prove the fake itself has the value a leaking
	// Get would return, so a pass below is not just "inner has nothing to
	// leak".
	if v, err := inner.Get(context.Background(), "llm.providers.openai.api_key"); err != nil || v != "sk-real" {
		t.Fatalf("known-positive: inner.Get(llm...) = (%q, %v), want (\"sk-real\", nil)", v, err)
	}

	_, err := scoped.Get(context.Background(), "llm.providers.openai.api_key")
	if err == nil {
		t.Fatal("scoped.Get(llm.providers.openai.api_key) under an \"email.\" prefix succeeded; " +
			"it should refuse a name belonging to another plugin")
	}
}

func TestScopedDeploymentSecretsAllowsANameInsideItsPrefix(t *testing.T) {
	inner := &fakeInnerDeploymentSecrets{values: map[string]string{
		"email.sendgrid_api_key": "sg-real",
	}}
	scoped := NewScopedPluginDeploymentSecrets(inner, "email.")

	got, err := scoped.Get(context.Background(), "email.sendgrid_api_key")
	if err != nil {
		t.Fatalf("scoped.Get(email.sendgrid_api_key) under its own prefix: %v", err)
	}
	if got != "sg-real" {
		t.Fatalf("scoped.Get(email.sendgrid_api_key) = %q, want %q", got, "sg-real")
	}
}
