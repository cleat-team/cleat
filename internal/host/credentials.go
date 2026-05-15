package host

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DBCredentialProvider resolves a database connection string from a configured
// source. Implementations may read from environment variables, a secrets store,
// or a CLI tool.
type DBCredentialProvider interface {
	// GetConnectionString returns the database connection string. If the
	// provider cannot resolve it the returned error explains why.
	GetConnectionString(ctx context.Context) (string, error)
}

// ---- EnvCredentialProvider ----

// EnvCredentialProvider resolves the connection string from the --db flag,
// then the DATABASE_URL env var, then the CLEAT_DATABASE_URL env var.
// This is the default provider used when no --db-credential-provider is set.
type EnvCredentialProvider struct {
	dbURL string
}

// NewEnvCredentialProvider creates an EnvCredentialProvider. The dbURL
// argument is the value of the --db flag (may be empty).
func NewEnvCredentialProvider(dbURL string) *EnvCredentialProvider {
	return &EnvCredentialProvider{dbURL: dbURL}
}

// GetConnectionString resolves the connection string by checking the --db
// flag first, then DATABASE_URL, then CLEAT_DATABASE_URL.
func (p *EnvCredentialProvider) GetConnectionString(_ context.Context) (string, error) {
	if p.dbURL != "" {
		return p.dbURL, nil
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v, nil
	}
	if v := os.Getenv("CLEAT_DATABASE_URL"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("no database connection string found: set --db, DATABASE_URL, or CLEAT_DATABASE_URL")
}

// ---- VaultCredentialProvider ----

// VaultCredentialProvider resolves the connection string by calling the
// HashiCorp Vault CLI:
//
//	vault kv get -field=connection_string <path>
type VaultCredentialProvider struct {
	credentialPath string
}

// NewVaultCredentialProvider creates a VaultCredentialProvider. The
// credentialPath is the Vault path to read (e.g., "secret/cleat/db").
func NewVaultCredentialProvider(credentialPath string) *VaultCredentialProvider {
	return &VaultCredentialProvider{credentialPath: credentialPath}
}

// GetConnectionString runs `vault kv get -field=connection_string <path>` and
// returns the connection string.
func (p *VaultCredentialProvider) GetConnectionString(ctx context.Context) (string, error) {
	if p.credentialPath == "" {
		return "", fmt.Errorf("vault credential provider: --db-credential-path is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "vault", "kv", "get", "-field=connection_string", p.credentialPath)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("vault credential provider: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// ---- AWSSecretsManagerProvider ----

// AWSSecretsManagerProvider resolves the connection string by calling the
// AWS CLI:
//
//	aws secretsmanager get-secret-value --secret-id <name> --query SecretString --output text
type AWSSecretsManagerProvider struct {
	credentialPath string
}

// NewAWSSecretsManagerProvider creates an AWSSecretsManagerProvider. The
// credentialPath is the secret name or ARN in AWS Secrets Manager.
func NewAWSSecretsManagerProvider(credentialPath string) *AWSSecretsManagerProvider {
	return &AWSSecretsManagerProvider{credentialPath: credentialPath}
}

// GetConnectionString runs `aws secretsmanager get-secret-value` and returns
// the connection string.
func (p *AWSSecretsManagerProvider) GetConnectionString(ctx context.Context) (string, error) {
	if p.credentialPath == "" {
		return "", fmt.Errorf("aws secrets manager provider: --db-credential-path is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "aws", "secretsmanager", "get-secret-value",
		"--secret-id", p.credentialPath,
		"--query", "SecretString",
		"--output", "text")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("aws secrets manager provider: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// ---- Factory ----

// NewDBCredentialProvider creates the appropriate DBCredentialProvider based on
// providerName. Supported values: "env" (default), "vault", "aws-secrets-manager".
// When providerName is "env", the dbURL argument is passed to the env provider.
// For "vault" and "aws-secrets-manager", credentialPath must be non-empty.
func NewDBCredentialProvider(providerName, dbURL, credentialPath string) (DBCredentialProvider, error) {
	switch providerName {
	case "", "env":
		return NewEnvCredentialProvider(dbURL), nil
	case "vault":
		if credentialPath == "" {
			return nil, fmt.Errorf("vault credential provider requires --db-credential-path")
		}
		return NewVaultCredentialProvider(credentialPath), nil
	case "aws-secrets-manager":
		if credentialPath == "" {
			return nil, fmt.Errorf("aws-secrets-manager credential provider requires --db-credential-path")
		}
		return NewAWSSecretsManagerProvider(credentialPath), nil
	default:
		return nil, fmt.Errorf("unknown credential provider %q: use env, vault, or aws-secrets-manager", providerName)
	}
}
