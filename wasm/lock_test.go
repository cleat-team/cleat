package wasm

import (
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
