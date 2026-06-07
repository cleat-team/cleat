package wasm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestReadLockFile_Valid(t *testing.T) {
	dir := t.TempDir()
	content := `{"version":1,"entries":{"child1":{"version":5},"child2":{"version":3}}}`
	if err := os.WriteFile(filepath.Join(dir, LockFileName), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	lf, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("ReadLockFile: %v", err)
	}
	if lf.Version != 1 {
		t.Errorf("expected version 1, got %d", lf.Version)
	}
	if len(lf.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(lf.Entries))
	}
	if lf.Entries["child1"].Version != 5 {
		t.Errorf("expected child1 version 5, got %d", lf.Entries["child1"].Version)
	}
	if lf.Entries["child2"].Version != 3 {
		t.Errorf("expected child2 version 3, got %d", lf.Entries["child2"].Version)
	}
}

func TestReadLockFile_Missing(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadLockFile(dir)
	if err == nil {
		t.Fatal("expected error for missing cleat.lock")
	}
}

func TestReadLockFile_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LockFileName), []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ReadLockFile(dir)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestReadLockFile_EmptyEntries(t *testing.T) {
	dir := t.TempDir()
	content := `{"version":1,"entries":{}}`
	if err := os.WriteFile(filepath.Join(dir, LockFileName), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	lf, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("ReadLockFile: %v", err)
	}
	if lf.Version != 1 {
		t.Errorf("expected version 1, got %d", lf.Version)
	}
	if len(lf.Entries) != 0 {
		t.Errorf("expected empty entries, got %d", len(lf.Entries))
	}
}

func TestWriteLockFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	lf := &LockFile{
		Version: 1,
		Entries: map[string]LockEntry{
			"wf1": {Version: 2},
			"wf2": {Version: 7},
		},
	}

	if err := WriteLockFile(dir, lf); err != nil {
		t.Fatalf("WriteLockFile: %v", err)
	}

	got, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("ReadLockFile after write: %v", err)
	}
	if got.Version != lf.Version {
		t.Errorf("version: got %d, want %d", got.Version, lf.Version)
	}
	if len(got.Entries) != len(lf.Entries) {
		t.Fatalf("entries count: got %d, want %d", len(got.Entries), len(lf.Entries))
	}
	for name, entry := range lf.Entries {
		if got.Entries[name].Version != entry.Version {
			t.Errorf("entry %q: got version %d, want %d", name, got.Entries[name].Version, entry.Version)
		}
	}
}

func TestWriteLockFile_NilEntries(t *testing.T) {
	dir := t.TempDir()
	lf := &LockFile{
		Version: 1,
		Entries: nil,
	}

	if err := WriteLockFile(dir, lf); err != nil {
		t.Fatalf("WriteLockFile: %v", err)
	}

	got, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("ReadLockFile after write: %v", err)
	}
	if got.Version != 1 {
		t.Errorf("expected version 1, got %d", got.Version)
	}
	if got.Entries != nil {
		t.Error("expected nil entries after round-trip (JSON null -> nil)")
	}
}

func TestWriteLockFile_VerifyOnDisk(t *testing.T) {
	dir := t.TempDir()
	lf := &LockFile{
		Version: 1,
		Entries: map[string]LockEntry{
			"test": {Version: 3},
		},
	}

	if err := WriteLockFile(dir, lf); err != nil {
		t.Fatalf("WriteLockFile: %v", err)
	}

	// Verify file exists and is readable JSON.
	data, err := os.ReadFile(filepath.Join(dir, LockFileName))
	if err != nil {
		t.Fatalf("os.ReadFile: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty lock file")
	}
}

// ---- mock *sql.DB for ResolveChildVersionsFromDB ----

type lockTestConnector struct {
	results map[string]int64 // child name → version (0 = not found)
	err     error
}

func (c *lockTestConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return &lockTestConn{connector: c}, nil
}

func (c *lockTestConnector) Driver() driver.Driver {
	return &lockTestDriver{}
}

type lockTestDriver struct{}

func (d *lockTestDriver) Open(name string) (driver.Conn, error) {
	return &lockTestConn{}, nil
}

type lockTestConn struct {
	connector *lockTestConnector
}

func (c *lockTestConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("Prepare not implemented")
}

func (c *lockTestConn) Close() error  { return nil }
func (c *lockTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("not supported")
}

// QueryContext implements driver.QueryerContext, used by sql.DB.QueryRowContext.
func (c *lockTestConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.connector.err != nil {
		return nil, c.connector.err
	}
	var v int64
	if len(args) > 0 {
		name := args[0].Value.(string)
		v = c.connector.results[name]
	}
	return &lockTestRows{value: v}, nil
}

type lockTestRows struct {
	value int64
	done  bool
}

func (r *lockTestRows) Columns() []string { return []string{"coalesce"} }
func (r *lockTestRows) Close() error      { return nil }
func (r *lockTestRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}

func newLockTestDB(results map[string]int64, err error) *sql.DB {
	return sql.OpenDB(&lockTestConnector{results: results, err: err})
}

// ---- ResolveChildVersionsFromDB tests ----

func TestResolveChildVersionsFromDB_SingleChild(t *testing.T) {
	db := newLockTestDB(map[string]int64{"child1": 5}, nil)
	defer db.Close()

	children := map[string]bool{"child1": true}
	got, err := ResolveChildVersionsFromDB(context.Background(), db, children)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got["child1"] != 5 {
		t.Errorf("expected child1=5, got %d", got["child1"])
	}
}

func TestResolveChildVersionsFromDB_MultipleChildren(t *testing.T) {
	db := newLockTestDB(map[string]int64{"a": 1, "b": 2, "c": 3}, nil)
	defer db.Close()

	children := map[string]bool{"a": true, "b": true, "c": true}
	got, err := ResolveChildVersionsFromDB(context.Background(), db, children)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	if got["a"] != 1 || got["b"] != 2 || got["c"] != 3 {
		t.Errorf("unexpected results: %v", got)
	}
}

func TestResolveChildVersionsFromDB_EmptyChildren(t *testing.T) {
	db := newLockTestDB(map[string]int64{}, nil)
	defer db.Close()

	got, err := ResolveChildVersionsFromDB(context.Background(), db, map[string]bool{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %d entries", len(got))
	}
}

func TestResolveChildVersionsFromDB_NoNonDeprecatedVersions(t *testing.T) {
	db := newLockTestDB(map[string]int64{"orphan": 0}, nil)
	defer db.Close()

	children := map[string]bool{"orphan": true}
	_, err := ResolveChildVersionsFromDB(context.Background(), db, children)
	if err == nil {
		t.Fatal("expected error for child with no non-deprecated versions")
	}
}

func TestResolveChildVersionsFromDB_DBError(t *testing.T) {
	db := newLockTestDB(nil, errors.New("connection refused"))
	defer db.Close()

	children := map[string]bool{"wf": true}
	_, err := ResolveChildVersionsFromDB(context.Background(), db, children)
	if err == nil {
		t.Fatal("expected error from DB, got nil")
	}
}
