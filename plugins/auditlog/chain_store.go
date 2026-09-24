package auditlog

// Appending to a tenant's chain, and the SQL that differs by dialect (cleat#2047).
//
// An append is one transaction:
//
//  1. lock the tenant's head row and read its (seq, hash);
//  2. hash the new row over that hash, and insert it at seq+1;
//  3. move the head to the new row.
//
// The head row is the per-tenant lock. Two workers appending for one tenant queue on
// it, so two rows can never claim the same predecessor, and UNIQUE (tenant_id, seq)
// makes a fork impossible to store even if something bypassed the lock.
//
// Different tenants never contend: each has its own head row.

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// zeroHashHex is the prev_hash of a tenant's first row, and the head of an empty chain.
const zeroHashHex = "0000000000000000000000000000000000000000000000000000000000000000"

var errHeadMissing = errors.New("audit chain head row is missing")

// chainEvent is what the middleware hands the store. Everything is text that has
// already been made valid UTF-8: see sanitizeText.
type chainEvent struct {
	tenantID   uuid.UUID
	userID     string
	method     string
	path       string
	statusCode int
	ipAddress  string
	userAgent  string
	durationMs int
}

// sanitizeText makes s storable and reproducible. PostgreSQL refuses invalid UTF-8 and
// NUL bytes, so an event carrying one (a User-Agent is attacker-chosen bytes) was never
// recorded at all; and a hash over bytes that are not text cannot be reproduced by a
// verifier written in another language. Both are replaced with U+FFFD, before the row is
// hashed AND before it is stored, so the two see the same string.
func sanitizeText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "�")
	}
	return s
}

// ---- per-dialect statements ----

// epochMicrosExpr reads a timestamp column as microseconds since the Unix epoch. The
// hash covers THIS, not the value the driver would hand back: a driver round trip goes
// through a time zone (MySQL's session zone, measured 2026-09-24: a time.Time written in
// a session at -04:00 or +05:30 reads back hours away), and a hash that depends on the
// session zone would call an untouched row edited the day someone changes it.
func epochMicrosExpr(d plugin.Dialect, col string) string {
	switch d {
	case plugin.DialectMySQL:
		return "CAST(UNIX_TIMESTAMP(" + col + ") * 1000000 AS SIGNED)"
	case plugin.DialectMSSQL:
		return "DATEDIFF_BIG(MICROSECOND, CAST('1970-01-01T00:00:00+00:00' AS DATETIMEOFFSET), " + col + ")"
	default:
		return "(EXTRACT(EPOCH FROM " + col + ") * 1000000)::bigint"
	}
}

// ensureHeadSQL creates the tenant's head row if it is absent. It runs OUTSIDE the
// append transaction, on its own: on MySQL an INSERT IGNORE that finds the row takes a
// shared lock on it for the life of its transaction, and two appenders that each held
// one and then asked for the exclusive lock would deadlock.
func ensureHeadSQL(d plugin.Dialect) string {
	switch d {
	case plugin.DialectMySQL:
		return `INSERT IGNORE INTO audit_chain_heads (tenant_id, seq, hash) VALUES ($1, 0, $2)`
	case plugin.DialectMSSQL:
		return `IF NOT EXISTS (SELECT 1 FROM audit_chain_heads WITH (UPDLOCK, HOLDLOCK) WHERE tenant_id = @p1)
			INSERT INTO audit_chain_heads (tenant_id, seq, hash) VALUES (@p1, 0, @p2)`
	default:
		return `INSERT INTO audit_chain_heads (tenant_id, seq, hash) VALUES ($1, 0, $2) ON CONFLICT (tenant_id) DO NOTHING`
	}
}

// lockHeadSQL takes the head row's exclusive lock and reads it.
func lockHeadSQL(d plugin.Dialect) string {
	if d == plugin.DialectMSSQL {
		return `SELECT seq, hash FROM audit_chain_heads WITH (UPDLOCK, ROWLOCK) WHERE tenant_id = $1`
	}
	return `SELECT seq, hash FROM audit_chain_heads WHERE tenant_id = $1 FOR UPDATE`
}

