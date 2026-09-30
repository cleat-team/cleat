// Package webhookingest behavioral tests — fake DB + in-memory store, no PostgreSQL.
package webhookingest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/cleat-team/cleat/plugins/plugintest"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// In-memory fake DB store
// ---------------------------------------------------------------------------

type webhookSourceRow struct {
	id                  string
	tenantID            string
	name                string
	sourceType          string
	secretConfigured    bool
	enabled             bool
	correlationKeyField string
	createdAt           time.Time
	updatedAt           time.Time
	deleted             bool
}

type webhookEventRow struct {
	id         string
	sourceID   string
	tenantID   string
	eventType  string
	headers    string
	payload    string
	receivedAt time.Time
	processed  bool
	status     string
	errorMsg   *string
}

type fakeDBStore struct {
	mu              sync.RWMutex
	sources         []webhookSourceRow
	events          []webhookEventRow
	apiKeys         map[string]string // key_hash_hex -> tenant_id
	failNextQuery   bool
	failNextExec    bool
	querySkip       int
	execSkip        int
	corruptNextScan bool
	corruptScanSkip int
	failNextRowsErr bool
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		sources: make([]webhookSourceRow, 0),
		events:  make([]webhookEventRow, 0),
		apiKeys: make(map[string]string),
	}
}

// ---------------------------------------------------------------------------
// Fake SQL driver
// ---------------------------------------------------------------------------

type fakeConnector struct {
	store *fakeDBStore
}

func (c *fakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &fakeConn{store: c.store}, nil
}
func (c *fakeConnector) Driver() driver.Driver { return &fakeDrv{} }

type fakeDrv struct{}

func (*fakeDrv) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeDriver: use sql.OpenDB")
}

type fakeConn struct {
	store *fakeDBStore
}

func (*fakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Prepare call")
}
func (*fakeConn) Close() error              { return nil }
func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

// --- ExecContext ---

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	if c.store.failNextExec {
		if c.store.execSkip > 0 {
			c.store.execSkip--
		} else {
			c.store.failNextExec = false
			return nil, fmt.Errorf("simulated exec error")
		}
	}

	switch {
	case strings.Contains(query, "INSERT INTO webhook_sources"):
		return c.execInsertSource(args)
	case strings.Contains(query, "INSERT INTO webhook_events"):
		return c.execInsertEvent(args)
	// cleat-review on #2221: handleDeleteSource cancels a deleted source's
	// own pending events in the same transaction as the soft-delete. Matched
	// on "SET status = 'cancelled'", a literal specific to this one
	// statement -- it is the only webhook_events UPDATE keyed by source_id
	// rather than by event id, so routing it into execUpdateEvent (which
	// reads args[0] as an event id) would silently no-op: no error, no
	// stored event ever marked cancelled, and every assertion the caller
	// makes about the DELETE response itself would still pass.
	case strings.Contains(query, "UPDATE webhook_events") && strings.Contains(query, "SET status = 'cancelled'"):
		return c.execCancelPendingEventsForSource(args)
	// cleat#2649: handleDeleteSource's THIRD statement, same transaction,
	// cancelling any still-unclaimed correlated event (key1 = the deleted
	// source's id) in eventtriggers' own table -- see routes.go's comment on
	// why cleat#2199's guarantee has to reach this table too now that
	// awaitWebhook claims from it instead of webhook_events. This fake never
	// populates ingested_events (awaitWebhook's real dialect-specific claim
	// query is tested against real databases instead -- see
	// a_deleted_sources_pending_event_is_cancelled_not_delivered_multidb_test.go,
	// which pairs eventtriggers.Plugin{} into its migration setup and asserts
	// awaitWebhook returns found=false after a delete; a hand-rolled fake
	// cannot faithfully represent FOR UPDATE SKIP LOCKED), so this is a no-op
	// success: nothing here asserts on ingested_events state, only that the
	// statement does not abort the DELETE's transaction.
	case strings.Contains(query, "UPDATE ingested_events"):
		return &fakeResult{rowsAffected: 0}, nil
	// cleat#2199: soft delete (UPDATE ... SET enabled = false, deleted_at =
	// ...), not a real DELETE -- the production query's literal SET clause
	// distinguishes it from every other webhook_sources UPDATE this fake
	// could see (there are none today, but matching a literal substring
	// specific to this statement rather than the bare table name is the same
	// discipline the SELECT routing below already needs).
	case strings.Contains(query, "UPDATE webhook_sources") && strings.Contains(query, "deleted_at"):
		return c.execDeleteSource(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query)
	}
}

