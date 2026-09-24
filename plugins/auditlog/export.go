package auditlog

// Exporting a tenant's audit log (cleat#2047).
//
// The export is JSON Lines: one `event` record per audit row, then one `checkpoint`
// record. It is streamed and paged by a keyset cursor, so a tenant with years of history
// costs a page of memory, not the history.
//
// TWO PROPERTIES THE FORMAT EXISTS TO GIVE.
//
//  1. A short export must not read as a complete one. The `checkpoint` record is the last
//     line and says the stream reached its end; a consumer that did not see one has a
//     truncated export. (The HTTP handler also aborts the connection on a mid-stream
//     error, so a transport that reports truncation does.) GET /audit/events skips a row
//     that fails to scan and ignores a malformed parameter; an export does neither.
//
//  2. Every event record is verifiable offline. It carries prev_hash and hash beside its
//     own fields, in the exact strings that were hashed (a canonical UTC microsecond
//     timestamp, canonical-JSON metadata), so a consumer needs no database: see
//     testdata/audit_chain_reference.py verify-export.
//
// The cursor is a POSITION, not data, and is not covered by the hash. A range (from/to) is
// applied to the timestamp column, so an export of a range has gaps in seq: consecutive
// records still link, and the gap is where the range excluded rows.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// exportPageSize is how many rows one query reads. A variable so a test can make a small
// export cross page boundaries.
var exportPageSize = 1000

// ErrExportGap is wrapped by the error an export returns when the chained rows it read were
// not an unbroken run, so it cannot honestly end in a checkpoint.
var ErrExportGap = errors.New("audit export: the chained rows are not contiguous")

// ErrBadCursor is returned, before any record is emitted, for a cursor this code did not
// issue.
var ErrBadCursor = errors.New("audit export: the cursor is not one this export issued")

// ExportOptions selects what to export.
type ExportOptions struct {
	// From and To bound the row timestamp, inclusive, at microsecond resolution.
	From, To *time.Time
	// Cursor resumes after the record that carried it. Empty starts at the beginning.
	Cursor string
}

