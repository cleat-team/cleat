package engine

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// SliceEventStream tests
// ---------------------------------------------------------------------------

func TestSliceEventStream_NilInput(t *testing.T) {
	s := NewSliceEventStream(nil)
	if s == nil {
		t.Fatal("NewSliceEventStream(nil) returned nil")
	}
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0", s.Len())
	}
	if s.At(0) != nil {
		t.Errorf("At(0) = %v, want nil", s.At(0))
	}
}

func TestSliceEventStream_At(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "a"},
		{Step: 1, EventType: EventTypeCall, Service: "b"},
		{Step: 2, EventType: EventTypeCall, Service: "c"},
	}
	s := NewSliceEventStream(events)

	if got := s.At(0); got == nil || got.Service != "a" {
		t.Errorf("At(0) = %v", got)
	}
	if got := s.At(2); got == nil || got.Service != "c" {
		t.Errorf("At(2) = %v", got)
	}
	if got := s.At(-1); got != nil {
		t.Errorf("At(-1) = %v, want nil", got)
	}
	if got := s.At(3); got != nil {
		t.Errorf("At(3) = %v, want nil", got)
	}
}

func TestSliceEventStream_Len(t *testing.T) {
	events := []EventRecord{{Step: 0}, {Step: 1}, {Step: 2}}
	s := NewSliceEventStream(events)
	if s.Len() != 3 {
		t.Errorf("Len() = %d, want 3", s.Len())
	}
	s.Append(EventRecord{Step: 3})
	if s.Len() != 4 {
		t.Errorf("Len() after Append = %d, want 4", s.Len())
	}
}

func TestSliceEventStream_Append(t *testing.T) {
	s := NewSliceEventStream(nil)
	s.Append(EventRecord{Step: 0, EventType: EventTypeCall, Service: "first"})
	s.Append(EventRecord{Step: 1, EventType: EventTypeCall, Service: "second"})

	if s.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", s.Len())
	}
	if got := s.At(0); got == nil || got.Service != "first" {
		t.Errorf("At(0) = %v", got)
	}
	if got := s.At(1); got == nil || got.Service != "second" {
		t.Errorf("At(1) = %v", got)
	}
}

func TestSliceEventStream_Slice_Basic(t *testing.T) {
	events := []EventRecord{
		{Step: 0, Service: "a"},
		{Step: 1, Service: "b"},
		{Step: 2, Service: "c"},
		{Step: 3, Service: "d"},
	}
	s := NewSliceEventStream(events)
	got := s.Slice(0, 2)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Service != "a" || got[1].Service != "b" {
		t.Errorf("Slice(0,2) = %+v", got)
	}
}

func TestSliceEventStream_Slice_ToEnd(t *testing.T) {
	events := []EventRecord{
		{Step: 0, Service: "a"},
		{Step: 1, Service: "b"},
		{Step: 2, Service: "c"},
	}
	s := NewSliceEventStream(events)
	got := s.Slice(1, 0) // end=0 means to end
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Service != "b" || got[1].Service != "c" {
		t.Errorf("Slice(1,0) = %+v", got)
	}
}

func TestSliceEventStream_Slice_NegativeStart(t *testing.T) {
	events := []EventRecord{
		{Step: 0, Service: "a"},
		{Step: 1, Service: "b"},
	}
	s := NewSliceEventStream(events)
	got := s.Slice(-1, 2) // negative start clamps to 0
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Service != "a" || got[1].Service != "b" {
		t.Errorf("Slice(-1,2) = %+v", got)
	}
}

