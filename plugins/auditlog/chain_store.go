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

	// id and ts are fixed when the request finished, not when the append runs: with a queue
	// and retries the two can be seconds apart, the row should say when the request happened,
	// and a retry has to be able to ask "did attempt one commit after all?" by id. Zero means
	// "choose now" (a direct caller, a test).
	id uuid.UUID
	ts time.Time
	// retry is set from the second attempt on. It makes the append look for e.id under the
	// head's lock first, so an attempt whose commit succeeded but whose acknowledgement was
	// lost (a dropped connection, a client timeout) is not appended a second time.
	retry bool
}

// errAlreadyRecorded reports that a retry found its own event already committed. It is a
// success, and only a caller that retries ever sees it.
var errAlreadyRecorded = errors.New("audit event already recorded")

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

// Column widths the text has to fit, in the units each dialect counts. MySQL's method and
// path are VARCHAR(255) and VARCHAR(700) (characters); SQL Server's are NVARCHAR(255) and
// NVARCHAR(900) (UTF-16 code units) -- but SQL Server indexes the path, and a nonclustered
// index key may not exceed 1,700 bytes: with the 16-byte tenant id that is 842 units, so
// the usable limit is under the declared one (measured: a 900-unit path fails with error
// 1946). PostgreSQL's are unbounded. The rule is the same on
// all three, so one request makes the same row everywhere: a value is cut to satisfy every
// dialect's limit at once, with a marker, BEFORE it is hashed and stored.
//
// A path is chosen by the caller: without this a 1,000-character path failed the insert on
// MySQL and SQL Server, the failure was logged and not retried, and the request was simply
// not in the log -- an attacker-chosen way to stay out of it.
const (
	maxMethodRunes, maxMethodUnits = 255, 255
	maxPathRunes, maxPathUnits     = 700, 800
	// The other text columns are unbounded TEXT or NVARCHAR(MAX), but a User-Agent is
	// attacker-chosen and MySQL's TEXT holds 65,535 bytes, so they are capped too.
	maxFreeTextRunes, maxFreeTextUnits = 4096, 8192
	truncationMarker                   = "...[truncated]"
)

// fitText cuts s to at most maxRunes characters and maxUnits UTF-16 code units, ending in
// truncationMarker when it had to cut. It never splits a character.
func fitText(s string, maxRunes, maxUnits int) string {
	runes, units := 0, 0
	for _, r := range s {
		u := 1
		if r > 0xFFFF {
			u = 2
		}
		runes, units = runes+1, units+u
	}
	if runes <= maxRunes && units <= maxUnits {
		return s
	}
	keepRunes, keepUnits := maxRunes-len(truncationMarker), maxUnits-len(truncationMarker)
	var b strings.Builder
	runes, units = 0, 0
	for _, r := range s {
		u := 1
		if r > 0xFFFF {
			u = 2
		}
		if runes+1 > keepRunes || units+u > keepUnits {
			break
		}
		b.WriteRune(r)
		runes, units = runes+1, units+u
	}
	b.WriteString(truncationMarker)
	return b.String()
}

// ---- per-dialect statements ----

// epochMicrosExpr reads a timestamp column as microseconds since the Unix epoch. The
// hash covers THIS, not the value the driver would hand back: a driver round trip goes
// through a time zone, and a hash that depends on one would call an untouched row edited
// the day someone changed it.
//
// MySQL's column is DATETIME(6) holding UTC wall-clock time (migration 3), which does no
// zone conversion on the way in or out. TIMESTAMPDIFF against the epoch's wall-clock
// reading is therefore independent of the session's time_zone. UNIX_TIMESTAMP is not: it
// interprets a DATETIME in the session zone (measured 2026-09-24 on the TIMESTAMP column
// this replaced: a session at -04:00 or +05:30 read rows hours away).
func epochMicrosExpr(d plugin.Dialect, col string) string {
	switch d {
	case plugin.DialectMySQL:
		return "TIMESTAMPDIFF(MICROSECOND, '1970-01-01 00:00:00', " + col + ")"
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
// take a time.Time and keep the instant. MySQL is given the UTC wall-clock text: its
// column is DATETIME(6), which stores what it is given, so no session zone is involved.
func timestampArg(d plugin.Dialect, ts time.Time) any {
	if d == plugin.DialectMySQL {
		return ts.UTC().Format("2006-01-02 15:04:05.000000")
	}
	return ts.UTC()
}

// ---- the append ----

// appendChained adds one event to its tenant's chain. ctx must carry the tenant
// (plugin.ForTenant): the head and the row are both row-level-secured.
func (p *Plugin) appendChained(ctx context.Context, e chainEvent) error {
	e.method, e.path = fitText(sanitizeText(e.method), maxMethodRunes, maxMethodUnits), fitText(sanitizeText(e.path), maxPathRunes, maxPathUnits)
	e.userID = fitText(sanitizeText(e.userID), maxFreeTextRunes, maxFreeTextUnits)
	e.ipAddress = fitText(sanitizeText(e.ipAddress), maxFreeTextRunes, maxFreeTextUnits)
	e.userAgent = fitText(sanitizeText(e.userAgent), maxFreeTextRunes, maxFreeTextUnits)

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

	// The head's lock is held, so nothing else can commit for this tenant between this read
	// and the insert: if the event is not here now it is not going to appear.
	if e.retry && e.id != uuid.Nil {
		var one int
		switch err := plugin.ScanRow(tx.QueryRow(ctx, plugin.Rebind(
			`SELECT 1 FROM audit_events WHERE tenant_id = $1 AND id = $2`, p.dialect), e.tenantID, e.id.String()), &one); {
		case err == nil:
			return errAlreadyRecorded
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("audit chain: look for an earlier attempt: %w", err)
		}
	}

	ts := e.ts
	if ts.IsZero() {
		ts = time.Now()
	}
	ts = ts.UTC().Truncate(time.Microsecond)
	id := e.id
	if id == uuid.Nil {
		id = uuid.New()
	}
	rec := chainRecord{
		TenantID:   e.tenantID,
		Seq:        head.seq + 1,
		ID:         id,
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

	// Every value is passed as the type the column holds, and the id is supplied rather
	// than defaulted: see the note on recordAudit.
	_, err = tx.Exec(ctx, plugin.Rebind(insertChainedSQL, p.dialect),
		rec.ID.String(), rec.TenantID, timestampArg(p.dialect, ts), rec.Method, rec.Path,
		rec.StatusCode.Int64, rec.UserID.String, rec.IPAddress.String, rec.UserAgent.String, rec.DurationMs.Int64,
		rec.Metadata.String, rec.Seq, strings.ToLower(hex.EncodeToString(prev[:])), rowHash)
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
