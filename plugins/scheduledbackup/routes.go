package scheduledbackup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
)

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("scheduledbackup: nil mux")
	}
	mux.HandleFunc("POST /backups/configs", p.handleCreateConfig)
	mux.HandleFunc("GET /backups/configs", p.handleListConfigs)
	mux.HandleFunc("GET /backups/configs/{id}", p.handleGetConfig)
	mux.HandleFunc("PUT /backups/configs/{id}", p.handleUpdateConfig)
	mux.HandleFunc("DELETE /backups/configs/{id}", p.handleDeleteConfig)
	mux.HandleFunc("GET /backups/history", p.handleListHistory)
	mux.HandleFunc("POST /backups/configs/{id}/run", p.handleRunBackup)
	return nil
}

// ---- helpers ----

func (p *Plugin) writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (p *Plugin) writeError(w http.ResponseWriter, status int, msg string) {
	p.writeJSON(w, status, map[string]string{"error": msg})
}

func (p *Plugin) tenantID(r *http.Request) uuid.UUID {
	tid, _ := auth.TenantIDFromContext(r.Context())
	return tid
}

// backupConfig represents a single backup_config row.
type backupConfig struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	Cron          string     `json:"cron"`
	S3Bucket      string     `json:"s3_bucket"`
	S3Prefix      string     `json:"s3_prefix"`
	RetentionDays int        `json:"retention_days"`
	Enabled       bool       `json:"enabled"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	NextRunAt     *time.Time `json:"next_run_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// backupHistory represents a single backup_history row.