func TestSliceEventStream_Slice_StartEQEnd(t *testing.T) {
	events := []EventRecord{
		{Step: 0, Service: "a"},
		{Step: 1, Service: "b"},
	}
	s := NewSliceEventStream(events)
	got := s.Slice(1, 1)
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestSliceEventStream_Slice_StartGELen(t *testing.T) {
	s := NewSliceEventStream([]EventRecord{{Step: 0}})
	got := s.Slice(1, 2) // start >= len → nil
	if got != nil {
		t.Errorf("Slice(1,2) = %v, want nil", got)
	}
}

func TestSliceEventStream_Slice_EmptyStream(t *testing.T) {
	s := NewSliceEventStream(nil)
	got := s.Slice(0, 0)
	if got != nil {
		t.Errorf("Slice(0,0) on empty stream = %v, want nil", got)
	}
}

func TestSliceEventStream_Slice_Copy(t *testing.T) {
	events := []EventRecord{
		{Step: 0, Service: "original"},
	}
	s := NewSliceEventStream(events)
	got := s.Slice(0, 1)
	// Mutate the copy
	got[0].Service = "modified"
	// Original should be unchanged
	if s.At(0).Service != "original" {
		t.Errorf("original was mutated: Service = %q", s.At(0).Service)
	}
}

func TestSliceEventStream_Total(t *testing.T) {
	s := NewSliceEventStream([]EventRecord{{Step: 0}, {Step: 1}})
	total, err := s.Total()
	if err != nil {
		t.Errorf("Total() error = %v, want nil", err)
	}
	if total != 2 {
		t.Errorf("Total() = %d, want 2", total)
	}
	// After append
	s.Append(EventRecord{Step: 2})
	total, _ = s.Total()
	if total != 3 {
		t.Errorf("Total() after append = %d, want 3", total)
	}
}

func TestSliceEventStream_Close(t *testing.T) {
	s := NewSliceEventStream([]EventRecord{{Step: 0}})
	if err := s.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func TestAsEventStream(t *testing.T) {
	events := []EventRecord{{Step: 0}, {Step: 1}}
	es := AsEventStream(events)
	if es.Len() != 2 {
		t.Errorf("Len() = %d, want 2", es.Len())
	}
	if es.At(0) == nil || es.At(0).Step != 0 {
		t.Errorf("At(0) = %v", es.At(0))
	}
}

func TestSliceEventStream_At_Empty(t *testing.T) {
	s := NewSliceEventStream(nil)
	if got := s.At(0); got != nil {
		t.Errorf("At(0) on empty stream = %v, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// DBEventStream tests (no database required)
// ---------------------------------------------------------------------------

func TestNewDBEventStream_DefaultPageSize(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 0)
	if s == nil {
		t.Fatal("NewDBEventStream returned nil")
	}
	if s.pageSize != 1000 {
		t.Errorf("pageSize = %d, want 1000", s.pageSize)
	}
}

func TestNewDBEventStream_NegativePageSize(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", -1)
	if s.pageSize != 1000 {
		t.Errorf("pageSize = %d, want 1000", s.pageSize)
	}
}

func TestNewDBEventStream_CustomPageSize(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 500)
	if s.pageSize != 500 {
		t.Errorf("pageSize = %d, want 500", s.pageSize)
	}
}

func TestDBEventStream_Len_Empty(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 100)
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0", s.Len())
	}
}

func TestDBEventStream_Append(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 100)
	s.Append(EventRecord{Step: 0, Service: "s"})
	if s.Len() != 1 {
		t.Errorf("Len() = %d, want 1", s.Len())
	}
}

func TestDBEventStream_At_Negative(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 100)
	if got := s.At(-1); got != nil {
		t.Errorf("At(-1) = %v, want nil", got)
	}
}

func TestDBEventStream_Close(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 100)
	s.Append(EventRecord{Step: 0})
	if err := s.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len() after Close = %d, want 0", s.Len())
	}
}

// ---------------------------------------------------------------------------
// eventStreamToJSON tests
// ---------------------------------------------------------------------------

func TestEventStreamToJSON_Empty(t *testing.T) {
	s := NewSliceEventStream(nil)
	got, err := eventStreamToJSON(s)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got != "[]" {
		t.Errorf("eventStreamToJSON = %q, want %q", got, "[]")
	}
}

func TestEventStreamToJSON_NonEmpty(t *testing.T) {
	s := NewSliceEventStream([]EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "s", Op: "o"},
	})
	got, err := eventStreamToJSON(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed []EventRecord
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, got)
	}
	if len(parsed) != 1 {
		t.Fatalf("len = %d, want 1", len(parsed))
	}
	if parsed[0].Step != 0 || parsed[0].Service != "s" || parsed[0].Op != "o" {
		t.Errorf("parsed record mismatch: %+v", parsed[0])
	}
}

// Compile-time check: both implementations satisfy EventStream.
var _ EventStream = (*DBEventStream)(nil)
var _ EventStream = (*SliceEventStream)(nil)

// ---------------------------------------------------------------------------
// Additional DBEventStream edge-case tests
// ---------------------------------------------------------------------------

func TestDBEventStream_At_Loaded(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 100)
	s.Append(EventRecord{Step: 0, Service: "loaded-test"})

	got := s.At(0)
	if got == nil {
		t.Fatal("At(0) after Append returned nil")
	}
	if got.Service != "loaded-test" {
		t.Errorf("At(0).Service = %q, want %q", got.Service, "loaded-test")
	}
	if got.Step != 0 {
		t.Errorf("At(0).Step = %d, want 0", got.Step)
	}
}

func TestDBEventStream_Slice_Basic(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 100)
	s.Append(EventRecord{Step: 0, Service: "a"})
	s.Append(EventRecord{Step: 1, Service: "b"})
	s.Append(EventRecord{Step: 2, Service: "c"})

	got := s.Slice(0, 2)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Service != "a" || got[1].Service != "b" {
		t.Errorf("Slice(0,2) = %+v", got)
	}
}

