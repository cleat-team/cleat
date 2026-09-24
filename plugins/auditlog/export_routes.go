package auditlog

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// handleExport handles GET /audit/export: the caller's tenant's audit log as JSON Lines.
//
//	from, to   RFC 3339, inclusive; a malformed value is a 400, not an ignored filter
//	cursor     resume after the record that carried it
//	format     "jsonl" (the only one, and the default)
//
// Any authenticated caller of the tenant may call it, exactly as GET /audit/events (the
// owner's decision on cleat#2047: cleat has no roles yet, cleat#2169). There is no
// cross-tenant variant over HTTP; an operator uses `cleatctl audit export --all-tenants`.
func (p *Plugin) handleExport(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, http.StatusUnauthorized, "tenant required")
		return
	}
	q := r.URL.Query()
	if f := q.Get("format"); f != "" && f != "jsonl" {
		p.writeError(w, http.StatusBadRequest, `format must be "jsonl"`)
		return
	}
	opts := ExportOptions{Cursor: q.Get("cursor")}
	for name, dst := range map[string]**time.Time{"from": &opts.From, "to": &opts.To} {
		if s := q.Get(name); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				p.writeError(w, http.StatusBadRequest, name+" must be an RFC 3339 timestamp")
				return
			}
			*dst = &t
		}
	}
	if opts.From != nil && opts.To != nil && opts.To.Before(*opts.From) {
		p.writeError(w, http.StatusBadRequest, "to is before from")
		return
	}

	started := false
	flusher, _ := w.(http.Flusher)
	lines := 0
	err := ExportTenant(r.Context(), p.db, p.dialect, tid, opts, func(line []byte) error {
		if !started {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		if lines++; flusher != nil && lines%200 == 0 {
			flusher.Flush()
		}
		return nil
	})
	switch {
	case err == nil:
		if flusher != nil {
			flusher.Flush()
		}
	case !started && errors.Is(err, ErrBadCursor):
		p.writeError(w, http.StatusBadRequest, "cursor is not one this export issued")
	case !started:
		p.logger.Error("audit-log: export", "tenant", tid, "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to export audit events")
	default:
		// Records already went out with a 200. Ending the stream cleanly would leave a
		// consumer with something that looks like a complete export, so cut the
		// connection: the transport reports a truncated body, and there is no checkpoint
		// line either. (A client that went away lands here too, and needs no log.)
		if r.Context().Err() == nil {
			p.logger.Error("audit-log: export failed mid-stream", "tenant", tid, "records_sent", lines, "error", err)
		}
		panic(http.ErrAbortHandler)
	}
}

// handleVerify handles GET /audit/verify: recompute the caller's tenant's chain and report
// the first break. A break is a finding, not an HTTP error, so it is a 200 with ok false;
// a 500 means the check could not be made. The plugin's own retention_days is passed, so a
// floor over rows too young to have expired is reported.
func (p *Plugin) handleVerify(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, http.StatusUnauthorized, "tenant required")
		return
	}
	rep, err := VerifyChain(r.Context(), p.db, p.dialect, tid, VerifyOptions{RetentionDays: p.config.RetentionDays})
	if err != nil {
		p.logger.Error("audit-log: verify", "tenant", tid, "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to verify the audit chain")
		return
	}
	p.writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
		ChainReport
	}{OK: rep.OK(), ChainReport: rep})
}
