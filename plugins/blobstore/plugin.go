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
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rcownie/durable/internal/plugin"
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

// Plugin implements content-addressed blob storage with tenant isolation.
type Plugin struct {
	db      *sql.DB
	mux     *http.ServeMux
	logger  *slog.Logger
	config  Config
	backend Backend
}

// Config controls blobstore backend selection and S3 parameters.
type Config struct {
	Backend         string `json:"backend"`                     // "s3" or "memory"; defaults to "memory"
	Bucket          string `json:"bucket"`                      // S3 bucket name (for s3 backend)
	Region          string `json:"region"`                      // AWS region (for s3 backend)
	Endpoint        string `json:"endpoint,omitempty"`          // custom S3 endpoint (for MinIO/GCS)
	AccessKeyID     string `json:"access_key_id,omitempty"`     // S3 access key; falls back to env/instance profile
	SecretAccessKey string `json:"secret_access_key,omitempty"` // S3 secret key
	MaxBlobSize     int64  `json:"max_blob_size"`               // max blob bytes; default 10 MB
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

	// Parse config. If no config provided, use safe defaults.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("blobstore: invalid config: %w", err)
		}
	}
	if p.config.Backend == "" {
		p.config.Backend = "memory" // safe default for dev/testing
	}

	// Set up the storage backend.
	switch p.config.Backend {
	case "s3":
		s3Backend, err := newS3Backend(ctx, p.config)
		if err != nil {
			return fmt.Errorf("blobstore: s3 backend: %w", err)
		}
		p.backend = s3Backend
	default:
		p.backend = newMemoryBackend(p.db)
	}

	p.logger.Info("blobstore: initialized",
		"backend", p.config.Backend,
	)
	return nil
}
