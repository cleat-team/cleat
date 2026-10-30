package main

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// pluginRegistry is where the deploy route learns which plugins are known and where it records
// an override (cleat#3600): the plugin-name registry in the worker's own database, the one worker
// registration writes. A nil database yields nil, which the check reads as "cannot check" and
// "cannot record", never as "all known".
//
// In a sharded deployment s.db is the FIRST shard's database (main.go: "the first shard's database
// for plugin migrations and administration"), which is also where the worker registers, so the
// registry is read and written in one place. A deploy that fans out to every shard asks that one
// registry once, before the first shard is written.
func (s *apiServer) pluginRegistry() engine.PluginRegistryAccess {
	if s.pluginReg != nil {
		return s.pluginReg
	}
	if s.db == nil || s.factory == nil {
		return nil
	}
	return &engine.PluginNameRegistry{DB: s.db, Dialect: s.factory.Dialect()}
}

// refusePluginDeps runs the shared plugin-dependency check for a deploy and, when it refuses,
// writes the response and returns true. It runs before any store write, so a refusal writes
// nothing. An override writes its audit record first, and is refused if that cannot be written.
func (s *apiServer) refusePluginDeps(w http.ResponseWriter, r *http.Request, name string, version int, binary, row map[string]string, allowMissing bool) bool {
	src, rec := engine.RegistrySplit(s.pluginRegistry())
	tenant, subject := "", ""
	if tid, ok := auth.TenantIDFromContext(r.Context()); ok {
		tenant = tid.String()
	}
	if sub, ok := auth.SubjectFromContext(r.Context()); ok {
		subject = sub
	}
	if tenant == "" {
		tenant = engine.DefaultTenantUUID
	}
	err := engine.EnforcePluginDeps(r.Context(), src, binary, row, engine.EnforceOptions{
		AllowMissing: allowMissing,
		Log:          slog.Default(),
		Recorder:     rec,
		Override:     engine.PluginDepOverride{TenantID: tenant, Definition: name, Version: version, Via: "rest", Actor: subject},
	})
	if err == nil {
		return false
	}
	var unmet *engine.UnmetPluginDepsError
	switch {
	case errors.As(err, &unmet):
		s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":           err.Error() + `; deploy the plugin to a worker first, or send "allow_missing_plugins": true to deploy anyway (logged and recorded)`,
			"missing_plugins": unmet.Missing,
		})
	case errors.Is(err, engine.ErrPluginSourceUnavailable), errors.Is(err, engine.ErrOverrideNotRecordable):
		s.writeJSON(w, http.StatusNotImplemented, map[string]string{"error": err.Error()})
	default:
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return true
}