func TestDBEventStream_Slice_NegativeStart(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 100)
	s.Append(EventRecord{Step: 0, Service: "x"})
	s.Append(EventRecord{Step: 1, Service: "y"})

	got := s.Slice(-1, 2)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Service != "x" || got[1].Service != "y" {
		t.Errorf("Slice(-1,2) = %+v", got)
	}
}

func TestDBEventStream_Slice_StartGELen(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 100)
	s.Append(EventRecord{Step: 0})

	// With 1 item loaded, Slice(1, 1): end=1 <= len=1, no DB access.
	got := s.Slice(1, 1)
	if got != nil {
		t.Errorf("Slice(1,1) = %v, want nil", got)
	}
}

func TestDBEventStream_Slice_Copy(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 100)
	s.Append(EventRecord{Step: 0, Service: "original"})

	got := s.Slice(0, 1)
	got[0].Service = "modified"

	if s.At(0).Service != "original" {
		t.Errorf("original was mutated: Service = %q", s.At(0).Service)
	}
}

func TestDBEventStream_Slice_StartEQEnd(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 100)
	s.Append(EventRecord{Step: 0, Service: "a"})
	s.Append(EventRecord{Step: 1, Service: "b"})

	got := s.Slice(1, 1)
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestDBEventStream_Slice_EndBeyondLoaded(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM event_history", data: nil},
	}, nil)
	defer db.Close()

	s := NewDBEventStream(db, "wf-1", 100)
	s.Append(EventRecord{Step: 0, Service: "a"})

	// end > loaded: ensureLoaded hits the DB, which returns no additional rows.
	got := s.Slice(0, 10)
	if len(got) != 1 {
		t.Errorf("len = %d, want 1", len(got))
	}
	if got[0].Service != "a" {
		t.Errorf("expected Service %q, got %q", "a", got[0].Service)
	}
}

func TestDBEventStream_At_OutOfBounds(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM event_history", data: nil},
	}, nil)
	defer db.Close()

	s := NewDBEventStream(db, "wf-1", 100)
	s.Append(EventRecord{Step: 0})

	// At(5) is beyond loaded events; ensureLoaded hits the DB, which returns
	// no additional rows, so At should return nil.
	got := s.At(5)
	if got != nil {
		t.Errorf("At(5) = %v, want nil (beyond loaded, no more in DB)", got)
	}
}

func TestSliceEventStream_SliceEndZeroWithEmptyStream(t *testing.T) {
	s := NewSliceEventStream(nil)
	got := s.Slice(0, 0)
	if got != nil {
		t.Errorf("Slice(0,0) on empty stream = %v, want nil", got)
	}
}

func TestSliceEventStream_SliceEndNegative(t *testing.T) {
	s := NewSliceEventStream([]EventRecord{
		{Step: 0, Service: "a"},
		{Step: 1, Service: "b"},
		{Step: 2, Service: "c"},
	})
	got := s.Slice(1, -1) // end < 0 → clamped to len
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
	if got[0].Service != "b" {
		t.Errorf("first element Service = %q, want %q", got[0].Service, "b")
	}
}

func TestSliceEventStream_At_NegativeIndex(t *testing.T) {
	s := NewSliceEventStream([]EventRecord{{Step: 0}})
	if got := s.At(-1); got != nil {
		t.Errorf("At(-1) = %v, want nil", got)
	}
}

func TestSliceEventStream_Slice_BoundsCheck(t *testing.T) {
	events := []EventRecord{
		{Step: 0, Service: "a"},
		{Step: 1, Service: "b"},
	}
	s := NewSliceEventStream(events)
	got := s.Slice(-5, 3) // negative start clamped to 0, end > len clamped to len
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

func TestSliceEventStream_AppendNilStream(t *testing.T) {
	s := NewSliceEventStream(nil)
	s.Append(EventRecord{Step: 0, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "f"})
	if s.Len() != 1 {
		t.Errorf("Len() = %d, want 1", s.Len())
	}
	if got := s.At(0); got == nil || got.PluginName != "p" {
		t.Errorf("At(0).PluginName = %q, want %q", s.At(0).PluginName, "p")
	}
}

// ---------------------------------------------------------------------------
// DBEventStream ensureLoaded tests
// ---------------------------------------------------------------------------

func TestDBEventStream_EnsureLoaded_DBQueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM event_history", err: errors.New("db conn error")},
	}, nil)
	defer db.Close()

	s := NewDBEventStream(db, "wf-1", 100)
	err := s.ensureLoaded(0)
	if err == nil {
		t.Fatal("ensureLoaded(0) expected error, got nil")
	}
	if !strings.Contains(err.Error(), "db conn error") {
		t.Errorf("error = %v, want substring 'db conn error'", err)
	}
}

