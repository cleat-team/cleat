// Package blobstore provides content-addressed blob storage with metadata
// queries, tenant isolation, and TTL-based expiry. It demonstrates all
// plugin API patterns: host functions, HTTP routes, database migrations,
// and background workers.
//
// Blobs are stored via a pluggable Backend interface. The memory backend
// stores bytes in the blob_content.data BYTEA column (dev/testing). The S3
// backend stores bytes in S3-compatible object storage; only metadata is kept
// in PostgreSQL. Switch backends via the "backend" config option.
package blobstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cleat-team/cleat/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "blobstore",
		Version:     "0.1.0",
		Description: "Content-addressed blob storage with metadata queries",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin implements content-addressed blob storage with tenant isolation.
type Plugin struct {
	db      plugin.PluginDB
	mux     *http.ServeMux
	logger  *slog.Logger
	config  Config
	backend Backend
	dialect plugin.Dialect

	deploymentSecrets plugin.DeploymentSecrets
}

// Config controls blobstore backend selection and S3 parameters.
//
// AccessKeyID and SecretAccessKey lived here until cleat#1992 part 1b moved
// them to deployment secrets ("blobstore.access_key_id",
// "blobstore.secret_access_key") -- see deploymentSecretsCredentialsProvider
// in backend.go, which fetches them per S3 request rather than caching them
// here.
type Config struct {
	Backend           string `json:"backend"`                      // "s3" or "memory"; defaults to "memory"
	Bucket            string `json:"bucket"`                       // S3 bucket name (for s3 backend)
	Region            string `json:"region"`                       // AWS region (for s3 backend)
	Endpoint          string `json:"endpoint,omitempty"`           // custom S3 endpoint (for MinIO/GCS)
	Secure            bool   `json:"secure"`                       // use HTTPS (default true, set false for local MinIO)
	MaxBlobSize       int64  `json:"max_blob_size"`                // max blob bytes; default 10 MB
	UseIAMCredentials bool   `json:"use_iam_credentials,omitempty"` // see doc comment on newS3Backend in backend.go
}

// legacyBlobstoreConfig catches access_key_id/secret_access_key left over in
// --plugin-config from before cleat#1992 part 1b. json.Unmarshal ignores
// fields a target struct does not declare, so a leftover value here silently
// stopped doing anything once Config dropped the fields -- no error, no log,
// just quietly wrong. This is unmarshaled from the same bytes purely to
// detect that and WARN; Config above no longer has anywhere to put either
// value even if this found one.
type legacyBlobstoreConfig struct {
	AccessKeyID     plugin.Secret `json:"access_key_id"`
	SecretAccessKey plugin.Secret `json:"secret_access_key"`
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "blobstore",
		Version:     "0.1.0",
		Description: "Content-addressed blob storage with metadata queries",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. It parses optional
// configuration and sets safe defaults for dev/testing.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.mux = env.Mux
	p.dialect = env.Dialect

	// Parse config. If no config provided, use safe defaults.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("blobstore: invalid config: %w", err)
		}
		var legacy legacyBlobstoreConfig
		if err := json.Unmarshal(env.Config, &legacy); err == nil &&
			(legacy.AccessKeyID != "" || legacy.SecretAccessKey != "") {
			p.logger.Warn("blobstore: access_key_id/secret_access_key in --plugin-config " +
				"are no longer read (cleat#1992 part 1b); they have no effect. Use " +
				"`cleatctl set-deployment-secret --name blobstore.access_key_id` and " +
				"`--name blobstore.secret_access_key` instead.")
		}
	}
	if p.config.Backend == "" {
		p.config.Backend = "memory" // safe default for dev/testing
	}

	p.deploymentSecrets = env.DeploymentSecrets

	// Set up the storage backend.
	switch p.config.Backend {
	case "s3":
		s3Backend, err := newS3Backend(ctx, p.config, p.deploymentSecrets)
		if err != nil {
			return fmt.Errorf("blobstore: s3 backend: %w", err)
		}
		p.backend = s3Backend
	default:
		p.backend = newMemoryBackend(p.db, p.dialect)
	}

	p.logger.Info("blobstore: initialized",
		"backend", p.config.Backend,
	)
	return nil
}

// DeploymentSecretPrefix implements plugin.HasDeploymentSecretPrefix:
// blobstore only ever reads "blobstore.access_key_id" and
// "blobstore.secret_access_key".
func (p *Plugin) DeploymentSecretPrefix() string {
	return "blobstore."
}