type backupHistory struct {
	ID           uuid.UUID  `json:"id"`
	ConfigID     uuid.UUID  `json:"config_id"`
	Filename     string     `json:"filename"`
	SizeBytes    *int64     `json:"size_bytes,omitempty"`
	Status       string     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	ErrorMessage *string    `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

type createConfigRequest struct {
	Name          string `json:"name"`
	Cron          string `json:"cron"`
	S3Bucket      string `json:"s3_bucket"`
	S3Prefix      string `json:"s3_prefix"`
	RetentionDays *int   `json:"retention_days,omitempty"`
	Enabled       *bool  `json:"enabled,omitempty"`
}

type updateConfigRequest struct {
	Name          *string `json:"name,omitempty"`
	Cron          *string `json:"cron,omitempty"`
	S3Bucket      *string `json:"s3_bucket,omitempty"`
	S3Prefix      *string `json:"s3_prefix,omitempty"`
	RetentionDays *int    `json:"retention_days,omitempty"`
	Enabled       *bool   `json:"enabled,omitempty"`
}

// ---- POST /backups/configs ----

func (p *Plugin) handleCreateConfig(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	var req createConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		p.writeError(w, 400, "invalid JSON body")
		return
	}

	if req.Name == "" {
		p.writeError(w, 400, "name is required")
		return
	}
	if req.Cron == "" {
		p.writeError(w, 400, "cron is required")
		return
	}

	now := time.Now()
	next := nextRun(req.Cron, now)
	if next.IsZero() {
		p.writeError(w, 400, "invalid cron expression or no future match found")
		return
	}

	retentionDays := 30
	if req.RetentionDays != nil && *req.RetentionDays > 0 {
		retentionDays = *req.RetentionDays
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	id := uuid.New()

	_, err := p.db.ExecContext(r.Context(), `
		INSERT INTO backup_config (tenant_id, id, name, cron, s3_bucket, s3_prefix, retention_days, enabled, next_run_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), now())
	`, tid, id, req.Name, req.Cron, req.S3Bucket, req.S3Prefix, retentionDays, enabled, next)
	if err != nil {
		p.logger.Error("scheduledbackup: create config", "error", err)
		p.writeError(w, 500, "failed to create backup config")
		return
	}

	p.logger.Info("scheduledbackup: config created", "id", id, "tenant", tid, "name", req.Name)

	p.writeJSON(w, 201, map[string]interface{}{
		"id":          id,
		"name":        req.Name,
		"cron":        req.Cron,
		"enabled":     enabled,
		"next_run_at": next,
	})
}

// ---- GET /backups/configs ----

func (p *Plugin) handleListConfigs(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.QueryContext(r.Context(), `
		SELECT id, name, cron, s3_bucket, s3_prefix, retention_days, enabled, last_run_at, next_run_at, created_at, updated_at
		FROM backup_config
		WHERE tenant_id = $1
		ORDER BY created_at DESC
	`, tid)
	if err != nil {
		p.logger.Error("scheduledbackup: list configs", "error", err)
		p.writeError(w, 500, "failed to list backup configs")
		return
	}
	defer rows.Close()

	configs := make([]backupConfig, 0)
	for rows.Next() {
		var c backupConfig
		err := rows.Scan(
			&c.ID, &c.Name, &c.Cron, &c.S3Bucket, &c.S3Prefix,
			&c.RetentionDays, &c.Enabled, &c.LastRunAt, &c.NextRunAt,
			&c.CreatedAt, &c.UpdatedAt,
		)
		if err != nil {
			p.logger.Error("scheduledbackup: scan config row", "error", err)
			continue
		}
		configs = append(configs, c)
	}

	p.writeJSON(w, 200, configs)
}

// ---- GET /backups/configs/{id} ----

func (p *Plugin) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid config id")
		return
	}

	var c backupConfig
	err = p.db.QueryRowContext(r.Context(), `
		SELECT id, name, cron, s3_bucket, s3_prefix, retention_days, enabled, last_run_at, next_run_at, created_at, updated_at
		FROM backup_config
		WHERE id = $1 AND tenant_id = $2
	`, id, tid).Scan(
		&c.ID, &c.Name, &c.Cron, &c.S3Bucket, &c.S3Prefix,
		&c.RetentionDays, &c.Enabled, &c.LastRunAt, &c.NextRunAt,
		&c.CreatedAt, &c.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "backup config not found")
		return
	}
	if err != nil {
		p.logger.Error("scheduledbackup: get config", "id", id, "error", err)
		p.writeError(w, 500, "failed to get backup config")
		return
	}

	p.writeJSON(w, 200, c)
}

// ---- PUT /backups/configs/{id} ----

func (p *Plugin) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid config id")
		return
	}

	var req updateConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		p.writeError(w, 400, "invalid JSON body")
		return
	}

	// Fetch the existing config to determine cron and enabled state.
	var currentCron string
	var currentEnabled bool
	err = p.db.QueryRowContext(r.Context(), `
		SELECT cron, enabled FROM backup_config WHERE id = $1 AND tenant_id = $2
	`, id, tid).Scan(&currentCron, &currentEnabled)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "backup config not found")
		return
	}
	if err != nil {
		p.logger.Error("scheduledbackup: update fetch", "id", id, "error", err)
		p.writeError(w, 500, "failed to update backup config")
		return
	}

	cron := currentCron
	if req.Cron != nil {
		cron = *req.Cron
	}

	enabled := currentEnabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// Recalculate next_run_at if cron or enabled changed.
	var nextRunVal *time.Time
	if req.Cron != nil || req.Enabled != nil {
		if enabled {
			nxt := nextRun(cron, time.Now())
			if nxt.IsZero() {
				p.writeError(w, 400, "invalid cron expression or no future match found")
				return
			}
			nextRunVal = &nxt
		}
	}

	// Build dynamic UPDATE query.
	query := `UPDATE backup_config SET updated_at = now()`
	args := []interface{}{}
	argIdx := 1

	if req.Name != nil {
		query += fmt.Sprintf(", name = $%d", argIdx)
		args = append(args, *req.Name)
		argIdx++
	}
	if req.Cron != nil {
		query += fmt.Sprintf(", cron = $%d", argIdx)
		args = append(args, *req.Cron)
		argIdx++
	}
	if req.S3Bucket != nil {
		query += fmt.Sprintf(", s3_bucket = $%d", argIdx)
		args = append(args, *req.S3Bucket)
		argIdx++
	}
	if req.S3Prefix != nil {
		query += fmt.Sprintf(", s3_prefix = $%d", argIdx)
		args = append(args, *req.S3Prefix)
		argIdx++
	}
	if req.RetentionDays != nil {
		query += fmt.Sprintf(", retention_days = $%d", argIdx)
		args = append(args, *req.RetentionDays)
		argIdx++
	}
	if req.Enabled != nil {
		query += fmt.Sprintf(", enabled = $%d", argIdx)
		args = append(args, *req.Enabled)
		argIdx++
	}
	if nextRunVal != nil {
		query += fmt.Sprintf(", next_run_at = $%d", argIdx)
		args = append(args, *nextRunVal)
		argIdx++
	}

	query += fmt.Sprintf(" WHERE id = $%d AND tenant_id = $%d", argIdx, argIdx+1)
	args = append(args, id, tid)

	_, err = p.db.ExecContext(r.Context(), query, args...)
	if err != nil {
		p.logger.Error("scheduledbackup: update config", "id", id, "error", err)
		p.writeError(w, 500, "failed to update backup config")
		return
	}

	p.logger.Info("scheduledbackup: config updated", "id", id, "tenant", tid)
	p.writeJSON(w, 200, map[string]string{"status": "updated"})
}

// ---- DELETE /backups/configs/{id} ----

func (p *Plugin) handleDeleteConfig(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid config id")
		return
	}

	// Delete history entries first (foreign key constraint).
	_, _ = p.db.ExecContext(r.Context(), `DELETE FROM backup_history WHERE config_id = $1 AND tenant_id = $2`, id, tid)

	result, err := p.db.ExecContext(r.Context(), `
		DELETE FROM backup_config WHERE id = $1 AND tenant_id = $2
	`, id, tid)
	if err != nil {
		p.logger.Error("scheduledbackup: delete config", "id", id, "error", err)
		p.writeError(w, 500, "failed to delete backup config")
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		p.writeError(w, 404, "backup config not found")
		return
	}

	p.logger.Info("scheduledbackup: config deleted", "id", id, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}

// ---- GET /backups/history ----

func (p *Plugin) handleListHistory(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	configIDStr := r.URL.Query().Get("config_id")

	query := `
		SELECT id, config_id, filename, size_bytes, status, started_at, completed_at, error_message, created_at
		FROM backup_history
		WHERE tenant_id = $1
	`
	args := []interface{}{tid}
	argIdx := 2

	if configIDStr != "" {
		configID, err := uuid.Parse(configIDStr)
		if err != nil {
			p.writeError(w, 400, "invalid config_id")
			return
		}
		query += fmt.Sprintf(" AND config_id = $%d", argIdx)
		args = append(args, configID)
		argIdx++
	}

	query += " ORDER BY started_at DESC"

	rows, err := p.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		p.logger.Error("scheduledbackup: list history", "error", err)
		p.writeError(w, 500, "failed to list backup history")
		return
	}
	defer rows.Close()

	history := make([]backupHistory, 0)
	for rows.Next() {
		var h backupHistory
		var sizeBytes sql.NullInt64
		var completedAt sql.NullTime
		var errorMsg sql.NullString

		err := rows.Scan(
			&h.ID, &h.ConfigID, &h.Filename, &sizeBytes, &h.Status,
			&h.StartedAt, &completedAt, &errorMsg, &h.CreatedAt,
		)
		if err != nil {
			p.logger.Error("scheduledbackup: scan history row", "error", err)
			continue
		}

		if sizeBytes.Valid {
			h.SizeBytes = &sizeBytes.Int64
		}
		if completedAt.Valid {
			h.CompletedAt = &completedAt.Time
		}
		if errorMsg.Valid {
			h.ErrorMessage = &errorMsg.String
		}

		history = append(history, h)
	}

	p.writeJSON(w, 200, history)
}

// ---- POST /backups/configs/{id}/run ----

func (p *Plugin) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid config id")
		return
	}

	// Fetch the backup config.
	var name, cron string
	err = p.db.QueryRowContext(r.Context(), `
		SELECT name, cron FROM backup_config WHERE id = $1 AND tenant_id = $2
	`, id, tid).Scan(&name, &cron)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "backup config not found")
		return
	}
	if err != nil {
		p.logger.Error("scheduledbackup: run fetch config", "id", id, "error", err)
		p.writeError(w, 500, "failed to fetch backup config")
		return
	}

	// Create history entry with status "running".
	historyID := uuid.New()
	now := time.Now()
	filename := fmt.Sprintf("manual_%s_%s.dump", name, now.Format("20060102150405"))

	_, err = p.db.ExecContext(r.Context(), `
		INSERT INTO backup_history (id, config_id, tenant_id, filename, status, started_at, created_at)
		VALUES ($1, $2, $3, $4, 'running', $5, $5)
	`, historyID, id, tid, filename, now)
	if err != nil {
		p.logger.Error("scheduledbackup: run create history", "error", err)
		p.writeError(w, 500, "failed to record backup start")
		return
	}

	// Return 202 immediately and run backup in background.
	p.writeJSON(w, 202, map[string]interface{}{
		"history_id": historyID,
		"config_id":  id,
		"status":     "running",
		"filename":   filename,
	})

	go p.runBackupAsync(id, historyID, tid, filename)
}

// runBackupAsync executes pg_dump and records the result in backup_history.
func (p *Plugin) runBackupAsync(configID, historyID, tenantID uuid.UUID, filename string) {
	dumpPath := filepath.Join(p.config.DumpDir, filename)

	var stderr bytes.Buffer
	cmd := exec.CommandContext(context.Background(), "pg_dump", "-f", dumpPath, p.config.DSN)
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		errMsg := stderr.String()
		if errMsg == "" {
			errMsg = err.Error()
		}
		p.logger.Error("scheduledbackup: pg_dump failed",
			"config_id", configID, "history_id", historyID, "error", errMsg,
		)
		p.db.ExecContext(context.Background(), `
			UPDATE backup_history SET status = 'failed', error_message = $1, completed_at = now()
			WHERE id = $2
		`, errMsg, historyID)
		return
	}

	// Read the file size from the dump.
	var sizeBytes int64
	if fi, fiErr := os.Stat(dumpPath); fiErr == nil {
		sizeBytes = fi.Size()
	}

	p.logger.Info("scheduledbackup: backup completed",
		"config_id", configID,
		"history_id", historyID,
		"filename", filename,
		"size_bytes", sizeBytes,
	)

	p.db.ExecContext(context.Background(), `
		UPDATE backup_history SET status = 'completed', size_bytes = $1, completed_at = now()
		WHERE id = $2
	`, sizeBytes, historyID)

	// Update last_run_at and next_run_at on the config.
	now := time.Now()
	var cronExpr string
	if err := p.db.QueryRowContext(context.Background(), `
		SELECT cron FROM backup_config WHERE id = $1
	`, configID).Scan(&cronExpr); err == nil && cronExpr != "" {
		if nxt := nextRun(cronExpr, now); !nxt.IsZero() {
			p.db.ExecContext(context.Background(), `
				UPDATE backup_config SET last_run_at = $1, next_run_at = $2, updated_at = now()
				WHERE id = $3
			`, now, nxt, configID)
		} else {
			p.db.ExecContext(context.Background(), `
				UPDATE backup_config SET last_run_at = $1, next_run_at = NULL, updated_at = now()
				WHERE id = $2
			`, now, configID)
		}
	}
}
