package engine

import (
	"context"
	"testing"
)

func TestEnvCredentialProvider_DBFlag(t *testing.T) {
	p := NewEnvCredentialProvider("postgres://user:pass@localhost/db")
	got, err := p.GetConnectionString(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "postgres://user:pass@localhost/db" {
		t.Errorf("expected dbURL, got %q", got)
	}
}

func TestEnvCredentialProvider_DATABASE_URL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://env-db/db")
	p := NewEnvCredentialProvider("")
	got, err := p.GetConnectionString(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "postgres://env-db/db" {
		t.Errorf("expected DATABASE_URL value, got %q", got)
	}
}

func TestEnvCredentialProvider_CLEAT_DATABASE_URL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CLEAT_DATABASE_URL", "postgres://cleat-db/db")
	p := NewEnvCredentialProvider("")
	got, err := p.GetConnectionString(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "postgres://cleat-db/db" {
		t.Errorf("expected CLEAT_DATABASE_URL value, got %q", got)
	}
}

func TestEnvCredentialProvider_NoURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CLEAT_DATABASE_URL", "")
	p := NewEnvCredentialProvider("")
	_, err := p.GetConnectionString(context.Background())
	if err == nil {
		t.Fatal("expected error when no URL is set, got nil")
	}
}

func TestEnvCredentialProvider_PriorityOrder(t *testing.T) {
	// --db flag wins over env vars.
	t.Setenv("DATABASE_URL", "postgres://env/db")
	t.Setenv("CLEAT_DATABASE_URL", "postgres://cleat/db")
	p := NewEnvCredentialProvider("postgres://flag/db")
	got, err := p.GetConnectionString(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "postgres://flag/db" {
		t.Errorf("expected --db flag to take priority, got %q", got)
	}

	// DATABASE_URL wins over CLEAT_DATABASE_URL.
	p2 := NewEnvCredentialProvider("")
	got2, err := p2.GetConnectionString(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got2 != "postgres://env/db" {
		t.Errorf("expected DATABASE_URL to take priority, got %q", got2)
	}

	// CLEAT_DATABASE_URL used when DATABASE_URL is empty.
	t.Setenv("DATABASE_URL", "")
	p3 := NewEnvCredentialProvider("")
	got3, err := p3.GetConnectionString(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got3 != "postgres://cleat/db" {
		t.Errorf("expected CLEAT_DATABASE_URL fallback, got %q", got3)
	}
}

func TestVaultCredentialProvider_New(t *testing.T) {
	p := NewVaultCredentialProvider("secret/cleat/db")
	if p == nil {
		t.Fatal("NewVaultCredentialProvider returned nil")
	}
	if p.credentialPath != "secret/cleat/db" {
		t.Errorf("expected credentialPath 'secret/cleat/db', got %q", p.credentialPath)
	}
}

func TestVaultCredentialProvider_EmptyPath(t *testing.T) {
	p := NewVaultCredentialProvider("")
	_, err := p.GetConnectionString(context.Background())
	if err == nil {
		t.Fatal("expected error for empty credential path")
	}
}

func TestVaultCredentialProvider_ContextCanceled(t *testing.T) {
	p := NewVaultCredentialProvider("secret/cleat/db")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.GetConnectionString(ctx)
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

func TestAWSSecretsManagerProvider_New(t *testing.T) {
	p := NewAWSSecretsManagerProvider("my-db-secret")
	if p == nil {
		t.Fatal("NewAWSSecretsManagerProvider returned nil")
	}
	if p.credentialPath != "my-db-secret" {
		t.Errorf("expected credentialPath 'my-db-secret', got %q", p.credentialPath)
	}
}

func TestAWSSecretsManagerProvider_EmptyPath(t *testing.T) {
	p := NewAWSSecretsManagerProvider("")
	_, err := p.GetConnectionString(context.Background())
	if err == nil {
		t.Fatal("expected error for empty credential path")
	}
}

func TestAWSSecretsManagerProvider_ContextCanceled(t *testing.T) {
	p := NewAWSSecretsManagerProvider("my-db-secret")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.GetConnectionString(ctx)
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

func TestNewDBCredentialProvider_Env_Defaults(t *testing.T) {
	// Empty string defaults to env provider.
	p, err := NewDBCredentialProvider("", "postgres://flag/db", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, ok := p.(*EnvCredentialProvider)
	if !ok {
		t.Errorf("expected *EnvCredentialProvider, got %T", p)
	}

	// Explicit "env" provider.
	p2, err := NewDBCredentialProvider("env", "postgres://flag/db", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, ok = p2.(*EnvCredentialProvider)
	if !ok {
		t.Errorf("expected *EnvCredentialProvider, got %T", p2)
	}
}

func TestNewDBCredentialProvider_Vault(t *testing.T) {
	p, err := NewDBCredentialProvider("vault", "", "secret/cleat/db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	vp, ok := p.(*VaultCredentialProvider)
	if !ok {
		t.Fatalf("expected *VaultCredentialProvider, got %T", p)
	}
	if vp.credentialPath != "secret/cleat/db" {
		t.Errorf("expected credentialPath 'secret/cleat/db', got %q", vp.credentialPath)
	}
}

func TestNewDBCredentialProvider_AWS(t *testing.T) {
	p, err := NewDBCredentialProvider("aws-secrets-manager", "", "my-db-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ap, ok := p.(*AWSSecretsManagerProvider)
	if !ok {
		t.Fatalf("expected *AWSSecretsManagerProvider, got %T", p)
	}
	if ap.credentialPath != "my-db-secret" {
		t.Errorf("expected credentialPath 'my-db-secret', got %q", ap.credentialPath)
	}
}

func TestNewDBCredentialProvider_VaultMissingPath(t *testing.T) {
	_, err := NewDBCredentialProvider("vault", "", "")
	if err == nil {
		t.Fatal("expected error for vault with empty path")
	}
}

func TestNewDBCredentialProvider_AWSMissingPath(t *testing.T) {
	_, err := NewDBCredentialProvider("aws-secrets-manager", "", "")
	if err == nil {
		t.Fatal("expected error for aws-secrets-manager with empty path")
	}
}

func TestNewDBCredentialProvider_Unknown(t *testing.T) {
	_, err := NewDBCredentialProvider("invalid-provider", "", "")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestNewDBCredentialProvider_EnvIgnoresCredentialPath(t *testing.T) {
	// The env provider doesn't need credentialPath; it should be ignored.
	p, err := NewDBCredentialProvider("env", "postgres://db", "some-path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, ok := p.(*EnvCredentialProvider)
	if !ok {
		t.Errorf("expected *EnvCredentialProvider, got %T", p)
	}
}
