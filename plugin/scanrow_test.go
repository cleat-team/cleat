package plugin

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeRow returns canned values, and records what it was handed.
type fakeRow struct {
	vals []any
	err  error
	got  []any
}

func (f *fakeRow) Scan(dest ...any) error {
	f.got = dest
	if f.err != nil {
		return f.err
	}
	for i, d := range dest {
		if i >= len(f.vals) {
			break
		}
		switch t := d.(type) {
		case *GUID:
			if err := t.Scan(f.vals[i]); err != nil {
				return err
			}
		case *NullGUID:
			if err := t.Scan(f.vals[i]); err != nil {
				return err
			}
		case *uuid.UUID:
			if err := t.Scan(f.vals[i]); err != nil {
				return err
			}
		case *uuid.NullUUID:
			if err := t.Scan(f.vals[i]); err != nil {
				return err
			}
		case *string:
			*t, _ = f.vals[i].(string)
		}
	}
	return nil
}

// SQL Server's mixed-endian bytes for a known uuid: the first three groups are
// byte-reversed relative to the RFC-4122 wire order.
func mixedEndian(u uuid.UUID) []byte {
	b := make([]byte, 16)
	copy(b, u[:])
	b[0], b[1], b[2], b[3] = u[3], u[2], u[1], u[0]
	b[4], b[5] = u[5], u[4]
	b[6], b[7] = u[7], u[6]
	return b
}

// The case cleat#1137 is about: 16 raw bytes in SQL Server's order.
func TestScanRowCorrectsMixedEndianBytes(t *testing.T) {
	want := uuid.MustParse("11223344-5566-7788-99aa-bbccddeeff00")
	row := &fakeRow{vals: []any{mixedEndian(want)}}

	var got uuid.UUID
	if err := ScanRow(row, &got); err != nil {
		t.Fatalf("ScanRow: %v", err)
	}
	if got != want {
		t.Errorf("ScanRow gave %s, want %s.\n\nSQL Server returns UNIQUEIDENTIFIER "+
			"mixed-endian; uuid.UUID's Scan takes those bytes without error and "+
			"produces a different id, which is the whole of cleat#1137.", got, want)
	}
}

// The control. Without it, a ScanRow that simply zeroed every uuid would pass
// the test above if `want` were the zero uuid, and a ScanRow that never swapped
// would pass on any dialect that already returns the right order.
func TestScanRowIsTheThingThatCorrects(t *testing.T) {
	want := uuid.MustParse("11223344-5566-7788-99aa-bbccddeeff00")
	row := &fakeRow{vals: []any{mixedEndian(want)}}

	var direct uuid.UUID
	if err := row.Scan(&direct); err != nil {
		t.Fatalf("direct Scan: %v", err)
	}
	if direct == want {
		t.Fatal("a direct uuid.UUID Scan already produced the right id, so this " +
			"fixture does not reproduce cleat#1137 and the test above proves nothing")
	}
}

// Text and non-uuid values must pass through untouched -- ScanRow is applied to
// whole Scan calls, so it has to be transparent for everything else.
func TestScanRowLeavesOtherDestinationsAlone(t *testing.T) {
	want := uuid.MustParse("11223344-5566-7788-99aa-bbccddeeff00")
	row := &fakeRow{vals: []any{mixedEndian(want), "hello"}}

	var id uuid.UUID
	var s string
	if err := ScanRow(row, &id, &s); err != nil {
		t.Fatalf("ScanRow: %v", err)
	}
	if id != want || s != "hello" {
		t.Errorf("got (%s, %q), want (%s, %q)", id, s, want, "hello")
	}
	if _, isGUID := row.got[1].(*GUID); isGUID {
		t.Error("a *string destination was substituted; only *uuid.UUID may be")
	}
}

// PostgreSQL and MySQL already deliver the correct order, usually as text.
// GUID.Scan defers to uuid.UUID for anything that is not 16 raw bytes, and
// ScanRow must not change that.
func TestScanRowDoesNotDisturbTextForms(t *testing.T) {
	want := uuid.MustParse("11223344-5566-7788-99aa-bbccddeeff00")
	row := &fakeRow{vals: []any{want.String()}}

	var got uuid.UUID
	if err := ScanRow(row, &got); err != nil {
		t.Fatalf("ScanRow: %v", err)
	}
	if got != want {
		t.Errorf("text form scanned as %s, want %s -- the correction must apply "+
			"only to raw 16-byte values, or PostgreSQL and MySQL break", got, want)
	}
}