func TestDBEventStream_EnsureLoaded_ScanError(t *testing.T) {
	// Row with 1 column instead of 23 triggers a column-count mismatch in Scan.
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM event_history", data: [][]driver.Value{{int64(0)}}},
	}, nil)
	defer db.Close()

	s := NewDBEventStream(db, "wf-1", 100)
	err := s.ensureLoaded(0)
	if err == nil {
		t.Fatal("ensureLoaded(0) expected scan error, got nil")
	}
	if !strings.Contains(err.Error(), "scan") {
		t.Errorf("error = %v, want substring 'scan'", err)
	}
}

func TestDBEventStream_EnsureLoaded_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM event_history", data: [][]driver.Value{
			{
				int64(0),               // Step
				string(EventTypeCall),  // EventType
				string("svc1"),         // service
				nil,                    // op
				nil,                    // request
				nil,                    // response
				nil,                    // error
				nil,                    // duration_ms
				nil,                    // signal_names
				nil,                    // timeout_ms
				nil,                    // signal_name
				nil,                    // signal_payload
				nil,                    // defer_description
				nil,                    // defer_id
				nil,                    // child_name
				nil,                    // child_input
				nil,                    // run_id
				nil,                    // new_input
				nil,                    // plugin_name
				nil,                    // plugin_func
				nil,                    // plugin_input
				nil,                    // plugin_output
				nil,                    // plugin_error
			},
			{
				int64(1),
				string(EventTypeCall),
				string("svc2"),
				nil, nil, nil, nil,
				nil,
				nil,
				nil,
				nil, nil,
				nil, nil,
				nil, nil, nil, nil,
				nil, nil, nil, nil, nil,
			},
		}},
	}, nil)
	defer db.Close()

	s := NewDBEventStream(db, "wf-1", 100)
	err := s.ensureLoaded(0)
	if err != nil {
		t.Fatalf("ensureLoaded(0) error = %v, want nil", err)
	}
	if s.Len() != 2 {
		t.Errorf("Len() = %d, want 2", s.Len())
	}
	if got := s.At(0); got == nil || got.Service != "svc1" {
		t.Errorf("At(0).Service = %q, want %q", s.At(0).Service, "svc1")
	}
	if got := s.At(1); got == nil || got.Service != "svc2" {
		t.Errorf("At(1).Service = %q, want %q", s.At(1).Service, "svc2")
	}
}

func TestDBEventStream_EnsureLoaded_AlreadyLoaded(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM event_history", err: errors.New("should not be called")},
	}, nil)
	defer db.Close()

	s := NewDBEventStream(db, "wf-1", 100)
	s.Append(EventRecord{Step: 0, Service: "preloaded"})

	// Index 0 is already loaded; ensureLoaded should return nil without a DB call.
	err := s.ensureLoaded(0)
	if err != nil {
		t.Errorf("ensureLoaded(0) error = %v, want nil (DB should not be called)", err)
	}
	if got := s.At(0); got == nil || got.Service != "preloaded" {
		t.Errorf("At(0).Service = %q, want %q", s.At(0).Service, "preloaded")
	}
}

// ---------------------------------------------------------------------------
// DBEventStream Total tests
// ---------------------------------------------------------------------------

func TestDBEventStream_Total_Preloaded(t *testing.T) {
	s := NewDBEventStream(nil, "wf-1", 100)
	s.totalLoaded = true
	s.totalCount = 5

	total, err := s.Total()
	if err != nil {
		t.Errorf("Total() error = %v, want nil", err)
	}
	if total != 5 {
		t.Errorf("Total() = %d, want 5", total)
	}
}

func TestDBEventStream_Total_DBQuery(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "COUNT", data: [][]driver.Value{{int64(5)}}},
	}, nil)
	defer db.Close()

	s := NewDBEventStream(db, "wf-1", 100)
	total, err := s.Total()
	if err != nil {
		t.Fatalf("Total() error = %v, want nil", err)
	}
	if total != 5 {
		t.Errorf("Total() = %d, want 5", total)
	}
	if !s.totalLoaded {
		t.Errorf("totalLoaded should be true after successful Total()")
	}
	if s.totalCount != 5 {
		t.Errorf("totalCount = %d, want 5", s.totalCount)
	}
}

func TestDBEventStream_Total_DBQueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "COUNT", err: errors.New("count error")},
	}, nil)
	defer db.Close()

	s := NewDBEventStream(db, "wf-1", 100)
	s.Append(EventRecord{Step: 0})

	total, err := s.Total()
	if err == nil {
		t.Fatal("Total() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "count error") {
		t.Errorf("error = %v, want substring 'count error'", err)
	}
	if total != 1 {
		t.Errorf("Total() = %d, want 1 (fallback to len(loaded))", total)
	}
}
