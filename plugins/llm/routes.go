package llm

import (
	"encoding/json"
	"net/http"
)

// RegisterRoutes registers HTTP endpoints for the LLM plugin.
func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	mux.HandleFunc("GET /api/llm/health", func(w http.ResponseWriter, r *http.Request) {
		providers := map[string]any{}
		for name, cfg := range p.config.Providers {
			providers[name] = map[string]any{
				"enabled":       cfg.Enabled,
				"default_model": cfg.DefaultModel,
				"has_api_key":   cfg.APIKey != "",
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":    "ok",
			"plugin":    "llm",
			"version":   "0.1.0",
			"providers": providers,
		})
	})

	mux.HandleFunc("GET /api/llm/models", func(w http.ResponseWriter, r *http.Request) {
		provider := r.URL.Query().Get("provider")
		reqJSON, _ := json.Marshal(map[string]string{"provider": provider})
		out, err := p.listModels(r.Context(), string(reqJSON))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(out))
	})

	return nil
}
