package wasm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// LockFileVersion is the current cleat.lock schema version.
const LockFileVersion = 2

// LockFileName is the standard lock file name.
const LockFileName = "cleat.lock"

// LockEntry records a pinned child workflow version.
type LockEntry struct {
	Version int `json:"version"`
}

// LockFile stores resolved child workflow versions for reproducible builds.
type LockFile struct {
	Version int                  `json:"version"`
	Policy  string               `json:"policy,omitempty"` // the binding policy used ("frozen", "stable", "latest", "tag:X")
	Entries map[string]LockEntry `json:"entries"`
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