// A failed Scan must leave destinations untouched, as database/sql does.
func TestScanRowDoesNotWriteBackOnError(t *testing.T) {
	row := &fakeRow{err: errors.New("boom")}
	before := uuid.MustParse("11223344-5566-7788-99aa-bbccddeeff00")
	got := before
	if err := ScanRow(row, &got); err == nil {
		t.Fatal("expected the underlying Scan error")
	}
	if got != before {
		t.Errorf("destination was modified on a failed Scan: %s -> %s", before, got)
	}
}

// cleat#2292 item 6's mechanism: a *uuid.NullUUID destination must get the
// same SQL Server byte-order correction as *uuid.UUID, or a nullable id
// column is correct on PostgreSQL/MySQL and silently wrong on SQL Server --
// exactly cleat#1137, one type over. Without NullGUID, ScanRow's original
// type switch (only *uuid.UUID) passes a *uuid.NullUUID straight through
// unswapped.
func TestScanRowCorrectsMixedEndianBytesForNullUUID(t *testing.T) {
	want := uuid.MustParse("11223344-5566-7788-99aa-bbccddeeff00")
	row := &fakeRow{vals: []any{mixedEndian(want)}}

	var got uuid.NullUUID
	if err := ScanRow(row, &got); err != nil {
		t.Fatalf("ScanRow: %v", err)
	}
	if !got.Valid || got.UUID != want {
		t.Errorf("ScanRow gave (valid=%v, %s), want (valid=true, %s)", got.Valid, got.UUID, want)
	}
}

// The control for the test above, mirroring TestScanRowIsTheThingThatCorrects:
// without it, a NullGUID that just aliased uuid.NullUUID.Scan would pass a
// mixed-endian test whose "want" happened to already look right, or a broken
// swap that always zeroed would pass if "want" were the zero uuid.
func TestScanRowNullUUIDIsTheThingThatCorrects(t *testing.T) {
	want := uuid.MustParse("11223344-5566-7788-99aa-bbccddeeff00")
	row := &fakeRow{vals: []any{mixedEndian(want)}}

	var direct uuid.NullUUID
	if err := row.Scan(&direct); err != nil {
		t.Fatalf("direct Scan: %v", err)
	}
	if direct.Valid && direct.UUID == want {
		t.Fatal("a direct uuid.NullUUID Scan already produced the right id, so this " +
			"fixture does not reproduce cleat#1137 and the test above proves nothing")
	}
}

// A SQL NULL must scan as Valid=false, not as the zero UUID or an error --
// this is the actual cleat#2292 item 6 symptom: a NULL config_id displayed as
// 00000000-0000-0000-0000-000000000000 rather than reported as absent.
func TestScanRowNullUUIDHandlesSQLNull(t *testing.T) {
	row := &fakeRow{vals: []any{nil}}

	var got uuid.NullUUID
	got.Valid = true // start non-zero-value so a no-op Scan would be caught
	if err := ScanRow(row, &got); err != nil {
		t.Fatalf("ScanRow: %v", err)
	}
	if got.Valid {
		t.Errorf("ScanRow reported Valid=true for a SQL NULL; got.UUID=%s", got.UUID)
	}
}

// Same as TestScanRowLeavesOtherDestinationsAlone, for the NullUUID branch:
// a *uuid.NullUUID destination is substituted with *NullGUID, nothing else.
func TestScanRowNullUUIDLeavesOtherDestinationsAlone(t *testing.T) {
	want := uuid.MustParse("11223344-5566-7788-99aa-bbccddeeff00")
	row := &fakeRow{vals: []any{mixedEndian(want), "hello"}}

	var id uuid.NullUUID
	var s string
	if err := ScanRow(row, &id, &s); err != nil {
		t.Fatalf("ScanRow: %v", err)
	}
	if !id.Valid || id.UUID != want || s != "hello" {
		t.Errorf("got (valid=%v, %s, %q), want (valid=true, %s, %q)", id.Valid, id.UUID, s, want, "hello")
	}
	if _, isNullGUID := row.got[1].(*NullGUID); isNullGUID {
		t.Error("a *string destination was substituted; only *uuid.UUID/*uuid.NullUUID may be")
	}
}
