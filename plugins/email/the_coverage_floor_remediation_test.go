package email

import "testing"

// cleat#2487. The three plugin-metadata functions below had no direct test
// (TestInfo and friends above construct a *Plugin by literal), and
// extractMessageID's lowercase and miss branches had no test, only the
// canonical-case path exercised indirectly by TestCheckStatus* elsewhere.

func TestNewReturnsAUsablePlugin(t *testing.T) {
	p := New()
	if p == nil {
		t.Fatal("New() returned nil")
	}
	if _, ok := p.(*Plugin); !ok {
		t.Fatalf("New() returned %T, want *Plugin", p)
	}
}

func TestRequiredDeploymentSecrets(t *testing.T) {
	p := &Plugin{}
	got, err := p.RequiredDeploymentSecrets(nil)
	if err != nil {
		t.Fatalf("RequiredDeploymentSecrets returned error: %v", err)
	}
	if len(got) != 1 || got[0] != "email.sendgrid_api_key" {
		t.Fatalf("RequiredDeploymentSecrets() = %v, want [email.sendgrid_api_key]", got)
	}
}

func TestDeploymentSecretPrefix(t *testing.T) {
	p := &Plugin{}
	if got := p.DeploymentSecretPrefix(); got != "email." {
		t.Fatalf("DeploymentSecretPrefix() = %q, want %q", got, "email.")
	}
}

func TestExtractMessageID(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string][]string
		want    string
	}{
		{"canonical", map[string][]string{"X-Message-Id": {"abc123"}}, "abc123"},
		{"lowercase fallback", map[string][]string{"x-message-id": {"def456"}}, "def456"},
		{"missing", map[string][]string{"Content-Type": {"application/json"}}, ""},
		{"empty slice, canonical", map[string][]string{"X-Message-Id": {}}, ""},
		{"nil headers", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractMessageID(c.headers); got != c.want {
				t.Errorf("extractMessageID(%v) = %q, want %q", c.headers, got, c.want)
			}
		})
	}
}
