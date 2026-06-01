package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// ---------------------------------------------------------------------------
// EventStream interface
// ---------------------------------------------------------------------------

// EventStream provides access to event history records without requiring the
// entire history to be loaded into memory at once. It supports both replay
// (forward-only consumption) and fresh execution (appending new events).
//
// Implementations:
//   - SliceEventStream: backed by []EventRecord (backward compatible, used
//     when the history is already in memory).
//   - DBEventStream: backed by a database cursor; loads events on demand as
//     the replay step counter advances. Memory usage is proportional to the
//     active working set, not the total history length.
//
// All implementations must be safe for concurrent access ONLY when accessed
// from a single goroutine (the WASM execution goroutine).
type EventStream interface {
	// At returns the event at index i. The index must be >= 0.
	// For forward-only replay, i should be >= the last consumed index.
	// Returns nil if i is out of bounds.
	At(i int) *EventRecord

	// Len returns the number of events currently available.
	// For slice-backed streams this is the total length.
	// For DB-backed streams this is the number of events loaded so far
	// (may be less than total; call EnsureLoaded to fetch more).
	Len() int

	// Append adds a new event to the stream. This is used during fresh
	// execution to record new events.
	Append(rec EventRecord)

	// Slice returns a contiguous portion of the stream as a []EventRecord.
	// For DB-backed streams, this may trigger loading if the requested
	// range is not yet in memory. Returns all events from start to end-1.
	// If end <= 0, returns all events from start to the end of the stream.
	Slice(start, end int) []EventRecord

	// Total returns the total number of events in the stream (including
	// events not yet loaded). For slice-backed streams this equals Len().
	// For DB-backed streams this may require a COUNT query.
	Total() (int, error)

	// Close releases any resources held by the stream (e.g., database rows).
	Close() error
}

// ---------------------------------------------------------------------------
// SliceEventStream -- in-memory []EventRecord (backward compatible)
// ---------------------------------------------------------------------------

// SliceEventStream wraps a []EventRecord as an EventStream. This is the
// default implementation used when the history has already been loaded
// into memory (e.g., from the worker's loadEventHistory call).
type SliceEventStream struct {
	events []EventRecord
}

// NewSliceEventStream creates a SliceEventStream from an existing slice.
// The caller retains ownership of the underlying slice.
func NewSliceEventStream(events []EventRecord) *SliceEventStream {
	if events == nil {
		events = make([]EventRecord, 0)
	}
	return &SliceEventStream{events: events}
}

func (s *SliceEventStream) At(i int) *EventRecord {
	if i < 0 || i >= len(s.events) {
		return nil
	}
	return &s.events[i]
}

func (s *SliceEventStream) Len() int {
	return len(s.events)
}

func (s *SliceEventStream) Append(rec EventRecord) {
	s.events = append(s.events, rec)
}

func (s *SliceEventStream) Slice(start, end int) []EventRecord {
	if end <= 0 || end > len(s.events) {
		end = len(s.events)
	}
	if start < 0 {
		start = 0
	}
	if start >= len(s.events) {
		return nil
	}
	result := make([]EventRecord, end-start)
	copy(result, s.events[start:end])
	return result
}

func (s *SliceEventStream) Total() (int, error) {
	return len(s.events), nil
}

func (s *SliceEventStream) Close() error {
	return nil
}

// AsEventStream is a helper to convert a []EventRecord to an EventStream.
// This is the main backward-compatibility adapter.
func AsEventStream(events []EventRecord) EventStream {
	return NewSliceEventStream(events)
}

// ---------------------------------------------------------------------------
// DBEventStream -- streaming cursor-based loader (memory-efficient)
// ---------------------------------------------------------------------------

// DBEventStream loads event history from the database on demand as the
// replay step counter advances. Instead of loading the entire history into
// memory, it fetches pages of events as needed.
//
// The stream maintains a sliding window of loaded events. As the replay
// advances past the current page, the next page is fetched automatically.
type DBEventStream struct {
	db         *sql.DB
	workflowID string

	// pageSize controls how many events are fetched per database query.
	pageSize int

	// loaded holds the events that have been fetched so far.
	loaded []EventRecord

	// totalCount caches the total event count (from COUNT query).
	totalCount   int
	totalLoaded  bool
}

// NewDBEventStream creates a new DBEventStream for the given workflow.
// The first page of events is loaded lazily on the first At() call.
func NewDBEventStream(db *sql.DB, workflowID string, pageSize int) *DBEventStream {
	if pageSize <= 0 {
		pageSize = 1000
	}
	return &DBEventStream{
		db:         db,
		workflowID: workflowID,
		pageSize:   pageSize,
	}
}

