package pgvector

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
		Name:        "pgvector",
		Version:     "0.1.0",
		Description: "Vector similarity search via pgvector for cleat durable execution engine",
		Author:      "cleat",
	}, func() plugin.Plugin { return &Plugin{} })
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin implements vector similarity search using the pgvector PostgreSQL
// extension. Workflows can store and query embedding vectors with metadata.
type Plugin struct {
	db     plugin.PluginDB
	mux    *http.ServeMux
	logger *slog.Logger
	config Config
}

// Config controls pgvector plugin behavior.
type Config struct {
	EmbeddingProvider string `json:"embedding_provider"` // embedding provider name (e.g., "openai")
	EmbeddingModel    string `json:"embedding_model"`    // embedding model name (e.g., "text-embedding-3-small")
	Dimensions        int    `json:"dimensions"`         // vector dimensions (default 1536)
	DefaultCollection string `json:"default_collection"` // default collection to create on init (optional)
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "pgvector",
		Version:     "0.1.0",
		Description: "Vector similarity search via pgvector for cleat durable execution engine",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. It parses optional
// configuration, ensures the pgvector extension is installed, and optionally
// creates a default collection.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.mux = env.Mux

	// Set defaults.
	p.config.Dimensions = 1536

	// Parse config. If no config provided, use safe defaults.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("pgvector: invalid config: %w", err)
		}
	}

	// Ensure the pgvector extension is installed.
	if _, err := p.db.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("pgvector: create extension: %w", err)
	}

	// Create default collection if configured.
	if p.config.DefaultCollection != "" {
		_, err := p.db.Exec(ctx, `
			INSERT INTO pgvector_collections (name, dimensions, embedding_provider, embedding_model)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (name) DO NOTHING
		`, p.config.DefaultCollection, p.config.Dimensions, p.config.EmbeddingProvider, p.config.EmbeddingModel)
		if err != nil {
			return fmt.Errorf("pgvector: create default collection: %w", err)
		}
	}

	p.logger.Info("pgvector: initialized",
		"dimensions", p.config.Dimensions,
		"embedding_provider", p.config.EmbeddingProvider,
		"embedding_model", p.config.EmbeddingModel,
		"default_collection", p.config.DefaultCollection,
	)
	return nil
}

// Migrations returns the database schema for pgvector collections and
// embeddings. Tables are idempotent (IF NOT EXISTS) and safe to run
// multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS pgvector_collections (
					id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
					name               TEXT NOT NULL UNIQUE,
					dimensions         INTEGER NOT NULL DEFAULT 1536,
					embedding_provider TEXT NOT NULL DEFAULT '',
					embedding_model    TEXT NOT NULL DEFAULT '',
					created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE TABLE IF NOT EXISTS pgvector_embeddings (
					id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
					tenant_id       UUID NOT NULL,
					collection_id   UUID NOT NULL REFERENCES pgvector_collections(id) ON DELETE CASCADE,
					external_id     TEXT NOT NULL DEFAULT '',
					content         TEXT NOT NULL DEFAULT '',
					metadata        JSONB NOT NULL DEFAULT '{}',
					embedding       vector(1536),
					created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
					UNIQUE (tenant_id, collection_id, external_id)
				);

				CREATE INDEX IF NOT EXISTS idx_pgvector_embeddings_tenant_collection
					ON pgvector_embeddings (tenant_id, collection_id);

				CREATE INDEX IF NOT EXISTS idx_pgvector_embeddings_external_id
					ON pgvector_embeddings (tenant_id, collection_id, external_id);
			`,
			Down: `
				DROP TABLE IF EXISTS pgvector_embeddings;
				DROP TABLE IF EXISTS pgvector_collections;
			`,
			// pgvector_collections is NOT here: it carries no tenant_id, so a
			// policy on cleat.tenant_row_is_visible(tenant_id) would fail the
			// migration on "column tenant_id does not exist". Collections are
			// global by construction -- `name TEXT NOT NULL UNIQUE` is unique
			// across the deployment, not per tenant -- and the per-tenant
			// thing is the embedding.
			//
			// pgvector_embeddings qualifies: every statement this plugin
			// issues against it already names tenant_id, and it runs no
			// background sweep, which is the condition the field's own doc
			// sets. The policy is a backstop behind those WHERE clauses
			// rather than a replacement for them.
			//
			// This does nothing at runtime today -- cmd/cleat-worker does not
			// link pgvector (see the comment above its commented-out import in
			// main.go) -- and that is the reason to write it now rather than
			// later: whoever links it gets the policy, instead of discovering
			// it is the one plugin table without one. cleat#1277, cleat#1512.
			TenantScoped: []string{"pgvector_embeddings"},
		},
	}
}