const insertChainedSQL = `INSERT INTO audit_events
	(id, tenant_id, timestamp, method, path, status_code, user_id, ip_address, user_agent, duration_ms, metadata,
	 seq, prev_hash, row_hash)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

const moveHeadSQL = `UPDATE audit_chain_heads SET seq = $1, hash = $2 WHERE tenant_id = $3 AND seq = $4`

// timestampArg is the value written to the timestamp column. PostgreSQL and SQL Server
// take a time.Time and keep the instant. MySQL is given the UTC wall-clock text and the
// session is forced to UTC around the write (see withUTCSession), because a time.Time is
// formatted by the driver and interpreted by the server in the SESSION zone.
func timestampArg(d plugin.Dialect, ts time.Time) any {
	if d == plugin.DialectMySQL {
		return ts.UTC().Format("2006-01-02 15:04:05.000000")
	}
	return ts.UTC()
}

// withUTCSession runs fn with the MySQL session's time zone set to UTC and restores it,
// on the same connection, before the transaction ends. A pooled connection that kept a
// changed zone would change how its next user reads and writes every TIMESTAMP.
func withUTCSession(ctx context.Context, tx plugin.PluginTx, d plugin.Dialect, fn func() error) error {
	if d != plugin.DialectMySQL {
		return fn()
	}
	if _, err := tx.Exec(ctx, `SET @cleat_audit_tz = @@session.time_zone`); err != nil {
		return fmt.Errorf("audit chain: read session time zone: %w", err)
	}
	if _, err := tx.Exec(ctx, `SET time_zone = '+00:00'`); err != nil {
		return fmt.Errorf("audit chain: force UTC session: %w", err)
	}
	ferr := fn()
	// A fresh context: the caller's is often what has just been cancelled.
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := tx.Exec(rctx, `SET time_zone = @cleat_audit_tz`); err != nil && ferr == nil {
		ferr = fmt.Errorf("audit chain: restore session time zone: %w", err)
	}
	return ferr
}

// ---- the append ----

// appendChained adds one event to its tenant's chain. ctx must carry the tenant
// (plugin.ForTenant): the head and the row are both row-level-secured.
func (p *Plugin) appendChained(ctx context.Context, e chainEvent) error {
	e.method, e.path = sanitizeText(e.method), sanitizeText(e.path)
	e.userID, e.ipAddress, e.userAgent = sanitizeText(e.userID), sanitizeText(e.ipAddress), sanitizeText(e.userAgent)

	if err := p.ensureHead(ctx, e.tenantID); err != nil {
		return err
	}
	err := p.appendOnce(ctx, e)
	if errors.Is(err, errHeadMissing) {
		// The head was removed between ensureHead and the lock (a tenant dropped and
		// recreated). Once is enough: a second miss is not a race.
		p.headsSeen.Delete(e.tenantID)
		if err = p.ensureHead(ctx, e.tenantID); err == nil {
			err = p.appendOnce(ctx, e)
		}
	}
	return err
}

// ensureHead makes sure the tenant's head row exists. It is remembered per process, so
// the steady state costs nothing.
func (p *Plugin) ensureHead(ctx context.Context, tenant uuid.UUID) error {
	if _, ok := p.headsSeen.Load(tenant); ok {
		return nil
	}
	if _, err := p.db.Exec(ctx, plugin.Rebind(ensureHeadSQL(p.dialect), p.dialect), tenant, zeroHashHex); err != nil {
		return fmt.Errorf("audit chain: create head for tenant %s: %w", tenant, err)
	}
	p.headsSeen.Store(tenant, struct{}{})
	return nil
}

func (p *Plugin) appendOnce(ctx context.Context, e chainEvent) (err error) {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("audit chain: begin: %w", err)
	}
	// Roll back on every exit, a panic included: the head's lock lives as long as this
	// transaction, and a transaction abandoned by a panic holds it until the connection
	// is collected, stalling every later append for the tenant.
	defer func() { _ = tx.Rollback() }()

	var head struct {
		seq  int64
		hash string
	}
	if err := plugin.ScanRow(tx.QueryRow(ctx, plugin.Rebind(lockHeadSQL(p.dialect), p.dialect), e.tenantID),
		&head.seq, &head.hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errHeadMissing
		}
		return fmt.Errorf("audit chain: lock head: %w", err)
	}
	prevBytes, err := hex.DecodeString(strings.TrimSpace(head.hash))
	if err != nil || len(prevBytes) != chainHashLen {
		return fmt.Errorf("audit chain: head hash %q for tenant %s is not a %d-byte hex value", head.hash, e.tenantID, chainHashLen)
	}
	var prev [chainHashLen]byte
	copy(prev[:], prevBytes)

	ts := time.Now().UTC().Truncate(time.Microsecond)
	rec := chainRecord{
		TenantID:   e.tenantID,
		Seq:        head.seq + 1,
		ID:         uuid.New(),
		Timestamp:  ts,
		Method:     e.method,
		Path:       e.path,
		StatusCode: sql.NullInt64{Int64: int64(e.statusCode), Valid: true},
		UserID:     sql.NullString{String: e.userID, Valid: true},
		IPAddress:  sql.NullString{String: e.ipAddress, Valid: true},
		UserAgent:  sql.NullString{String: e.userAgent, Valid: true},
		DurationMs: sql.NullInt64{Int64: int64(e.durationMs), Valid: true},
		Metadata:   sql.NullString{String: "{}", Valid: true},
	}
	sum, err := chainHash(prev, rec)
	if err != nil {
		return err
	}
	rowHash := hex.EncodeToString(sum[:])

	err = withUTCSession(ctx, tx, p.dialect, func() error {
		// Every value is passed as the type the column holds, and the id is supplied
		// rather than defaulted: see the note on recordAudit.
		_, err := tx.Exec(ctx, plugin.Rebind(insertChainedSQL, p.dialect),
			rec.ID.String(), rec.TenantID, timestampArg(p.dialect, ts), rec.Method, rec.Path,
			rec.StatusCode.Int64, rec.UserID.String, rec.IPAddress.String, rec.UserAgent.String, rec.DurationMs.Int64,
			rec.Metadata.String, rec.Seq, strings.ToLower(hex.EncodeToString(prev[:])), rowHash)
		return err
	})
	if err != nil {
		return fmt.Errorf("audit chain: insert row: %w", err)
	}
	n, err := tx.Exec(ctx, plugin.Rebind(moveHeadSQL, p.dialect), rec.Seq, rowHash, e.tenantID, head.seq)
	if err != nil {
		return fmt.Errorf("audit chain: move head: %w", err)
	}
	if n != 1 {
		// The lock was held, so this cannot happen unless something changed the head
		// underneath it. Failing is right: a row inserted without moving the head is a
		// fork waiting to be found.
		return fmt.Errorf("audit chain: head for tenant %s was not at seq %d when it was moved", e.tenantID, head.seq)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("audit chain: commit: %w", err)
	}
	return nil
}
