// Package kafkaconnect provides Kafka publish and consume capabilities for
// workflows. It exposes CRUD routes for managing Kafka configurations, a
// produce host function for publishing messages, and a background consumer
// loop that polls for messages and logs them.
package kafkaconnect

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rcownie/durable/internal/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "kafka-connect",
		Version:     "0.1.0",
		Description: "Publish and consume Kafka messages",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// Plugin implements Kafka publish and consume integration for workflows.
type Plugin struct {
	db         *sql.DB
	mux        *http.ServeMux
	logger     *slog.Logger
	httpClient *http.Client
	config     Config
}

// Config holds optional configuration for the kafka-connect plugin.
type Config struct {
	RestProxyURL string `json:"rest_proxy_url,omitempty"` // Confluent REST Proxy URL
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "kafka-connect",
		Version:     "0.1.0",
		Description: "Publish and consume Kafka messages",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. It creates an HTTP
// client with a 10-second timeout for REST proxy requests.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.httpClient = &http.Client{
		Timeout: 10 * time.Second,
	}

	// Parse optional config.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("kafka-connect: invalid config: %w", err)
		}
	}

	p.logger.Info("kafka-connect: initialized",
		"rest_proxy", p.config.RestProxyURL,
	)
	return nil
}
