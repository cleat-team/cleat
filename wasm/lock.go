package wasm

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// LockFileVersion is the current cleat.lock schema version.
const LockFileVersion = 1

// LockFileName is the standard lock file name.
const LockFileName = "cleat.lock"

// LockEntry records a pinned child workflow version.
type LockEntry struct {
	Version int `json:"version"`
}

// LockFile stores resolved child workflow versions for reproducible builds.
type LockFile struct {
	Version int                  `json:"version"`
	Entries map[string]LockEntry `json:"entries"`
}

// ResolveChildVersionsFromDB queries the database for the latest non-deprecated
// version of each child workflow name. Returns a map of name -> version.
func ResolveChildVersionsFromDB(ctx context.Context, db *sql.DB, children map[string]bool) (map[string]int, error) {
	result := make(map[string]int, len(children))
	for name := range children {
		var v int
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(version), 0) FROM workflow_defs
			WHERE name = $1 AND NOT deprecated
		`, name).Scan(&v)
		if err != nil {
			return nil, fmt.Errorf("resolve child %q: %w", name, err)
		}
		if v == 0 {
			return nil, fmt.Errorf("child workflow %q has no non-deprecated versions deployed", name)
		}
		result[name] = v
	}
	return result, nil
}

// ReadLockFile reads and parses a cleat.lock file from dir.
func ReadLockFile(dir string) (*LockFile, error) {
	data, err := os.ReadFile(filepath.Join(dir, LockFileName))
	if err != nil {
		return nil, err
	}
	var lf LockFile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", LockFileName, err)
	}
	return &lf, nil
}

// WriteLockFile writes a cleat.lock file to dir.
func WriteLockFile(dir string, lf *LockFile) error {
	data, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, LockFileName), data, 0644)
}
