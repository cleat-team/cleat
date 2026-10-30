package main

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/cleat-team/cleat/engine"
)

// pluginSource is where the deploy route learns which plugins are known: the plugin-name
// registry in the worker's own database, the one worker registration writes (cleat#3600).
// A nil database yields a nil source, which CheckPluginDeps reads as "cannot check" and
// refuses on, never as "all known".
func (s *apiServer) pluginSource() engine.PluginSource {
	if s.pluginSrc != nil {
		return s.pluginSrc
	}
	if s.db == nil || s.factory == nil {
		return nil
	}
	return &engine.PluginNameRegistry{DB: s.db, Dialect: engine.Dialect(s.factory.Dialect())}
}

// refusePluginDeps runs the shared plugin-dependency check for a deploy and, when it refuses,
// writes the response and returns true. It runs before any store write, so a refusal writes
// nothing.
func (s *apiServer) refusePluginDeps(w http.ResponseWriter, r *http.Request, binary, row map[string]string, allowMissing bool) bool {
	err := engine.EnforcePluginDeps(r.Context(), s.pluginSource(), binary, row, allowMissing, slog.Default())
	if err == nil {
		return false
	}
	var unmet *engine.UnmetPluginDepsError
	switch {
	case errors.As(err, &unmet):
		s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":           err.Error() + `; deploy the plugin to a worker first, or send "allow_missing_plugins": true to deploy anyway (logged)`,
			"missing_plugins": unmet.Missing,
		})
	case errors.Is(err, engine.ErrPluginSourceUnavailable):
		s.writeJSON(w, http.StatusNotImplemented, map[string]string{"error": err.Error()})
	default:
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return true
}
