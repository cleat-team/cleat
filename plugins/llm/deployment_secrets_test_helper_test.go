package llm

import (
	"context"
	"fmt"
)

// fakeDeploymentSecrets is a plugin.DeploymentSecrets stand-in for tests:
// it answers a fixed value for each "llm.providers.<provider>.api_key" name
// it was seeded with, and ErrDeploymentSecretNotFound-shaped error for
// anything else -- mirroring engine.ErrDeploymentSecretNotFound's message
// shape without importing engine, which plugin code (and its tests) must
// not (see plugin.DeploymentSecrets' own doc comment).
type fakeDeploymentSecrets struct {
	// keyed by provider name, not by the full deployment-secret name, so
	// callers read naturally: newFakeProviderKeys(map[string]string{"openai": "sk-test"}).
	byProvider map[string]string
}

// newFakeProviderKeys builds a fakeDeploymentSecrets from provider name ->
// API key. Every provider not present answers "not found", the same as an
// unset deployment secret would.
func newFakeProviderKeys(byProvider map[string]string) *fakeDeploymentSecrets {
	return &fakeDeploymentSecrets{byProvider: byProvider}
}

func (f *fakeDeploymentSecrets) Get(ctx context.Context, name string) (string, error) {
	for provider, key := range f.byProvider {
		if name == "llm.providers."+provider+".api_key" {
			return key, nil
		}
	}
	return "", fmt.Errorf("fakeDeploymentSecrets: %q not set", name)
}