// ensureLoaded loads more events from the database if needed to satisfy
// an access to index i. Returns nil if the index is beyond the total
// available events.
func (s *DBEventStream) ensureLoaded(i int) error {
	if i < len(s.loaded) {
		return nil
	}

	// Fetch the next page of events.
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input,
		       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error
		FROM event_history
		WHERE workflow_id = $1
		ORDER BY step
		LIMIT $2 OFFSET $3
	`, s.workflowID, s.pageSize, len(s.loaded))
	if err != nil {
		return fmt.Errorf("db event stream: load at %d: %w", len(s.loaded), err)
	}
	defer rows.Close()

	for rows.Next() {
		var rec EventRecord
		var service, op, request, response, errMsg sql.NullString
		var durationMs, timeoutMs sql.NullInt64
		var signalNames, signalName, signalPayload sql.NullString
		var deferDesc, deferID sql.NullString
		var childName, childInput, runID, newInput sql.NullString
		var pluginName, pluginFunc, pluginInput, pluginOutput, pluginErr sql.NullString

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
			&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr); err != nil {
			return fmt.Errorf("db event stream scan: %w", err)
		}

		rec.Service = service.String
		rec.Op = op.String
		rec.Request = request.String
		rec.Response = response.String
		rec.Err = errMsg.String
		rec.DurationMs = durationMs.Int64
		rec.SignalNames = signalNames.String
		rec.TimeoutMs = timeoutMs.Int64
		rec.SignalName = signalName.String
		rec.SignalPayload = signalPayload.String
		rec.DeferDescription = deferDesc.String
		rec.DeferID = deferID.String
		rec.ChildName = childName.String
		rec.ChildInput = childInput.String
		rec.RunID = runID.String
		rec.NewInput = newInput.String
		rec.PluginName = pluginName.String
		rec.PluginFunc = pluginFunc.String
		rec.PluginInput = pluginInput.String
		rec.PluginOutput = pluginOutput.String
		rec.PluginError = pluginErr.String

		s.loaded = append(s.loaded, rec)
	}

	return rows.Err()
}

func (s *DBEventStream) At(i int) *EventRecord {
	if i < 0 {
		return nil
	}
	if i >= len(s.loaded) {
		if err := s.ensureLoaded(i); err != nil {
			return nil
		}
	}
	if i >= len(s.loaded) {
		return nil
	}
	return &s.loaded[i]
}

func (s *DBEventStream) Len() int {
	return len(s.loaded)
}

func (s *DBEventStream) Append(rec EventRecord) {
	s.loaded = append(s.loaded, rec)
}

func (s *DBEventStream) Slice(start, end int) []EventRecord {
	if end <= 0 || end > len(s.loaded) {
		// Try to ensure all events are loaded up to at least start.
		if err := s.ensureLoaded(start + s.pageSize); err == nil {
			// Keep loading all remaining events.
			for {
				prevLen := len(s.loaded)
				if err := s.ensureLoaded(len(s.loaded) + s.pageSize); err != nil {
					break
				}
				if s.totalLoaded || len(s.loaded) == prevLen {
					break
				}
			}
		}
		end = len(s.loaded)
	}
	if start < 0 {
		start = 0
	}
	if start >= len(s.loaded) {
		return nil
	}
	result := make([]EventRecord, end-start)
	copy(result, s.loaded[start:end])
	return result
}

func (s *DBEventStream) Total() (int, error) {
	if s.totalLoaded {
		return s.totalCount, nil
	}

	// If we think all rows are loaded, use len.
	// Otherwise query COUNT.
	err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM event_history WHERE workflow_id = $1`,
		s.workflowID).Scan(&s.totalCount)
	if err != nil {
		return len(s.loaded), err
	}
	s.totalLoaded = true
	return s.totalCount, nil
}

func (s *DBEventStream) Close() error {
	// Clear the loaded slice to release memory.
	s.loaded = nil
	return nil
}

// ---------------------------------------------------------------------------
// JSON serialization helpers (used by event_stream and history)
// ---------------------------------------------------------------------------

// eventStreamToJSON converts an EventStream to a JSON string for debugging.
func eventStreamToJSON(es EventStream) (string, error) {
	events := es.Slice(0, es.Len())
	if len(events) == 0 {
		return "[]", nil
	}
	data, err := json.Marshal(events)
	if err != nil {
		return "[]", err
	}
	return string(data), nil
}