// exportEvent is one event line. The field order is the order on the wire.
type exportEvent struct {
	Type       string          `json:"type"`
	Cursor     string          `json:"cursor"`
	ID         string          `json:"id"`
	TenantID   string          `json:"tenant_id"`
	Seq        *int64          `json:"seq"`
	Timestamp  string          `json:"timestamp"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	StatusCode *int64          `json:"status_code"`
	UserID     *string         `json:"user_id"`
	IPAddress  *string         `json:"ip_address"`
	UserAgent  *string         `json:"user_agent"`
	DurationMs *int64          `json:"duration_ms"`
	Metadata   json.RawMessage `json:"metadata"`
	PrevHash   *string         `json:"prev_hash"`
	Hash       *string         `json:"hash"`
}

// exportCheckpoint is the last line. HeadSeq and HeadHash are the tenant's chain head at
// the moment the export began: chained rows appended after that are not in this export
// and are the next one's.
//
// From, To and AfterSeq say WHICH KIND of export this was, and so which completeness rules
// an offline verifier may apply: with none of them set the export claims the whole chain
// from the floor to the head; with AfterSeq it claims the chain after that seq; with a range
// it claims nothing about coverage, only that every record is intact. The checkpoint is
// not signed: the ends of a file are binding only against an anchor recorded elsewhere.
type exportCheckpoint struct {
	Type      string  `json:"type"`
	TenantID  string  `json:"tenant_id"`
	HeadSeq   int64   `json:"head_seq"`
	HeadHash  string  `json:"head_hash"`
	FloorSeq  int64   `json:"floor_seq"`
	FloorHash string  `json:"floor_hash"`
	From      *string `json:"from"`
	To        *string `json:"to"`
	AfterSeq  *int64  `json:"after_seq"`
	Events    int64   `json:"events"`
	// Unchained is how many of Events carry no seq or hash: rows written before the chain
	// existed. They are covered by nothing, so a caller who wants to notice one being added
	// or removed records this beside the head and floor. It is NOT the tenant's current
	// count: retention removes old unchained rows, so the count of an earlier export is the
	// one to compare an earlier export with.
	Unchained int64 `json:"unchained"`
}

// ExportTenant streams tenant's audit rows to emit, one JSON line per call (newline
// included), and ends with a checkpoint line. emit must not retain the slice.
//
// Order: rows written before the chain existed (no seq) first, by timestamp then id, then
// the chained rows by seq. It returns ErrBadCursor before emitting anything if the cursor
// is not valid. Any other error may arrive after records were emitted; the caller then
// has a truncated export, which the missing checkpoint line makes visible.
func ExportTenant(ctx context.Context, db plugin.PluginDB, dialect plugin.Dialect, tenant uuid.UUID,
	opts ExportOptions, emit func(line []byte) error) error {
	ctx = plugin.ForTenant(ctx, tenant)

	phase, afterMicros, afterID, afterSeq, err := parseCursor(opts.Cursor)
	if err != nil {
		return err
	}
	cursorSeq := afterSeq

	var headSeq, floorSeq int64
	var headHash, floorHash string
	bound := int64(math.MaxInt64)
	err = plugin.ScanRow(db.QueryRow(ctx, plugin.Rebind(
		`SELECT seq, hash, floor_seq, floor_hash FROM audit_chain_heads WHERE tenant_id = $1`, dialect), tenant),
		&headSeq, &headHash, &floorSeq, &floorHash)
	switch {
	case err == nil:
		bound = headSeq
		headHash, floorHash = strings.TrimSpace(headHash), strings.TrimSpace(floorHash)
	case errors.Is(err, sql.ErrNoRows):
		headHash, floorHash = zeroHashHex, zeroHashHex
	default:
		return fmt.Errorf("audit export: read head: %w", err)
	}

	var n, unchained int64
	send := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return emit(append(b, '\n'))
	}

	if phase == "u" || phase == "" {
		for {
			evs, more, err := exportPage(ctx, db, dialect, tenant, opts, false, afterMicros, afterID, 0, bound)
			if err != nil {
				return err
			}
			for _, e := range evs {
				if err := send(e.event); err != nil {
					return err
				}
				n++
				unchained++
				afterMicros, afterID = e.micros, e.event.ID
			}
			if !more {
				break
			}
		}
	}
	// Without a range, the chained rows must be an unbroken run: the first is the one after
	// the floor (or after the cursor), and each next is the previous plus one. A hole means a
	// retention sweep removed rows between pages, or a row is missing, and an export that
	// went on to end in a checkpoint would look complete with a silent gap in it. Fail
	// instead: the HTTP handler aborts the connection, cleatctl says INCOMPLETE, and the
	// caller repeats the export. A range legitimately has gaps and is exempt.
	continuous := opts.From == nil && opts.To == nil
	expectSeq := afterSeq + 1
	if phase != "c" {
		expectSeq = floorSeq + 1
	}
	for {
		evs, more, err := exportPage(ctx, db, dialect, tenant, opts, true, 0, "", afterSeq, bound)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if continuous && *e.event.Seq != expectSeq {
				return fmt.Errorf("audit export: expected seq %d and found seq %d: the rows between were removed while the export ran (a retention sweep) or are missing. The export is incomplete; repeat it (%w)",
					expectSeq, *e.event.Seq, ErrExportGap)
			}
			expectSeq = *e.event.Seq + 1
			if err := send(e.event); err != nil {
				return err
			}
			n++
			afterSeq = *e.event.Seq
		}
		if !more {
			break
		}
	}
	cp := exportCheckpoint{Type: "checkpoint", TenantID: tenant.String(), HeadSeq: headSeq, HeadHash: headHash,
		FloorSeq: floorSeq, FloorHash: floorHash, Events: n, Unchained: unchained}
	if opts.From != nil {
		v := canonicalTimestamp(*opts.From)
		cp.From = &v
	}
	if opts.To != nil {
		v := canonicalTimestamp(*opts.To)
		cp.To = &v
	}
	if phase == "c" {
		v := cursorSeq
		cp.AfterSeq = &v
	}
	return send(cp)
}

type exportRow struct {
	event  exportEvent
	micros int64
}

// exportPage reads one page. chained selects the phase; more says the page was full, so
// another may follow.
func exportPage(ctx context.Context, db plugin.PluginDB, dialect plugin.Dialect, tenant uuid.UUID, opts ExportOptions,
	chained bool, afterMicros int64, afterID string, afterSeq, bound int64) ([]exportRow, bool, error) {

	ts := epochMicrosExpr(dialect, "timestamp")
	var where []string
	args := []any{tenant.String()}
	next := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	where = append(where, "tenant_id = $1")

	// Placeholders are numbered in the order they appear in the text: MySQL binds `?`
	// by appearance, and the rebind turns $N into `?`.
	if chained {
		where = append(where, "seq IS NOT NULL", "seq > "+next(afterSeq), "seq <= "+next(bound))
	} else {
		where = append(where, "seq IS NULL")
		if afterID != "" {
			a, b, c := next(afterMicros), next(afterMicros), next(afterID)
			where = append(where, fmt.Sprintf("(%s > %s OR (%s = %s AND id > %s))", ts, a, ts, b, c))
		}
	}
	if opts.From != nil {
		where = append(where, ts+" >= "+next(opts.From.UTC().UnixMicro()))
	}
	if opts.To != nil {
		where = append(where, ts+" <= "+next(opts.To.UTC().UnixMicro()))
	}
	order := "seq"
	if !chained {
		order = ts + ", id"
	}
	limit := next(exportPageSize)

	query := plugin.Rebind(fmt.Sprintf(`
		SELECT id, seq, %s, method, path, status_code, user_id, ip_address, user_agent, duration_ms, metadata,
		       prev_hash, row_hash
		FROM audit_events
		WHERE %s
		ORDER BY %s %s`, ts, strings.Join(where, " AND "), order, plugin.LimitClause(limit, dialect)), dialect)

	// A deadlock victim (SQL Server picks a reader against a retention sweep) is repeated:
	// nothing of this page has been emitted yet.
	var rows plugin.Rows
	var err error
	for attempt := 1; ; attempt++ {
		if rows, err = db.Query(ctx, query, args...); err == nil || !isTransientDBError(err) || attempt >= verifyAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(time.Duration(attempt) * 25 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, false, fmt.Errorf("audit export: read rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []exportRow
	for rows.Next() {
		var (
			id                                      uuid.UUID
			seq, status, dur                        sql.NullInt64
			micros                                  int64
			method, path                            string
			userID, ip, ua, meta, prevHash, rowHash sql.NullString
		)
		if err := plugin.ScanRow(rows, &id, &seq, &micros, &method, &path, &status, &userID, &ip, &ua, &dur, &meta, &prevHash, &rowHash); err != nil {
			return nil, false, fmt.Errorf("audit export: scan row: %w", err)
		}
		ev := exportEvent{
			Type: "event", ID: id.String(), TenantID: tenant.String(),
			Timestamp: canonicalTimestamp(time.UnixMicro(micros)),
			Method:    method, Path: path,
			StatusCode: intp(status), UserID: strp2(userID), IPAddress: strp2(ip), UserAgent: strp2(ua), DurationMs: intp(dur),
		}
		if meta.Valid {
			canon, cerr := canonicalJSON(meta.String)
			switch {
			case cerr == nil:
				ev.Metadata = json.RawMessage(canon)
			case chained:
				// A chained row's metadata is hashed, so a value that is not JSON is a
				// row that cannot be verified: say so rather than export a different value.
				return nil, false, fmt.Errorf("audit export: metadata of row %s is not JSON: %w", id, cerr)
			}
		}
		if chained {
			ev.Seq = intp(seq)
			ev.PrevHash, ev.Hash = strp2(prevHash), strp2(rowHash)
			if ev.PrevHash != nil {
				*ev.PrevHash = strings.ToLower(strings.TrimSpace(*ev.PrevHash))
			}
			if ev.Hash != nil {
				*ev.Hash = strings.ToLower(strings.TrimSpace(*ev.Hash))
			}
			ev.Cursor = encodeCursor("c", 0, "", ev.seqValue())
		} else {
			ev.Cursor = encodeCursor("u", micros, ev.ID, 0)
		}
		out = append(out, exportRow{event: ev, micros: micros})
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("audit export: read rows: %w", err)
	}
	return out, len(out) == exportPageSize, nil
}

func (e exportEvent) seqValue() int64 {
	if e.Seq == nil {
		return 0
	}
	return *e.Seq
}

func intp(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

func strp2(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	s := n.String
	return &s
}

func encodeCursor(phase string, micros int64, id string, seq int64) string {
	var raw string
	if phase == "c" {
		raw = "c:" + strconv.FormatInt(seq, 10)
	} else {
		raw = "u:" + strconv.FormatInt(micros, 10) + ":" + id
	}
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// parseCursor returns the phase to start in ("" for the beginning) and the position in it.
func parseCursor(c string) (phase string, micros int64, id string, seq int64, err error) {
	if c == "" {
		return "", 0, "", 0, nil
	}
	b, derr := base64.RawURLEncoding.DecodeString(c)
	if derr != nil {
		return "", 0, "", 0, ErrBadCursor
	}
	parts := strings.SplitN(string(b), ":", 3)
	switch {
	case len(parts) == 2 && parts[0] == "c":
		s, perr := strconv.ParseInt(parts[1], 10, 64)
		if perr != nil || s < 0 {
			return "", 0, "", 0, ErrBadCursor
		}
		return "c", 0, "", s, nil
	case len(parts) == 3 && parts[0] == "u":
		m, perr := strconv.ParseInt(parts[1], 10, 64)
		if _, uerr := uuid.Parse(parts[2]); perr != nil || uerr != nil {
			return "", 0, "", 0, ErrBadCursor
		}
		return "u", m, parts[2], 0, nil
	}
	return "", 0, "", 0, ErrBadCursor
}
