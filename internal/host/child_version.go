package host

import (
	"context"
	"database/sql"
	"fmt"
)

// ChildWorkflowOptions carries version resolution configuration for
// spawning a child workflow. Passed through the WASM ABI from SDKs.
type ChildWorkflowOptions struct {
	// Version is the explicit child workflow version to use.
	//   > 0  : use this version explicitly
	//   <= 0 : use default resolution (parent's version, or latest compatible)
	Version int `json:"version"`
}

// ResolveChildVersion determines the version for a child workflow.
// Rules in priority order:
//
//  1. Explicit pin: opts.Version > 0 → use that version.
//  2. Parent's version: child uses same version as parent (default when
//     opts.Version <= 0). This ensures tightly-coupled workflows stay
//     on compatible versions.
//  3. Latest compatible: if the parent's version does not exist for the
//     child workflow name, fall back to:
//     SELECT MAX(version) FROM workflow_defs
//     WHERE name = $1 AND min_version <= parentVersion AND NOT deprecated
//
// Args:
//   - db: database connection (may be nil; if nil, rule 3 is skipped)
//   - childName: the workflow definition name of the child
//   - parentVersion: the version of the parent workflow
//   - opts: version options from the caller
//
// Returns the resolved version number, or an error if no version can be found.
func ResolveChildVersion(ctx context.Context, db *sql.DB,
	childName string, parentVersion int, opts ChildWorkflowOptions) (int, error) {

	// Rule 1: Explicit pin.
	if opts.Version > 0 {
		// Validate that this version actually exists in workflow_defs.
		if db != nil {
			exists, err := versionExists(ctx, db, childName, opts.Version)
			if err != nil {
				return 0, fmt.Errorf("resolve child version: check explicit version %d for %q: %w",
					opts.Version, childName, err)
			}
			if !exists {
				return 0, fmt.Errorf("resolve child version: explicit version %d not found for workflow %q",
					opts.Version, childName)
			}
		}
		return opts.Version, nil
	}

	// Rule 2 (default): Same version as parent.
	if db != nil {
		exists, err := versionExists(ctx, db, childName, parentVersion)
		if err != nil {
			return 0, fmt.Errorf("resolve child version: check parent version %d for %q: %w",
				parentVersion, childName, err)
		}
		if exists {
			return parentVersion, nil
		}
	} else {
		// No DB — return parent version as the best guess.
		return parentVersion, nil
	}

	// Rule 3: Latest compatible version.
	// Parent's version doesn't exist for this child, so find the latest
	// version that is compatible (min_version <= parentVersion).
	latest, err := latestCompatibleVersion(ctx, db, childName, parentVersion)
	if err != nil {
		return 0, fmt.Errorf("resolve child version: find latest compatible for %q: %w", childName, err)
	}
	if latest == 0 {
		// No compatible version found. Return the absolute latest as a
		// last resort, so the child workflow can at least start.
		latest, err = latestVersion(ctx, db, childName)
		if err != nil {
			return 0, fmt.Errorf("resolve child version: find any version for %q: %w", childName, err)
		}
		if latest == 0 {
			return 0, fmt.Errorf("resolve child version: no version found for workflow %q", childName)
		}
	}

	return latest, nil
}

// versionExists checks whether a specific (name, version) exists in
// workflow_defs and is not deprecated.
func versionExists(ctx context.Context, db *sql.DB, name string, version int) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_defs
		WHERE name = $1 AND version = $2 AND NOT deprecated
	`, name, version).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// latestCompatibleVersion returns the maximum version of the named workflow
// whose min_version <= parentVersion and is not deprecated.
func latestCompatibleVersion(ctx context.Context, db *sql.DB, name string, parentVersion int) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) FROM workflow_defs
		WHERE name = $1 AND min_version <= $2 AND NOT deprecated
	`, name, parentVersion).Scan(&version)
	if err != nil {
		return 0, err
	}
	return version, nil
}

// latestVersion returns the maximum non-deprecated version of the named workflow.
func latestVersion(ctx context.Context, db *sql.DB, name string) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) FROM workflow_defs
		WHERE name = $1 AND NOT deprecated
	`, name).Scan(&version)
	if err != nil {
		return 0, err
	}
	return version, nil
}
