package auditlog

// Verifying a tenant's chain (cleat#2047).
//
// WHAT A CLEAN RESULT MEANS. Every row from the recorded floor to the head is present,
// in order, unedited, and links to its predecessor, and the head agrees with the last
// row. It means the rows were not edited or removed by anyone who did not also rewrite
// the head and the floor. It does NOT mean every request was recorded (an event dropped
// before it was appended leaves no gap), and it does NOT stop a database administrator
// who rewrites the whole chain and the head together: the hash is not keyed and nothing
// outside the database anchors it. Both limits are stated in docs/reference/audit-log.md.
//
// The verifier reads and never writes.

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// Break kinds, in the words an operator will search for.
const (
	// BreakEdited: a row's stored hash is not the hash of its own contents.
	BreakEdited = "edited"
	// BreakMissing: rows are absent from the middle of the chain (a gap in seq), or
	// from its start without the floor having been moved to account for them.
	BreakMissing = "missing"
	// BreakRelinked: a row's stored prev_hash is not its predecessor's hash.
	BreakRelinked = "relinked"
	// BreakTruncatedTail: the head is ahead of the last row, so the newest rows are gone.
	BreakTruncatedTail = "truncated_tail"
	// BreakExtraRows: rows exist beyond the head.
	BreakExtraRows = "extra_rows"
	// BreakHeadMissing: chained rows exist and the head row that anchors them does not.
	BreakHeadMissing = "head_missing"
	// BreakHeadMismatch: every row verifies and the last seq matches, but the hash the
	// head records is not the newest row's. The head itself was changed.
	BreakHeadMismatch = "head_mismatch"
	// BreakUnreadable: a row could not be hashed at all (metadata that is not JSON).
	BreakUnreadable = "unreadable"
	// BreakFloorUnexpired: the floor covers a row that was too young to have expired, or
	// has no recorded timestamp. Retention only removes expired rows, so this is a floor
	// moved by something else. It is reported only when the caller supplies the retention
	// period (VerifyOptions.RetentionDays), and raising that period later reports floors
	// that were set under the shorter one.
	BreakFloorUnexpired = "floor_unexpired"
)