// --- QueryContext ---

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	// Check fail flag under write lock, then proceed.
	c.store.mu.Lock()
	failed := c.store.failNextQuery
	if failed && c.store.querySkip > 0 {
		c.store.querySkip--
		failed = false
	}
	if failed {
		c.store.failNextQuery = false
	}
	corrupt := c.store.corruptNextScan
	if corrupt && c.store.corruptScanSkip > 0 {
		c.store.corruptScanSkip--
		corrupt = false
	}
	if corrupt {
		c.store.corruptNextScan = false
	}
	failRowsErr := c.store.failNextRowsErr
	if failRowsErr {
		c.store.failNextRowsErr = false
	}
	c.store.mu.Unlock()

	if failed {
		return nil, fmt.Errorf("simulated query error")
	}

	if failRowsErr {
		return &rowsErrFakeRows{}, nil
	}

	switch {
	case strings.Contains(query, "tenant_api_keys") && strings.Contains(query, "tenant_id"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryTenantLookup(args)
	case strings.Contains(query, "SELECT id, tenant_id, name, source_type, secret_configured, enabled, correlation_key_field, created_at, updated_at"):
		// "WHERE id = $1 AND" used to be specific to handleGetSource's
		// two-argument query (id, tenant_id). cleat#2199 added
		// "AND deleted_at IS NULL" to handleIngestWebhook's one-argument
		// (id only) query too, so both now contain that substring --
		// matched on "tenant_id = $2" instead, which only handleGetSource's
		// query has.
		if strings.Contains(query, "WHERE id = $1 AND tenant_id = $2") {
			c.store.mu.RLock()
			defer c.store.mu.RUnlock()
			return c.queryGetSource(args)
		}
		if strings.Contains(query, "WHERE id = $1") {
			c.store.mu.RLock()
			defer c.store.mu.RUnlock()
			return c.querySourceByID(args)
		}
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryListSources(args, corrupt)
	case strings.Contains(query, "SELECT id, source_id, tenant_id, event_type, headers, payload, received_at, processed"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryListEvents(query, args, corrupt)
	// cleat-review on #2221 joined webhook_sources into this query (for the
	// deleted_at guard, see host_functions.go), so it now shares the same
	// "FROM webhook_events e ... LEFT JOIN webhook_sources s" shape as
	// queryProcessBatch below -- matched, and ordered ahead of that case,
	// on the select list instead, which the two queries do not share.
	case strings.Contains(query, "SELECT e.id, e.event_type, e.payload, e.received_at"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryAwaitEvents(query, args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query)
	}
}

// ---------------------------------------------------------------------------
// Exec implementations
// ---------------------------------------------------------------------------

func (c *fakeConn) execInsertSource(args []driver.NamedValue) (driver.Result, error) {
	tenantID, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	id, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	name, err := argString(args, 3)
	if err != nil {
		return nil, err
	}
	sourceType, err := argString(args, 4)
	if err != nil {
		return nil, err
	}
	secretConfigured, err := argBool(args, 5)
	if err != nil {
		return nil, err
	}
	// created_at (6) and updated_at (7) are two DISTINCT arguments, both
	// carrying `now` -- see the production INSERT's own comment on why a
	// placeholder is never reused across two argument positions.
	nowVal, err := argTime(args, 6)
	if err != nil {
		return nil, err
	}

	var correlationKeyField string
	if len(args) >= 8 {
		if v, err := argString(args, 8); err == nil {
			correlationKeyField = v
		}
	}

	c.store.sources = append(c.store.sources, webhookSourceRow{
		id:                  id,
		tenantID:            tenantID,
		name:                name,
		sourceType:          sourceType,
		secretConfigured:    secretConfigured,
		enabled:             true,
		correlationKeyField: correlationKeyField,
		createdAt:           nowVal,
		updatedAt:           nowVal,
	})
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execInsertEvent(args []driver.NamedValue) (driver.Result, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	sourceID, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	tenantID, err := argString(args, 3)
	if err != nil {
		return nil, err
	}
	eventType, err := argString(args, 4)
	if err != nil {
		return nil, err
	}
	headers, err := argString(args, 5)
	if err != nil {
		return nil, err
	}
	payload, err := argString(args, 6)
	if err != nil {
		return nil, err
	}
	nowVal, err := argTime(args, 7)
	if err != nil {
		return nil, err
	}

	c.store.events = append(c.store.events, webhookEventRow{
		id:         id,
		sourceID:   sourceID,
		tenantID:   tenantID,
		eventType:  eventType,
		headers:    headers,
		payload:    payload,
		receivedAt: nowVal,
		processed:  false,
		status:     "pending",
	})
	return &fakeResult{rowsAffected: 1}, nil
}

// execCancelPendingEventsForSource is cleat-review on #2221's delete-time
// cancellation: production's `UPDATE webhook_events SET status =
// 'cancelled', processed = true, error_msg = 'source deleted' WHERE
// source_id = $1 AND tenant_id = $2 AND processed = false AND (status =
// 'pending' OR status IS NULL)`. 'cancelled' and the DEFAULT 'pending' are
// this table's only two live statuses since cleat#2689 retired the
// background retry sweep that used to also produce 'completed'/
// 'dead_letter'. Owner decision on cleat#2199: a delete stops a future
// await_webhook call from ever reaching this event too (see the THIRD
// statement in routes.go's handleDeleteSource, against ingested_events).
func (c *fakeConn) execCancelPendingEventsForSource(args []driver.NamedValue) (driver.Result, error) {
	sourceID, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tenantID, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	var affected int64
	for i, evt := range c.store.events {
		if evt.sourceID != sourceID || evt.tenantID != tenantID || evt.processed {
			continue
		}
		if evt.status != "pending" && evt.status != "" {
			continue
		}
		reason := "source deleted"
		c.store.events[i].status = "cancelled"
		c.store.events[i].processed = true
		c.store.events[i].errorMsg = &reason
		affected++
	}
	return &fakeResult{rowsAffected: affected}, nil
}

// execDeleteSource is cleat#2199's soft delete: production's
// `UPDATE webhook_sources SET enabled = false, deleted_at = $1 WHERE id = $2
// AND tenant_id = $3 AND deleted_at IS NULL` -- $1 is the deletion
// timestamp, not an id, matching the argument order a hand-typed
// falsification of this file would get wrong first.
func (c *fakeConn) execDeleteSource(args []driver.NamedValue) (driver.Result, error) {
	id, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 3)
	if err != nil {
		return nil, err
	}

	for i, src := range c.store.sources {
		if src.id == id && src.tenantID == tid && !src.deleted {
			c.store.sources[i].deleted = true
			c.store.sources[i].enabled = false
			return &fakeResult{rowsAffected: 1}, nil
		}
	}
	return &fakeResult{rowsAffected: 0}, nil
}

// ---------------------------------------------------------------------------
// Query implementations
// ---------------------------------------------------------------------------

func (c *fakeConn) queryTenantLookup(args []driver.NamedValue) (driver.Rows, error) {
	keyHash, err := argBytes(args, 1)
	if err != nil {
		return nil, err
	}
	hashHex := fmt.Sprintf("%x", keyHash)
	tid, ok := c.store.apiKeys[hashHex]
	if !ok {
		return &fakeRows{columns: []string{"tenant_id"}}, nil
	}
	return &fakeRows{
		columns: []string{"tenant_id"},
		data:    [][]driver.Value{{tid}},
	}, nil
}

func (c *fakeConn) queryListSources(args []driver.NamedValue, corrupt bool) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	var results []webhookSourceRow
	for _, s := range c.store.sources {
		if s.tenantID == tid && !s.deleted {
			results = append(results, s)
		}
	}

	columns := []string{"id", "tenant_id", "name", "source_type", "secret_configured", "enabled", "correlation_key_field", "created_at", "updated_at"}
	var data [][]driver.Value
	for i, s := range results {
		enabled := driver.Value(s.enabled)
		if corrupt && i == 0 {
			enabled = "not-a-bool"
		}
		data = append(data, []driver.Value{
			s.id, s.tenantID, s.name, s.sourceType, s.secretConfigured,
			enabled, s.correlationKeyField,
			s.createdAt, s.updatedAt,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) querySourceByID(args []driver.NamedValue) (driver.Rows, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	for _, s := range c.store.sources {
		if s.id == id && !s.deleted {
			return &fakeRows{
				columns: []string{"id", "tenant_id", "name", "source_type", "secret_configured", "enabled", "correlation_key_field", "created_at", "updated_at"},
				data: [][]driver.Value{{
					s.id, s.tenantID, s.name, s.sourceType, s.secretConfigured,
					s.enabled, s.correlationKeyField,
					s.createdAt, s.updatedAt,
				}},
			}, nil
		}
	}
	return &fakeRows{columns: []string{"id", "tenant_id", "name", "source_type", "secret_configured", "enabled", "correlation_key_field", "created_at", "updated_at"}}, nil
}

func (c *fakeConn) queryGetSource(args []driver.NamedValue) (driver.Rows, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	for _, s := range c.store.sources {
		if s.id == id && s.tenantID == tid && !s.deleted {
			return &fakeRows{
				columns: []string{"id", "tenant_id", "name", "source_type", "secret_configured", "enabled", "correlation_key_field", "created_at", "updated_at"},
				data: [][]driver.Value{{
					s.id, s.tenantID, s.name, s.sourceType, s.secretConfigured,
					s.enabled, s.correlationKeyField,
					s.createdAt, s.updatedAt,
				}},
			}, nil
		}
	}
	return &fakeRows{columns: []string{"id", "tenant_id", "name", "source_type", "secret_configured", "enabled", "correlation_key_field", "created_at", "updated_at"}}, nil
}

func (c *fakeConn) queryListEvents(query string, args []driver.NamedValue, corrupt bool) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	var results []webhookEventRow
	for _, e := range c.store.events {
		if e.tenantID == tid {
			results = append(results, e)
		}
	}

	// Apply optional filters.
	nextArg := 2

	// source_id filter
	if strings.Contains(query, "AND source_id = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if sid, ok := v.(string); ok {
				var filtered []webhookEventRow
				for _, e := range results {
					if e.sourceID == sid {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// event_type filter
	if strings.Contains(query, "AND event_type = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if et, ok := v.(string); ok {
				var filtered []webhookEventRow
				for _, e := range results {
					if e.eventType == et {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// processed filter
	if strings.Contains(query, "AND processed = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if p, ok := v.(bool); ok {
				var filtered []webhookEventRow
				for _, e := range results {
					if e.processed == p {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// Sort by received_at DESC
	sorted := make([]webhookEventRow, len(results))
	copy(sorted, results)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].receivedAt.After(sorted[i].receivedAt) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	results = sorted

	// Apply LIMIT
	for i := nextArg; i <= len(args); i++ {
		if v, err := argAny(args, i); err == nil {
			if lim, ok := v.(int64); ok && lim > 0 && int(lim) < len(results) {
				results = results[:lim]
				break
			}
		}
	}

	rows, err := c.buildEventRows(results, corrupt)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (c *fakeConn) queryAwaitEvents(query string, args []driver.NamedValue) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	var results []webhookEventRow
	for _, e := range c.store.events {
		if e.tenantID != tid || e.processed {
			continue
		}
		// Mirrors production's `AND (e.status IS NULL OR e.status !=
		// 'cancelled') AND s.deleted_at IS NULL` (cleat-review on #2221,
		// owner decision on cleat#2199): a second, independent guard on top
		// of execCancelPendingEventsForSource setting processed=true above,
		// the same belt-and-suspenders shape queryProcessBatch already has.
		if e.status == "cancelled" || c.sourceDeleted(e.sourceID) {
			continue
		}
		results = append(results, e)
	}

	nextArg := 2

	// source_id filter
	if strings.Contains(query, "AND source_id = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if sid, ok := v.(string); ok {
				var filtered []webhookEventRow
				for _, e := range results {
					if e.sourceID == sid {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// event_type filter
	if strings.Contains(query, "AND event_type = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if et, ok := v.(string); ok {
				var filtered []webhookEventRow
				for _, e := range results {
					if e.eventType == et {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		// Ineffectual only because this is the last clause. Keeping it is what
		// makes the next filter added below read the right $N instead of
		// silently reusing this one's. Same reasoning as the //nolint'd
		// increments in this plugin's own host_functions.go.
		nextArg++ //nolint:ineffassign,staticcheck // trailing counter; see above
	}

	// Sort by received_at DESC, take first
	if len(results) > 0 {
		best := results[0]
		for _, e := range results[1:] {
			if e.receivedAt.After(best.receivedAt) {
				best = e
			}
		}
		results = []webhookEventRow{best}
	}

	columns := []string{"id", "event_type", "payload", "received_at"}
	var data [][]driver.Value
	for _, e := range results {
		data = append(data, []driver.Value{
			e.id, e.eventType, []byte(e.payload), e.receivedAt,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

// sourceDeleted reports whether id names a source this store has soft-
// deleted, or no source at all -- a LEFT JOIN with no matching row also
// reads s.deleted_at as NULL, so this returns false in that case too,
// matching the real query's (deliberately) permissive behaviour for an
// orphaned event.
func (c *fakeConn) sourceDeleted(id string) bool {
	for _, s := range c.store.sources {
		if s.id == id {
			return s.deleted
		}
	}
	return false
}

func (c *fakeConn) buildEventRows(events []webhookEventRow, corrupt bool) (driver.Rows, error) {
	columns := []string{"id", "source_id", "tenant_id", "event_type", "headers", "payload", "received_at", "processed"}
	var data [][]driver.Value
	for i, e := range events {
		processed := driver.Value(e.processed)
		if corrupt && i == 0 {
			processed = "not-a-bool"
		}
		data = append(data, []driver.Value{
			e.id, e.sourceID, e.tenantID, e.eventType,
			[]byte(e.headers), []byte(e.payload), e.receivedAt, processed,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

// ---------------------------------------------------------------------------
// Argument extractors
// ---------------------------------------------------------------------------

func argString(args []driver.NamedValue, ordinal int) (string, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			switch v := a.Value.(type) {
			case string:
				return v, nil
			case []byte:
				return string(v), nil
			default:
				return "", fmt.Errorf("arg %d: want string, got %T", ordinal, a.Value)
			}
		}
	}
	return "", fmt.Errorf("arg %d not found", ordinal)
}

func argBytes(args []driver.NamedValue, ordinal int) ([]byte, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			b, ok := a.Value.([]byte)
			if !ok {
				return nil, fmt.Errorf("arg %d: want []byte, got %T", ordinal, a.Value)
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

func argInt64(args []driver.NamedValue, ordinal int) (int64, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			switch v := a.Value.(type) {
			case int64:
				return v, nil
			case float64:
				return int64(v), nil
			default:
				return 0, fmt.Errorf("arg %d: want int64, got %T", ordinal, a.Value)
			}
		}
	}
	return 0, fmt.Errorf("arg %d not found", ordinal)
}

func argBool(args []driver.NamedValue, ordinal int) (bool, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			b, ok := a.Value.(bool)
			if !ok {
				return false, fmt.Errorf("arg %d: want bool, got %T", ordinal, a.Value)
			}
			return b, nil
		}
	}
	return false, fmt.Errorf("arg %d not found", ordinal)
}

func argTime(args []driver.NamedValue, ordinal int) (time.Time, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			t, ok := a.Value.(time.Time)
			if !ok {
				return time.Time{}, fmt.Errorf("arg %d: want time.Time, got %T", ordinal, a.Value)
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("arg %d not found", ordinal)
}

func argAny(args []driver.NamedValue, ordinal int) (driver.Value, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			return a.Value, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

// ---------------------------------------------------------------------------
// driver.Result / driver.Rows stubs
// ---------------------------------------------------------------------------

type fakeResult struct {
	rowsAffected int64
}

func (r *fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (r *fakeResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type fakeRows struct {
	columns []string
	data    [][]driver.Value
	pos     int
}

func (r *fakeRows) Columns() []string { return r.columns }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// ---------------------------------------------------------------------------
// Test setup
// ---------------------------------------------------------------------------

var testTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
var testTenantStr = testTenantID.String()

func setupTestPlugin(t *testing.T) (*Plugin, http.Handler, *fakeDBStore) {
	t.Helper()

	store := newFakeDBStore()

	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: plugintest.NewFakeSecrets(),
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// Auth middleware for management routes; ingest route works without auth.
	handler := auth.MiddlewareWithMux(engine.NewPostgresStore(db), false, mux)(mux)
	return p, handler, store
}

func authedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer test-api-key")
	return req
}

// ---------------------------------------------------------------------------
// Behavioral tests
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Fake function registry
// ---------------------------------------------------------------------------

type fakeFuncRegistry struct {
	funcs map[string]plugin.PluginFunc
}

func newFakeFuncRegistry() *fakeFuncRegistry {
	return &fakeFuncRegistry{funcs: map[string]plugin.PluginFunc{}}
}

func (r *fakeFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	r.funcs[opts.Name] = fn
	return nil
}

func (r *fakeFuncRegistry) Has(name string) bool {
	_, ok := r.funcs[name]
	return ok
}

func (r *fakeFuncRegistry) Get(name string) plugin.PluginFunc {
	return r.funcs[name]
}

// errReadCloser simulates an io.ReadCloser that fails on Read.
type errReadCloser struct{}

func (*errReadCloser) Read(_ []byte) (int, error) { return 0, fmt.Errorf("simulated read error") }
func (*errReadCloser) Close() error               { return nil }

// rowsErrFakeRows returns a non-EOF error from Next() and non-nil from Err().
type rowsErrFakeRows struct{ pos int }

func (*rowsErrFakeRows) Columns() []string { return []string{"id"} }
func (*rowsErrFakeRows) Close() error      { return nil }
func (r *rowsErrFakeRows) Next(_ []driver.Value) error {
	if r.pos > 0 {
		return io.EOF
	}
	r.pos++
	return fmt.Errorf("simulated rows iteration error")
}
func (*rowsErrFakeRows) Err() error { return fmt.Errorf("simulated rows iteration error") }

// ===========================================================================
// RegisterHostFunctions
// ===========================================================================

func TestRegisterHostFunctions_NilRegistry(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	err := p.RegisterHostFunctions(nil)
	if err == nil || !strings.Contains(err.Error(), "nil function registry") {
		t.Fatalf("expected nil registry error, got: %v", err)
	}
}

func TestRegisterHostFunctions_Valid(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	reg := newFakeFuncRegistry()
	if err := p.RegisterHostFunctions(reg); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}
	if !reg.Has("await_webhook") {
		t.Error("expected await_webhook to be registered")
	}
}

// ===========================================================================
// Migrations
// ===========================================================================

func TestMigrations(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	migrations := p.Migrations()
	if len(migrations) == 0 {
		t.Error("expected at least one migration")
	}
	for i, m := range migrations {
		if m.Version == 0 {
			t.Errorf("migration %d: version must be non-zero", i)
		}
	}

	// cleat#1513 has landed; this is the helper rather than a variant of it.
	// The predicate here was `m.Up == ""`, which reads a MySQL-only migration
	// as doing nothing -- the drift the comment it replaces predicted.
	// cleat#1622.
	plugintest.AssertMigrationsDoSomething(t, migrations)
}

// ===========================================================================
// RegisterRoutes
// ===========================================================================

func TestRegisterRoutes_NilMux(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	err := p.RegisterRoutes(nil)
	if err == nil || !strings.Contains(err.Error(), "nil mux") {
		t.Fatalf("expected nil mux error, got: %v", err)
	}
}

func TestRegisterRoutes_Valid(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	// Verify each expected route is registered (does not return 404).
	routes := []struct{ method, path string }{
		{"POST", "/ingest/11111111-1111-1111-1111-111111111111"},
		{"GET", "/ingest/sources"},
		{"POST", "/ingest/sources"},
		{"GET", "/ingest/sources/11111111-1111-1111-1111-111111111111"},
		{"DELETE", "/ingest/sources/11111111-1111-1111-1111-111111111111"},
		{"GET", "/ingest/events"},
	}
	for _, r := range routes {
		req := httptest.NewRequest(r.method, r.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", r.method, r.path)
		}
	}
}

// TestCreateSource verifies creating a webhook ingestion source and
// reading it back.
func TestCreateSource(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	body := `{"name":"github-webhook","source_type":"github","secret":"my-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	id := created["id"].(string)
	if created["name"] != "github-webhook" {
		t.Errorf("expected name 'github-webhook', got %s", created["name"])
	}
	if created["source_type"] != "github" {
		t.Errorf("expected source_type 'github', got %s", created["source_type"])
	}
	if created["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", created["enabled"])
	}
	if endpoint, ok := created["endpoint_url"].(string); !ok || endpoint == "" {
		t.Errorf("expected non-empty endpoint_url, got %q", endpoint)
	}

	// Verify in store.
	store.mu.RLock()
	sourceCount := len(store.sources)
	store.mu.RUnlock()
	if sourceCount != 1 {
		t.Fatalf("expected 1 source in store, got %d", sourceCount)
	}

	// GET by ID.
	req = authedRequest("GET", "/ingest/sources/"+id, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var fetched map[string]any
	json.Unmarshal(rec.Body.Bytes(), &fetched)
	if fetched["name"] != "github-webhook" {
		t.Errorf("expected name 'github-webhook', got %s", fetched["name"])
	}
}

// TestListAndGetSourcesDoNotLeakSecret verifies that neither the list nor
// the get endpoint (nor create) ever returns the real HMAC secret in the
// response body.
func TestListAndGetSourcesDoNotLeakSecret(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	const realSecret = "do-not-leak-this-hmac-secret"
	body := `{"name":"github-webhook","source_type":"github","secret":"` + realSecret + `"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), realSecret) {
		t.Errorf("create response leaked the real secret: %s", rec.Body.String())
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	if _, present := created["secret"]; present {
		t.Errorf("create: response carries a \"secret\" key at all: %v", created["secret"])
	}
	if created["secret_configured"] != true {
		t.Errorf("create: expected secret_configured=true, got %v", created["secret_configured"])
	}
	id := created["id"].(string)

	// GET.
	req = authedRequest("GET", "/ingest/sources/"+id, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), realSecret) {
		t.Errorf("GET response leaked the real secret: %s", rec.Body.String())
	}
	var fetched map[string]any
	json.Unmarshal(rec.Body.Bytes(), &fetched)
	if _, present := fetched["secret"]; present {
		t.Errorf("GET: response carries a \"secret\" key at all: %v", fetched["secret"])
	}
	if fetched["secret_configured"] != true {
		t.Errorf("GET: expected secret_configured=true, got %v", fetched["secret_configured"])
	}

	// LIST.
	req = authedRequest("GET", "/ingest/sources", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), realSecret) {
		t.Errorf("LIST response leaked the real secret: %s", rec.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("LIST: failed to decode: %v", err)
	}
	found := false
	for _, s := range list {
		if s["id"] == id {
			found = true
			if _, present := s["secret"]; present {
				t.Errorf("LIST: response carries a \"secret\" key at all: %v", s["secret"])
			}
			if s["secret_configured"] != true {
				t.Errorf("LIST: expected secret_configured=true, got %v", s["secret_configured"])
			}
		}
	}
	if !found {
		t.Fatalf("expected source %s in list response", id)
	}
}

// TestCreateSourceDefaults verifies default values when creating a source
// without source_type.
func TestCreateSourceDefaults(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `{"name":"generic-source","secret":"test-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created["source_type"] != "generic" {
		t.Errorf("expected default source_type 'generic', got %s", created["source_type"])
	}
}

// TestIngestWebhookPayload verifies the full flow: create source → POST
// payload to ingest endpoint → event stored.
func TestIngestWebhookPayload(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source.
	createBody := `{"name":"test-source","source_type":"generic","secret":"test-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}

	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Ingest a webhook payload (no auth required on ingest endpoint, but a
	// valid signature is -- every source has a secret now, cleat#1992/#2172).
	payload := `{"action":"opened","issue":{"number":1}}`
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write([]byte(payload))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", sig)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var ingestResp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &ingestResp)
	if ingestResp["received"] != true {
		t.Errorf("expected received=true, got %v", ingestResp["received"])
	}
	eventID := ingestResp["id"].(string)

	// Verify event in store.
	store.mu.RLock()
	eventCount := len(store.events)
	store.mu.RUnlock()
	if eventCount != 1 {
		t.Fatalf("expected 1 event in store, got %d", eventCount)
	}

	store.mu.RLock()
	event := store.events[0]
	store.mu.RUnlock()
	if event.id != eventID {
		t.Errorf("expected event id %s, got %s", eventID, event.id)
	}
	if event.sourceID != sourceID {
		t.Errorf("expected source_id %s, got %s", sourceID, event.sourceID)
	}
	if event.processed {
		t.Errorf("expected processed=false")
	}

	// Verify the event is listed via GET /ingest/events.
	req = authedRequest("GET", "/ingest/events", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list events: expected 200, got %d", rec.Code)
	}

	var events []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 1 {
		t.Fatalf("expected 1 event in list, got %d", len(events))
	}
	if events[0]["id"] != eventID {
		t.Errorf("expected event id %s, got %s", eventID, events[0]["id"])
	}
}

// TestIngestWithHMACVerification verifies HMAC signature validation.
func TestIngestWithHMACVerification(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Create source with a secret.
	createBody := `{"name":"signed-source","source_type":"github","secret":"my-hmac-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}

	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Ingest with valid HMAC signature.
	payload := `{"action":"opened"}`
	mac := hmac.New(sha256.New, []byte("my-hmac-secret"))
	mac.Write([]byte(payload))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	req.Header.Set("X-Hub-Signature-256", sig)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid sig: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIngestInvalidHMAC verifies that an invalid HMAC signature is rejected.
func TestIngestInvalidHMAC(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	createBody := `{"name":"signed-source","source_type":"github","secret":"real-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}

	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Ingest with WRONG HMAC signature.
	payload := `{"action":"opened"}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	req.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid sig: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIngestMissingHMAC verifies that missing HMAC signature is rejected
// when the source has a secret.
func TestIngestMissingHMAC(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	createBody := `{"name":"signed-source","source_type":"github","secret":"some-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}

	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Ingest without HMAC signature.
	payload := `{"action":"opened"}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing sig: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIngestDisabledSource verifies that a disabled source returns 403.
func TestIngestDisabledSource(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	// Directly insert a disabled source.
	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:         sourceID.String(),
		tenantID:   testTenantStr,
		name:       "disabled",
		sourceType: "generic",
		enabled:    false,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: plugintest.NewFakeSecrets(),
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.MiddlewareWithMux(engine.NewPostgresStore(db), false, mux)(mux)

	req := httptest.NewRequest("POST", "/ingest/"+sourceID.String(), bytes.NewReader([]byte(`{"test":true}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for disabled source, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDeletingASourceCancelsItsPendingEvents pins cleat-review's finding on
// #2221: an event ingested before a source is deleted, still pending, is
// cancelled (status='cancelled', processed=true) in the same transaction as
// the soft-delete. Falsified by reverting handleDeleteSource's webhook_events
// UPDATE (the one that cancels the source's own pending events) back to a
// no-op: the assertion below then fails.
//
// This used to also assert a PUSH-path non-delivery (background.go's
// processBatch must never signal a cancelled event's bound workflow) and,
// before that, a PULL-path one too. Both mechanisms are gone now: the
// PUSH path (signal_workflow_id + the retry sweep) was retired in cleat#2689,
// and the PULL path (awaitWebhook) moved in cleat#2649 onto
// eventtriggers.ClaimOrRegisterAwaiter, which claims from ingested_events
// with a real, dialect-specific locking query (FOR UPDATE SKIP LOCKED and
// friends) this hand-rolled fake cannot faithfully represent -- see
// a_deleted_sources_pending_event_is_cancelled_not_delivered_multidb_test.go's
// TestADeletedSourcesPendingEventIsCancelledNotDelivered, which pins the PULL
// path against real databases instead.
func TestDeletingASourceCancelsItsPendingEvents(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:         sourceID.String(),
		tenantID:   testTenantStr,
		name:       "to-delete-with-pending-event",
		sourceType: "generic",
		enabled:    true,
	})

	eventID := uuid.New()
	store.events = append(store.events, webhookEventRow{
		id:         eventID.String(),
		sourceID:   sourceID.String(),
		tenantID:   testTenantStr,
		eventType:  "push",
		payload:    `{"hello":"world"}`,
		receivedAt: time.Now().Add(-30 * time.Second),
		processed:  false,
		status:     "pending",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: plugintest.NewFakeSecrets(),
	}
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	handler := auth.MiddlewareWithMux(engine.NewPostgresStore(db), false, mux)(mux)

	req := authedRequest("DELETE", "/ingest/sources/"+sourceID.String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	store.mu.RLock()
	evt := store.events[0]
	store.mu.RUnlock()
	if evt.status != "cancelled" {
		t.Fatalf("event status after delete: got %q, want %q", evt.status, "cancelled")
	}
	if !evt.processed {
		t.Fatalf("event processed after delete: got false, want true")
	}
}

// TestAwaitWebhookHostFunction and TestAwaitWebhookNoEvents used to live
// here, seeding this file's fake webhook_events-backed driver and asserting
// the found/not-found shape of awaitWebhook. cleat#2649 moved awaitWebhook
// onto eventtriggers.ClaimOrRegisterAwaiter, which claims from
// ingested_events through a real, dialect-specific locking query (FOR
// UPDATE SKIP LOCKED and friends) and, on no match, upserts an
// event_awaiters row -- two query shapes this hand-rolled fake has no
// concept of and cannot faithfully represent (see the comment on
// TestDeletingASourceCancelsItsPendingEvents, above, for the same reasoning
// applied to that test's former PULL-path assertion).
//
// Their coverage now lives in
// an_ingested_event_is_listed_and_awaited_multidb_test.go, against real
// databases -- TestAnIngestedEventIsListedAndAwaited claims a matching event
// and asserts a second call finds nothing (already consumed), and
// a_deleted_sources_pending_event_is_cancelled_not_delivered_multidb_test.go's
// TestADeletedSourcesPendingEventIsCancelledNotDelivered exercises the
// not-found/register-an-awaiter path. Both now pair eventtriggers.Plugin{}
// into their migration setup, which is also where eventtriggers' own
// equivalent claim/register tests (a_await_event_correlates_by_key_test.go)
// already live, so both sides of the shared mechanism are tested the same
// way.

// TestSourceDelete verifies deleting a webhook source.
//
// cleat#2199: this is a SOFT delete -- the row stays in the store (marked
// deleted, disabled), it does not vanish from it. See
// a_deleted_source_stays_gone_but_keeps_its_events_multidb_test.go for the
// end-to-end pin, against real databases, of what soft-delete is for: an
// event ingested before the delete has to survive it.
func TestSourceDelete(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	createBody := `{"name":"to-delete","source_type":"generic","secret":"test-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	req = authedRequest("DELETE", "/ingest/sources/"+id, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", rec.Code)
	}

	store.mu.RLock()
	count := len(store.sources)
	var deleted, enabled bool
	if count == 1 {
		deleted = store.sources[0].deleted
		enabled = store.sources[0].enabled
	}
	store.mu.RUnlock()
	if count != 1 {
		t.Fatalf("expected 1 source after a soft delete (the row is marked, not removed), got %d", count)
	}
	if !deleted {
		t.Errorf("source row after delete: deleted=false, want true")
	}
	if enabled {
		t.Errorf("source row after delete: enabled=true, want false")
	}

	// And it is unreachable through the API, which is the half of
	// soft-delete that has to look like a real delete to a caller.
	getReq := authedRequest("GET", "/ingest/sources/"+id, nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusNotFound {
		t.Errorf("GET after delete: expected 404, got %d", getRec.Code)
	}
}

// ===========================================================================
// HandleGetSource not found
// ===========================================================================

func TestGetSourceNotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/ingest/sources/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent source, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// CreateSource missing required fields
// ===========================================================================

func TestCreateSourceMissingName(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `{"source_type":"github"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing name, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] == nil {
		t.Error("expected error message in response")
	}
}

// TestCreateSourceMissingSecret is cleat#1992/#2172's pin for owner decision
// (b): a signing secret is now REQUIRED, not optional -- a POST with no
// secret at all is rejected outright, the same shape as a missing name.
func TestCreateSourceMissingSecret(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `{"name":"unsigned-source","source_type":"github"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing secret, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] == nil {
		t.Error("expected error message in response")
	}
}

// TestCreateSourceEmptySecret is TestCreateSourceMissingSecret's sibling: an
// explicit empty string is not a secret either. Without this, `"secret":""`
// could slip through a check that only tests for the field's absence.
func TestCreateSourceEmptySecret(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `{"name":"unsigned-source","source_type":"github","secret":""}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty secret, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateSourceInvalidBody(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `not valid json`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid body, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// List events with and without filters
// ===========================================================================

func TestListEventsWithFilters(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create two sources.
	createBody1 := `{"name":"source-1","source_type":"github","secret":"test-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody1)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source 1: expected 201, got %d", rec.Code)
	}
	var src1 map[string]any
	json.Unmarshal(rec.Body.Bytes(), &src1)
	sourceID1 := src1["id"].(string)

	createBody2 := `{"name":"source-2","source_type":"stripe","secret":"test-secret"}`
	req = authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody2)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source 2: expected 201, got %d", rec.Code)
	}
	var src2 map[string]any
	json.Unmarshal(rec.Body.Bytes(), &src2)
	sourceID2 := src2["id"].(string)

	// Ingest an event for source 1. Both sources were created with the same
	// secret ("test-secret"), so one signing helper covers both.
	sign := func(payload string) string {
		mac := hmac.New(sha256.New, []byte("test-secret"))
		mac.Write([]byte(payload))
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	payload1 := `{"event":"push"}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID1, bytes.NewReader([]byte(payload1)))
	req.Header.Set("X-Github-Event", "push")
	req.Header.Set("X-Hub-Signature-256", sign(payload1))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest 1: expected 201, got %d", rec.Code)
	}

	// Ingest an event for source 2.
	payload2 := `{"event":"charge"}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID2, bytes.NewReader([]byte(payload2)))
	req.Header.Set("X-Event-Type", "payment")
	req.Header.Set("X-Hub-Signature-256", sign(payload2))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest 2: expected 201, got %d", rec.Code)
	}

	// List all events.
	req = authedRequest("GET", "/ingest/events", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list events: expected 200, got %d", rec.Code)
	}
	var events []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// List events filtered by source_id.
	req = authedRequest("GET", "/ingest/events?source_id="+sourceID1, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list events by source: expected 200, got %d", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 1 {
		t.Fatalf("expected 1 event for source 1 filter, got %d", len(events))
	}

	// List events filtered by event_type.
	req = authedRequest("GET", "/ingest/events?event_type=push", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list events by type: expected 200, got %d", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 1 {
		t.Fatalf("expected 1 event for event_type filter, got %d", len(events))
	}

	// List events filtered by processed=false.
	req = authedRequest("GET", "/ingest/events?processed=false", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list events by processed: expected 200, got %d", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 2 {
		t.Fatalf("expected 2 unprocessed events, got %d", len(events))
	}

	_ = store // store used implicitly through handler
}

// ===========================================================================
// Ingest with non-JSON body
// ===========================================================================

func TestIngestNonJSONBody(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create source.
	createBody := `{"name":"nonjson-test","source_type":"generic","secret":"test-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}

	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Ingest a non-JSON plain text payload.
	rawText := "plain text webhook body"
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write([]byte(rawText))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(rawText)))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Hub-Signature-256", sig)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the payload was stored as a JSON string.
	store.mu.RLock()
	if len(store.events) != 1 {
		store.mu.RUnlock()
		t.Fatalf("expected 1 event, got %d", len(store.events))
	}
	payload := store.events[0].payload
	store.mu.RUnlock()

	var decodedPayload string
	if err := json.Unmarshal([]byte(payload), &decodedPayload); err != nil {
		t.Fatalf("payload should be a JSON string, got: %s (parse error: %v)", payload, err)
	}
	if decodedPayload != rawText {
		t.Errorf("expected payload %q, got %q", rawText, decodedPayload)
	}
}

// ===========================================================================
// Ingest with invalid source ID (not a UUID)
// ===========================================================================

func TestIngestInvalidSourceID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("POST", "/ingest/not-a-uuid", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid source ID, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Ingest with source not found (valid UUID but doesn't exist)
// ===========================================================================

func TestIngestSourceNotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("POST", "/ingest/"+uuid.New().String(), bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent source, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// List sources handler
// ===========================================================================

func TestListSourcesHandler(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Empty list initially.
	req := authedRequest("GET", "/ingest/sources", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for list sources, got %d: %s", rec.Code, rec.Body.String())
	}
	// Unmarshal into map[string]any: cleat#1992 removed the response's
	// "secret" field entirely -- a source's response now carries
	// secret_configured (bool), not a redacted placeholder to compare.
	var sources []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sources); err != nil {
		t.Fatalf("unmarshal sources: %v", err)
	}
	if len(sources) != 0 {
		t.Errorf("expected empty list, got %d sources", len(sources))
	}

	// Create two sources.
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("source-%d", i)
		body := fmt.Sprintf(`{"name":"%s","secret":"test-secret"}`, name)
		req := authedRequest("POST", "/ingest/sources", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create source %d: expected 201, got %d", i, rec.Code)
		}
	}

	// List should now return 2 sources.
	req = authedRequest("GET", "/ingest/sources", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for list sources, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sources); err != nil {
		t.Fatalf("unmarshal sources: %v", err)
	}
	if len(sources) != 2 {
		t.Errorf("expected 2 sources, got %d", len(sources))
	}

	// Verify they are stored in the fake DB store.
	store.mu.RLock()
	n := len(store.sources)
	store.mu.RUnlock()
	if n != 2 {
		t.Errorf("expected 2 sources in store, got %d", n)
	}
}

func TestListSourcesNoAuth(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("GET", "/ingest/sources", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Get source with no auth
// ===========================================================================

func TestGetSourceNoAuth(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("GET", "/ingest/sources/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Init with nil logger
// ===========================================================================

func TestWH_Init_NilLogger(t *testing.T) {
	p := &Plugin{}
	ctx := context.Background()
	env := &plugin.Environment{
		DB:  &engine.SQLDBAdapter{DB: sql.OpenDB(&fakeConnector{store: newFakeDBStore()})},
		Mux: http.NewServeMux(),
	}
	err := p.Init(ctx, env)
	if err != nil {
		t.Fatalf("Init with nil logger: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set even when nil is provided")
	}
}

// ===========================================================================
// CreateSource no auth
// ===========================================================================

func TestWH_CreateSource_NoTenant(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("POST", "/ingest/sources", bytes.NewReader([]byte(`{"name":"test"}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// CreateSource DB exec error
// ===========================================================================

func TestWH_CreateSource_ExecError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	store.mu.Lock()
	store.failNextExec = true
	store.mu.Unlock()

	body := `{"name":"test-source","source_type":"github","secret":"test-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from exec error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// GetSource invalid ID
// ===========================================================================

func TestWH_GetSource_InvalidID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/ingest/sources/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid UUID, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// GetSource DB query error
// ===========================================================================

func TestWH_GetSource_QueryError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source first so we have a valid ID.
	createBody := `{"name":"test-source","source_type":"github","secret":"test-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Set fail flag with skip=1 for auth middleware query.
	store.mu.Lock()
	store.failNextQuery = true
	store.querySkip = 1
	store.mu.Unlock()

	req = authedRequest("GET", "/ingest/sources/"+sourceID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from query error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// DeleteSource no auth
// ===========================================================================

func TestWH_DeleteSource_NoTenant(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("DELETE", "/ingest/sources/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// DeleteSource invalid ID
// ===========================================================================

func TestWH_DeleteSource_InvalidID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("DELETE", "/ingest/sources/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid UUID, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// DeleteSource not found
// ===========================================================================

func TestWH_DeleteSource_NotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Delete a non-existent source (valid UUID but doesn't exist).
	req := authedRequest("DELETE", "/ingest/sources/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// DeleteSource DB exec error
// ===========================================================================

func TestWH_DeleteSource_ExecError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source first.
	createBody := `{"name":"test-source","secret":"test-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Set exec fail flag.
	store.mu.Lock()
	store.failNextExec = true
	store.mu.Unlock()

	req = authedRequest("DELETE", "/ingest/sources/"+sourceID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from exec error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// ListSources DB query error
// ===========================================================================

func TestWH_ListSources_QueryError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	store.mu.Lock()
	store.failNextQuery = true
	store.querySkip = 1
	store.mu.Unlock()

	req := authedRequest("GET", "/ingest/sources", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from query error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// ListSources tenant isolation
// ===========================================================================

func TestWH_ListSources_TenantIsolation(t *testing.T) {
	store := newFakeDBStore()
	keyHash1 := sha256.Sum256([]byte("key-tenant-1"))
	keyHash2 := sha256.Sum256([]byte("key-tenant-2"))
	tenant1Str := "00000000-0000-0000-0000-000000000001"
	tenant2Str := "00000000-0000-0000-0000-000000000002"
	store.apiKeys[fmt.Sprintf("%x", keyHash1)] = tenant1Str
	store.apiKeys[fmt.Sprintf("%x", keyHash2)] = tenant2Str

	// Add a source for each tenant.
	store.sources = append(store.sources, webhookSourceRow{
		id:       uuid.New().String(),
		tenantID: tenant1Str,
		name:     "tenant-1-source",
	})
	store.sources = append(store.sources, webhookSourceRow{
		id:       uuid.New().String(),
		tenantID: tenant2Str,
		name:     "tenant-2-source",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: plugintest.NewFakeSecrets(),
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	handler := auth.MiddlewareWithMux(engine.NewPostgresStore(db), false, mux)(mux)

	// Tenant 1 lists sources.
	req := httptest.NewRequest("GET", "/ingest/sources", nil)
	req.Header.Set("Authorization", "Bearer key-tenant-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant 1 list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Unmarshal into map[string]any: cleat#1992 removed the response's
	// "secret" field entirely -- a source's response now carries
	// secret_configured (bool), not a redacted placeholder to compare.
	var sources []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sources); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("expected 1 source for tenant 1, got %d", len(sources))
	}
	if sources[0]["name"] != "tenant-1-source" {
		t.Errorf("expected 'tenant-1-source', got %s", sources[0]["name"])
	}

	// Tenant 2 lists sources.
	req = httptest.NewRequest("GET", "/ingest/sources", nil)
	req.Header.Set("Authorization", "Bearer key-tenant-2")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant 2 list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sources); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("expected 1 source for tenant 2, got %d", len(sources))
	}
	if sources[0]["name"] != "tenant-2-source" {
		t.Errorf("expected 'tenant-2-source', got %s", sources[0]["name"])
	}
}

// ===========================================================================
// ListEvents no auth
// ===========================================================================

func TestWH_ListEvents_NoTenant(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("GET", "/ingest/events", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// ListEvents DB query error
// ===========================================================================

func TestWH_ListEvents_QueryError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	store.mu.Lock()
	store.failNextQuery = true
	store.querySkip = 1
	store.mu.Unlock()

	req := authedRequest("GET", "/ingest/events", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from query error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Ingest source lookup DB error
// ===========================================================================

func TestWH_Ingest_SourceLookupError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source so we have a valid source ID to use.
	createBody := `{"name":"test-source","secret":"test-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Set flag to fail the next query (ingest endpoint has no auth middleware query).
	store.mu.Lock()
	store.failNextQuery = true
	store.mu.Unlock()

	// Ingest request should fail during source lookup.
	payload := `{"action":"opened"}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from source lookup error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Ingest event insert DB error
// ===========================================================================

func TestWH_Ingest_EventInsertError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source so we have a valid source ID to use.
	createBody := `{"name":"test-source","secret":"test-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Set flag to fail the next exec (event insert after source lookup).
	store.mu.Lock()
	store.failNextExec = true
	store.mu.Unlock()

	// Ingest request should fail during event insert -- signed, so the
	// failure under test is the insert, not the signature check.
	payload := `{"action":"opened"}`
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write([]byte(payload))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	req.Header.Set("X-Hub-Signature-256", sig)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from event insert error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// AwaitWebhook no tenant context
// ===========================================================================

func TestWH_AwaitWebhook_NoTenant(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: plugintest.NewFakeSecrets(),
	}

	// Call awaitWebhook with a context that has no CallContext.
	_, err := p.awaitWebhook(context.Background(), AwaitWebhookInput{SourceID: uuid.New().String()})
	if err == nil {
		t.Fatal("expected error for missing tenant context, got nil")
	}
	if !strings.Contains(err.Error(), "no tenant context") {
		t.Errorf("expected 'no tenant context' error, got: %v", err)
	}
}

// ===========================================================================
// AwaitWebhook empty source ID -- cleat#2649, BREAKING (see CHANGELOG.md)
// ===========================================================================

// TestWH_AwaitWebhook_EmptySourceIDIsRequired asserts the exact message a
// caller relying on the pre-cleat#2649 "empty source_id means any source"
// behaviour now gets, per cleat-review's ask on that PR: state the message,
// not just that an error occurs -- the #2665/#2668 vacuity lesson (a
// `-run` pattern or an error check that matches everything proves nothing
// about which case fired). A caller upgrading a long-running workflow needs
// this exact string to recognise the failure as this breaking change rather
// than a generic bug.
func TestWH_AwaitWebhook_EmptySourceIDIsRequired(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: plugintest.NewFakeSecrets(),
	}

	callCtx := &plugin.CallContext{TenantID: testTenantID.String(), WorkflowID: "test-wf"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	// No EventType or Keys either -- SourceID alone is what must trigger
	// this, before the call reaches any query (this fake's QueryContext has
	// no case for ingested_events, so a query would fail loudly with
	// "unexpected Query" if this check did not short-circuit first).
	_, err := p.awaitWebhook(ctx, AwaitWebhookInput{})
	if err == nil {
		t.Fatal("expected error for empty source_id, got nil")
	}
	const want = "webhook-ingest: source_id is required for correlated await"
	if err.Error() != want {
		t.Errorf("error message: got %q, want %q", err.Error(), want)
	}
}

// ===========================================================================
// AwaitWebhook invalid source ID
// ===========================================================================

func TestWH_AwaitWebhook_InvalidSourceID(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: plugintest.NewFakeSecrets(),
	}

	callCtx := &plugin.CallContext{TenantID: testTenantID.String(), WorkflowID: "test-wf"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	// Call with invalid source_id.
	_, err := p.awaitWebhook(ctx, AwaitWebhookInput{SourceID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected error for invalid source_id, got nil")
	}
	if !strings.Contains(err.Error(), "invalid source_id") {
		t.Errorf("expected 'invalid source_id' error, got: %v", err)
	}
}

// ===========================================================================
// AwaitWebhook invalid input JSON
// ===========================================================================

// TestWH_AwaitWebhook_InvalidJSON exercises the real call site a workflow
// reaches -- the registered PluginFunc, which does its own JSON
// unmarshaling via plugin.RegisterTyped -- rather than p.awaitWebhook
// directly. awaitWebhook itself (cleat#2626) no longer parses JSON; that
// moved into RegisterTyped's wrapper, which is tested on its own merits in
// plugin/typed_test.go. This test is what's left to prove: that
// webhookingest's own registration still rejects malformed input
// end-to-end.
func TestWH_AwaitWebhook_InvalidJSON(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: plugintest.NewFakeSecrets(),
	}

	reg := newFakeFuncRegistry()
	if err := p.RegisterHostFunctions(reg); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}

	callCtx := &plugin.CallContext{TenantID: testTenantID.String(), WorkflowID: "test-wf"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	// Call with invalid JSON input.
	_, err := reg.Get("await_webhook")(ctx, `not valid json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("expected 'invalid input' error, got: %v", err)
	}
}

// ===========================================================================
// RegisterHostFunctions register error
// ===========================================================================

type errFuncRegistry struct{}

func (r *errFuncRegistry) Register(_ plugin.FuncOptions, _ plugin.PluginFunc) error {
	return fmt.Errorf("simulated register error")
}

func TestWH_RegisterHostFunctions_RegisterError(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	err := p.RegisterHostFunctions(&errFuncRegistry{})
	if err == nil {
		t.Fatal("expected error from Register, got nil")
	}
	if !strings.Contains(err.Error(), "simulated register error") {
		t.Errorf("expected 'simulated register error', got: %v", err)
	}
}

// ===========================================================================
// Ingest body read error
// ===========================================================================

func TestWH_Ingest_BodyReadError(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:         sourceID.String(),
		tenantID:   testTenantStr,
		name:       "test",
		sourceType: "generic",
		enabled:    true,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: plugintest.NewFakeSecrets(),
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	handler := auth.MiddlewareWithMux(engine.NewPostgresStore(db), false, mux)(mux)

	// Send request with a body that fails on Read.
	req := httptest.NewRequest("POST", "/ingest/"+sourceID.String(), &errReadCloser{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// cleat#2232: plugin.ReadBody, not a hand-rolled ReadAll, now serves this
	// route -- a generic read failure (as opposed to an oversized body) is a
	// 400, matching cmd/cleat-worker's own decodeBody/readBody precedent.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for body read error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to read request body") {
		t.Errorf("expected a 'failed to read request body' error, got: %s", rec.Body.String())
	}
}

// ===========================================================================
// CreateSource body read error
// ===========================================================================

func TestWH_CreateSource_BodyReadError(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Send request with a body that fails on Read.
	req := authedRequest("POST", "/ingest/sources", &errReadCloser{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// cleat#2232: plugin.ReadJSONBody, not a hand-rolled ReadAll, now serves
	// this route -- a generic read failure is a 400, matching
	// cmd/cleat-worker's own decodeBody/readBody precedent.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for body read error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to read request body") {
		t.Errorf("expected a 'failed to read request body' error, got: %s", rec.Body.String())
	}
}

// ===========================================================================
// AwaitWebhook query DB error
// ===========================================================================

func TestWH_AwaitWebhook_QueryError(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	// awaitWebhook's FIRST query is now the correlation_key_field guard's
	// own source lookup (cleat-review on #2697) -- a real row must exist
	// for it to reach ClaimOrRegisterAwaiter's claim SELECT at all, which is
	// what this test actually wants to exercise a failure in.
	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:       sourceID.String(),
		tenantID: testTenantStr,
		name:     "query-error-source",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	// Set fail flag for the SECOND query, skipping the guard's own lookup
	// (querySkip=1) -- awaitWebhook's second query is
	// ClaimOrRegisterAwaiter's claim SELECT against ingested_events
	// (cleat#2649), reached through db.Begin()+tx.QueryRow(), not a direct
	// query on this Plugin's own db. This still exercises the same
	// property (a query failure propagates as an error mentioning
	// "query events"): ClaimOrRegisterAwaiter wraps it as
	// "event-triggers: query events: %w".
	store.mu.Lock()
	store.failNextQuery = true
	store.querySkip = 1
	store.mu.Unlock()

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: plugintest.NewFakeSecrets(),
	}

	callCtx := &plugin.CallContext{TenantID: testTenantID.String(), WorkflowID: "test-wf"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	_, err := p.awaitWebhook(ctx, AwaitWebhookInput{SourceID: sourceID.String()})
	if err == nil {
		t.Fatal("expected error from query failure, got nil")
	}
	if !strings.Contains(err.Error(), "query events") {
		t.Errorf("expected error mentioning 'query events', got: %v", err)
	}
}

// TestWH_AwaitWebhook_SourceLookupQueryError is cleat-review's minor note on
// #2697: the correlation_key_field guard's OWN lookup query error path
// (distinct from sql.ErrNoRows, which is deliberately not an error -- see
// awaitWebhook's guard comment and TestADeletedSourcesPendingEventIsCancelledNotDelivered)
// had no test. This is the FIRST query awaitWebhook runs, so no querySkip is
// needed and no source needs seeding -- failNextQuery fires before the fake
// even looks at what query it is.
func TestWH_AwaitWebhook_SourceLookupQueryError(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	store.mu.Lock()
	store.failNextQuery = true
	store.mu.Unlock()

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: plugintest.NewFakeSecrets(),
	}

	callCtx := &plugin.CallContext{TenantID: testTenantID.String(), WorkflowID: "test-wf"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	_, err := p.awaitWebhook(ctx, AwaitWebhookInput{SourceID: uuid.New().String()})
	if err == nil {
		t.Fatal("expected error from source lookup query failure, got nil")
	}
	const want = "webhook-ingest: look up source:"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error: got %q, want it to contain %q", err.Error(), want)
	}
}

// ===========================================================================
// AwaitWebhook exec error (mark processed)
// ===========================================================================

// TestWH_AwaitWebhook_ExecError used to pin that a failed "mark processed"
// UPDATE was tolerated -- awaitWebhook still returned the found event. That
// was never a deliberately reviewed property, just the old poll's
// best-effort default (a logged warning, nothing more). cleat#2649 moved
// awaitWebhook onto eventtriggers.ClaimOrRegisterAwaiter, which claims and
// marks an event consumed in ONE transaction and fails loudly if any part
// of it errors (cleat#2654's own reasoning: a claim that cannot be reported
// back must roll back rather than report success over a write that may not
// have landed). Also, this test's call had no SourceID, which now errors
// before reaching any query at all. Superseded, not replaced --
// ClaimOrRegisterAwaiter's transactional failure handling is exercised by
// eventtriggers' own tests (e.g. TestClaimOrRegisterAwaiterSkippingBeforeCommitLosesCleat2654IsRealFinding).

// ===========================================================================
// ListEvents empty list (nil to empty slice)
// ===========================================================================

func TestWH_ListEvents_EmptyList(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// List events with no events in the store — should return [] not null.
	req := authedRequest("GET", "/ingest/events", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var events []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("unmarshal events: %v", err)
	}
	if events == nil {
		t.Error("expected non-nil empty slice, got null")
	}
}

// ===========================================================================
// ListSources scan error (corrupt data)
// ===========================================================================

func TestWH_ListSources_ScanError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source so there's data to scan.
	createBody := `{"name":"test-source","secret":"test-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}

	// Set corrupt flag to cause scan error on the next list query.
	store.mu.Lock()
	store.corruptNextScan = true
	store.corruptScanSkip = 1 // skip the auth middleware query (tenant lookup)
	store.mu.Unlock()

	req = authedRequest("GET", "/ingest/sources", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// Should still return 200 — corrupt row is skipped and logged.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite scan error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// ListEvents scan error (corrupt data)
// ===========================================================================

func TestWH_ListEvents_ScanError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source to accept webhook events.
	createBody := `{"name":"test-source","source_type":"github","secret":"test-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Ingest a webhook payload to create an event, signed with the source's secret.
	payload := `{"event":"test"}`
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write([]byte(payload))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	req.Header.Set("X-Hub-Signature-256", sig)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest: expected 201, got %d", rec.Code)
	}

	// Set corrupt flag for the events list query.
	store.mu.Lock()
	store.corruptNextScan = true
	store.corruptScanSkip = 1 // skip auth middleware query
	store.mu.Unlock()

	req = authedRequest("GET", "/ingest/events", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// Should still return 200 — corrupt row is skipped and logged.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite scan error, got %d: %s", rec.Code, rec.Body.String())
	}
}