// ChainBreak is the FIRST place the chain fails to verify, in seq order.
type ChainBreak struct {
	Seq    int64     `json:"seq"`
	ID     uuid.UUID `json:"id"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
}

// ChainReport is the outcome of one verification.
type ChainReport struct {
	TenantID uuid.UUID `json:"tenant_id"`
	// Checked is how many chained rows were verified.
	Checked int64 `json:"checked"`
	// FloorSeq is the seq retention has removed through: the chain is verified from
	// FloorSeq+1. Zero means nothing has been removed.
	FloorSeq int64 `json:"floor_seq"`
	// HeadSeq is the seq the head row records as newest.
	HeadSeq int64 `json:"head_seq"`
	// Unchained is how many rows have no seq: written before the chain existed, and not
	// covered by it.
	Unchained int64       `json:"unchained"`
	Break     *ChainBreak `json:"break,omitempty"`
}

// OK reports whether the chain verified end to end.
func (r ChainReport) OK() bool { return r.Break == nil }

const verifyPageSize = 1000

// VerifyOptions tunes a verification.
type VerifyOptions struct {
	// RetentionDays, when positive, lets verify say that the floor covers rows too young to
	// have expired (BreakFloorUnexpired). Zero skips that check, and the report says nothing
	// about it: the caller either knows the plugin's retention or does not.
	RetentionDays int
	// Now is the clock, for a test. Nil means time.Now.
	Now func() time.Time
}

// floorClockSkew is how far a floor timestamp may sit inside the retention window before
// it is reported: the sweeping worker's clock is not the verifier's.
const floorClockSkew = 5 * time.Minute

// verifyAttempts bounds how many times a verification restarts because the chain moved
// underneath it (a retention sweep advancing the floor) or the database picked it as a
// deadlock victim.
const verifyAttempts = 8

// errChainMoved says an attempt is void because the floor moved while it ran.
var errChainMoved = errors.New("the chain's floor moved during verification")

// VerifyChain recomputes tenant's chain and reports the first break, if any. db is the
// plugin's database handle and dialect its dialect; the tenant is put in the context. It
// is safe on a chain that is live.
//
// A chain being appended to and swept by retention is the normal case, and a verifier
// that reads the head and then pages the rows in separate queries sees a moving target.
// Two things keep the answer true:
//
//   - The scan is BOUNDED by the head's seq as read at the start. Rows appended after are
//     not extra rows, they are newer than the head this run started from. (Rows beyond
//     even the CURRENT head are extra: see the end of an attempt.)
//   - An attempt whose floor moved while it ran is thrown away and repeated, because a
//     sweep that removed rows the scan had not reached shows up as a gap that is not one.
//     A deadlock victim is repeated the same way. After verifyAttempts the chain is
//     changing faster than it can be read, which is an error (the check could not be made),
//     never a finding.
func VerifyChain(ctx context.Context, db plugin.PluginDB, dialect plugin.Dialect, tenant uuid.UUID, opts VerifyOptions) (ChainReport, error) {
	ctx = plugin.ForTenant(ctx, tenant)
	var lastErr error
	for attempt := 1; attempt <= verifyAttempts; attempt++ {
		rep, err := verifyOnce(ctx, db, dialect, tenant, opts)
		if err == nil {
			return rep, nil
		}
		if !errors.Is(err, errChainMoved) && !isTransientDBError(err) {
			return rep, err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return rep, ctx.Err()
		case <-time.After(time.Duration(attempt) * 25 * time.Millisecond):
		}
	}
	return ChainReport{TenantID: tenant}, fmt.Errorf("audit verify: gave up after %d attempts, the chain kept changing under the verifier: %w", verifyAttempts, lastErr)
}

// isTransientDBError reports a deadlock or serialisation failure: the statement was chosen
// as a victim and is safe to repeat. Matched on the message because the drivers do not
// share an error type: PostgreSQL 40P01 and 40001, MySQL 1213, SQL Server 1205.
func isTransientDBError(err error) bool {
	m := strings.ToLower(err.Error())
	for _, k := range []string{"deadlock", "(1205)", "error 1205", "error 1213", "40p01", "40001", "serialization failure"} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}

type chainHead struct {
	seq, floorSeq, floorTS int64
	hash, floorHash        string
}

func readHead(ctx context.Context, db plugin.PluginDB, dialect plugin.Dialect, tenant uuid.UUID) (chainHead, bool, error) {
	var h chainHead
	err := plugin.ScanRow(db.QueryRow(ctx, plugin.Rebind(
		`SELECT seq, hash, floor_seq, floor_hash, floor_ts FROM audit_chain_heads WHERE tenant_id = $1`, dialect), tenant),
		&h.seq, &h.hash, &h.floorSeq, &h.floorHash, &h.floorTS)
	switch {
	case err == nil:
		h.hash, h.floorHash = strings.TrimSpace(h.hash), strings.TrimSpace(h.floorHash)
		return h, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return chainHead{}, false, nil
	}
	return chainHead{}, false, fmt.Errorf("audit verify: read head: %w", err)
}

func verifyOnce(ctx context.Context, db plugin.PluginDB, dialect plugin.Dialect, tenant uuid.UUID, opts VerifyOptions) (ChainReport, error) {
	rep := ChainReport{TenantID: tenant}

	head, haveHead, err := readHead(ctx, db, dialect, tenant)
	if err != nil {
		return rep, err
	}
	rep.HeadSeq, rep.FloorSeq = head.seq, head.floorSeq

	if err := db.QueryRow(ctx, plugin.Rebind(
		`SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND seq IS NULL`, dialect), tenant).Scan(&rep.Unchained); err != nil {
		return rep, fmt.Errorf("audit verify: count unchained rows: %w", err)
	}

	// A floor over rows that had not expired is reported before anything else: it is
	// about seq <= floor, which precedes every row this scan reads. It cannot see a floor
	// recorded with a forged timestamp (nothing outside the database says what it should
	// be); it sees a floor moved carelessly, or by code that did not know to record one.
	if haveHead && head.floorSeq > 0 && opts.RetentionDays > 0 {
		now := time.Now
		if opts.Now != nil {
			now = opts.Now
		}
		window := now().Add(-time.Duration(opts.RetentionDays)*24*time.Hour + floorClockSkew)
		switch {
		case head.floorTS <= 0:
			rep.Break = &ChainBreak{Seq: head.floorSeq, Kind: BreakFloorUnexpired,
				Detail: fmt.Sprintf("the floor (seq %d) has no recorded timestamp, so it cannot be shown to cover only expired rows", head.floorSeq)}
		case time.UnixMicro(head.floorTS).After(window):
			rep.Break = &ChainBreak{Seq: head.floorSeq, Kind: BreakFloorUnexpired,
				Detail: fmt.Sprintf("the floor removed seq %d, timestamped %s, which is inside the %d-day retention window: rows that had not expired were removed",
					head.floorSeq, time.UnixMicro(head.floorTS).UTC().Format(time.RFC3339), opts.RetentionDays)}
		}
		if rep.Break != nil {
			return rep, nil
		}
	}

	// The first row expected is the one after the floor, linking to the floor's hash.
	expectSeq, expectPrev := head.floorSeq+1, head.floorHash
	after := head.floorSeq
	bound := head.seq
	if !haveHead {
		expectSeq, expectPrev, after = 1, zeroHashHex, 0
		bound = math.MaxInt64
	}
	var lastSeq int64
	var lastHash string

	query := plugin.Rebind(fmt.Sprintf(`
		SELECT seq, id, %s, method, path, status_code, user_id, ip_address, user_agent, duration_ms, metadata,
		       prev_hash, row_hash
		FROM audit_events
		WHERE tenant_id = $1 AND seq IS NOT NULL AND seq > $2 AND seq <= $3
		ORDER BY seq %s`, epochMicrosExpr(dialect, "timestamp"), plugin.LimitClause("$4", dialect)), dialect)

scan:
	for {
		rows, err := db.Query(ctx, query, tenant, after, bound, verifyPageSize)
		if err != nil {
			return rep, fmt.Errorf("audit verify: read rows: %w", err)
		}
		n := 0
		for rows.Next() {
			n++
			var (
				seq                                     int64
				id                                      uuid.UUID
				us                                      int64
				method, path                            string
				status, dur                             sql.NullInt64
				userID, ip, ua, meta, prevHash, rowHash sql.NullString
			)
			if err := plugin.ScanRow(rows, &seq, &id, &us, &method, &path, &status, &userID, &ip, &ua, &dur, &meta, &prevHash, &rowHash); err != nil {
				_ = rows.Close()
				return rep, fmt.Errorf("audit verify: scan row: %w", err)
			}
			after = seq
			brk := checkRow(rep.TenantID, &expectSeq, &expectPrev, chainRecord{
				TenantID: tenant, Seq: seq, ID: id, Timestamp: time.UnixMicro(us).UTC(),
				Method: method, Path: path, StatusCode: status, UserID: userID, IPAddress: ip,
				UserAgent: ua, DurationMs: dur, Metadata: meta,
			}, strings.TrimSpace(prevHash.String), strings.TrimSpace(rowHash.String))
			if brk != nil {
				_ = rows.Close()
				rep.Break = brk
				break scan
			}
			rep.Checked++
			lastSeq, lastHash = seq, strings.TrimSpace(rowHash.String)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return rep, fmt.Errorf("audit verify: read rows: %w", err)
		}
		_ = rows.Close()
		if n < verifyPageSize {
			break
		}
	}

	// Rows beyond the head the run started from. The newest row is read BEFORE the head is
	// read again: a row appended in between has then been committed with its head move, so
	// the fresh head covers it. Reading them the other way round would report a concurrent
	// append as extra rows.
	var maxBeyond sql.NullInt64
	if haveHead {
		if err := db.QueryRow(ctx, plugin.Rebind(
			`SELECT MAX(seq) FROM audit_events WHERE tenant_id = $1 AND seq > $2`, dialect), tenant, head.seq).Scan(&maxBeyond); err != nil {
			return rep, fmt.Errorf("audit verify: look for rows beyond the head: %w", err)
		}
	}
	head2, haveHead2, err := readHead(ctx, db, dialect, tenant)
	if err != nil {
		return rep, err
	}
	// Whatever this attempt saw was measured against a floor that has since moved, or
	// before the tenant's first append created the head, and none of it can be trusted.
	// (A head that VANISHED is not a move: nothing appends a head away, and it is reported.)
	moved := haveHead2 && (!haveHead || head2.floorSeq != head.floorSeq || head2.floorHash != head.floorHash)
	if moved {
		return rep, errChainMoved
	}
	if rep.Break != nil {
		return rep, nil
	}

	switch {
	case !haveHead && rep.Checked > 0:
		rep.Break = &ChainBreak{Seq: lastSeq, Kind: BreakHeadMissing,
			Detail: fmt.Sprintf("%d chained rows exist but the head row that anchors them does not", rep.Checked)}
	case haveHead && rep.Checked == 0 && rep.HeadSeq > rep.FloorSeq:
		rep.Break = &ChainBreak{Seq: rep.FloorSeq + 1, Kind: BreakTruncatedTail,
			Detail: fmt.Sprintf("the head records seq %d and floor %d, and no chained rows remain", rep.HeadSeq, rep.FloorSeq)}
	case haveHead && rep.Checked > 0 && lastSeq < rep.HeadSeq:
		rep.Break = &ChainBreak{Seq: lastSeq + 1, Kind: BreakTruncatedTail,
			Detail: fmt.Sprintf("the head records seq %d but the newest row is seq %d: rows %d through %d are gone", rep.HeadSeq, lastSeq, lastSeq+1, rep.HeadSeq)}
	case haveHead && maxBeyond.Valid && maxBeyond.Int64 > head2.seq:
		rep.Break = &ChainBreak{Seq: head2.seq + 1, Kind: BreakExtraRows,
			Detail: fmt.Sprintf("rows exist beyond the head (head seq %d, newest row %d)", head2.seq, maxBeyond.Int64)}
	case haveHead && rep.Checked > 0 && lastHash != head.hash:
		rep.Break = &ChainBreak{Seq: lastSeq, Kind: BreakHeadMismatch,
			Detail: "the newest row's hash is not the hash the head records"}
	}
	return rep, nil
}

// checkRow verifies one row against what the chain expects next, and advances the
// expectation. It returns the break, or nil.
func checkRow(tenant uuid.UUID, expectSeq *int64, expectPrev *string, r chainRecord, prevHash, rowHash string) *ChainBreak {
	if r.Seq != *expectSeq {
		kind, detail := BreakMissing, fmt.Sprintf("expected seq %d, found seq %d: %d row(s) are missing", *expectSeq, r.Seq, r.Seq-*expectSeq)
		return &ChainBreak{Seq: *expectSeq, ID: r.ID, Kind: kind, Detail: detail}
	}
	if !strings.EqualFold(prevHash, *expectPrev) {
		return &ChainBreak{Seq: r.Seq, ID: r.ID, Kind: BreakRelinked,
			Detail: fmt.Sprintf("prev_hash %s is not the previous row's hash %s", short(prevHash), short(*expectPrev))}
	}
	prevRaw, err := hex.DecodeString(*expectPrev)
	if err != nil || len(prevRaw) != chainHashLen {
		return &ChainBreak{Seq: r.Seq, ID: r.ID, Kind: BreakRelinked, Detail: "the previous hash is not a valid hash"}
	}
	var prev [chainHashLen]byte
	copy(prev[:], prevRaw)
	sum, err := chainHash(prev, r)
	if err != nil {
		return &ChainBreak{Seq: r.Seq, ID: r.ID, Kind: BreakUnreadable, Detail: err.Error()}
	}
	if !strings.EqualFold(hex.EncodeToString(sum[:]), rowHash) {
		return &ChainBreak{Seq: r.Seq, ID: r.ID, Kind: BreakEdited,
			Detail: fmt.Sprintf("stored hash %s is not the hash of the row's contents %s", short(rowHash), short(hex.EncodeToString(sum[:])))}
	}
	*expectSeq++
	*expectPrev = strings.ToLower(rowHash)
	return nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

// ChainedTenants lists every tenant that has a chain to verify: a head row, or chained
// rows with no head (which is itself a break, and must not be invisible because the
// thing that would name the tenant is the thing that is missing).
//
// It crosses tenants, so it is for an operator connection and for nothing on a request
// path, and it reads ids, never a row. The plugin's own HTTP surface verifies the
// caller's tenant only.
func ChainedTenants(ctx context.Context, db plugin.PluginDB, dialect plugin.Dialect) ([]uuid.UUID, error) {
	col := "tenant_id"
	if dialect == plugin.DialectMSSQL {
		col = "CONVERT(varchar(36), tenant_id)"
	}
	ctx = plugin.AcrossAllTenants(ctx, "audit verify: list the tenants that have a chain")
	rows, err := db.Query(ctx, fmt.Sprintf(
		`SELECT %[1]s FROM audit_chain_heads UNION SELECT %[1]s FROM audit_events WHERE seq IS NOT NULL`, col))
	if err != nil {
		return nil, fmt.Errorf("audit verify: list tenants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []uuid.UUID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("audit verify: list tenants: scan: %w", err)
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("audit verify: tenant id %q is not a UUID: %w", raw, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit verify: list tenants: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}
