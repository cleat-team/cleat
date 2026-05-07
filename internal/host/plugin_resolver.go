package host

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// PluginConstraint represents a version constraint for a plugin dependency.
type PluginConstraint struct {
	Name       string `json:"name"`
	Constraint string `json:"constraint"` // semver: ">=1.2.0", "~1.2.0", "^1.2.0", "=1.2.0"
}

// semverVersion is a simple semantic version for plugin matching.
// In production, use github.com/Masterminds/semver/v3 for full spec compliance.
type semverVersion struct {
	Major int
	Minor int
	Patch int
}

// parseVersion parses a "major.minor.patch" semver string.
func parseVersion(v string) (semverVersion, error) {
	v = strings.TrimLeft(v, "vV ")
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		return semverVersion{}, fmt.Errorf("invalid semver %q: need at least major.minor.patch", v)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return semverVersion{}, fmt.Errorf("invalid major version in %q: %w", v, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return semverVersion{}, fmt.Errorf("invalid minor version in %q: %w", v, err)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return semverVersion{}, fmt.Errorf("invalid patch version in %q: %w", v, err)
	}
	return semverVersion{Major: major, Minor: minor, Patch: patch}, nil
}

// matchesConstraint checks whether version satisfies the given constraint string.
// Supported operators: >=, <=, >, <, = (or ==), ~ (patch-locked), ^ (minor-locked).
func matchesConstraint(ver semverVersion, constraint string) bool {
	constraint = strings.TrimSpace(constraint)

	// Determine operator and target version string.
	var operator string
	var verStr string
	for _, op := range []string{">=", "<=", "~", "^", ">", "<", "==", "="} {
		if strings.HasPrefix(constraint, op) {
			operator = op
			verStr = strings.TrimSpace(constraint[len(op):])
			break
		}
	}
	if operator == "" {
		// Bare version: exact match.
		operator = "="
		verStr = constraint
	}

	target, err := parseVersion(verStr)
	if err != nil {
		return false
	}

	switch operator {
	case "=", "==":
		return ver == target
	case ">=":
		return ver.compareTo(target) >= 0
	case ">":
		return ver.compareTo(target) > 0
	case "<=":
		return ver.compareTo(target) <= 0
	case "<":
		return ver.compareTo(target) < 0
	case "~":
		// Patch-locked: >= target, < next minor.
		if ver.Major != target.Major || ver.Minor != target.Minor {
			return false
		}
		return ver.Patch >= target.Patch
	case "^":
		// Minor-locked: >= target, < next major.
		if ver.Major != target.Major {
			return false
		}
		if ver.Minor > target.Minor {
			return true
		}
		return ver.Minor == target.Minor && ver.Patch >= target.Patch
	}
	return false
}

// compareTo returns -1 if v < other, 0 if v == other, 1 if v > other.
func (v semverVersion) compareTo(other semverVersion) int {
	if v.Major != other.Major {
		if v.Major < other.Major {
			return -1
		}
		return 1
	}
	if v.Minor != other.Minor {
		if v.Minor < other.Minor {
			return -1
		}
		return 1
	}
	if v.Patch < other.Patch {
		return -1
	}
	if v.Patch > other.Patch {
		return 1
	}
	return 0
}

// ResolvePlugins resolves all plugin dependencies to specific versions.
// Takes the plugin_deps JSON from workflow_defs (format: {"llm": ">=1.2.0"}),
// queries plugin_defs, and returns pinned version strings.
//
// Resolution strategy:
//  1. For each plugin dependency, find all non-deprecated versions from
//     plugin_defs ordered by created_at DESC (newest first).
//  2. Return the newest version that satisfies the constraint.
//  3. If no version satisfies the constraint, return an error.
func ResolvePlugins(ctx context.Context, db *sql.DB, pluginDepsJSON string) (map[string]string, error) {
	if pluginDepsJSON == "" || pluginDepsJSON == "{}" {
		return make(map[string]string), nil
	}

	// Try parsing as map[string]string first (common format).
	var deps map[string]string
	if err := json.Unmarshal([]byte(pluginDepsJSON), &deps); err != nil {
		// Try as array of PluginConstraint.
		var constraints []PluginConstraint
		if err2 := json.Unmarshal([]byte(pluginDepsJSON), &constraints); err2 != nil {
			return nil, fmt.Errorf("resolve plugins: invalid plugin_deps JSON: %w (attempted map and array)", err)
		}
		deps = make(map[string]string, len(constraints))
		for _, c := range constraints {
			deps[c.Name] = c.Constraint
		}
	}

	result := make(map[string]string, len(deps))
	for name, constraintStr := range deps {
		version, err := resolveOnePlugin(ctx, db, name, constraintStr)
		if err != nil {
			return nil, fmt.Errorf("resolve plugin %q: %w", name, err)
		}
		result[name] = version
	}
	return result, nil
}

// resolveOnePlugin finds the best version of a single plugin that satisfies
// the given constraint. Queries plugin_defs for all non-deprecated versions
// of the plugin, ordered by created_at DESC (newest first), and returns the
// first one that matches.
func resolveOnePlugin(ctx context.Context, db *sql.DB, name, constraintStr string) (string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT version FROM plugin_defs
		WHERE name = $1 AND NOT deprecated
		ORDER BY created_at DESC
	`, name)
	if err != nil {
		return "", fmt.Errorf("query plugin versions: %w", err)
	}
	defer rows.Close()

	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return "", fmt.Errorf("scan plugin version: %w", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate plugin versions: %w", err)
	}

	if len(versions) == 0 {
		return "", fmt.Errorf("no versions found for plugin %q", name)
	}

	// Try each version (newest first) and return the first match.
	for _, v := range versions {
		ver, err := parseVersion(v)
		if err != nil {
			continue // skip malformed versions
		}
		if matchesConstraint(ver, constraintStr) {
			return v, nil
		}
	}

	return "", fmt.Errorf("no version of plugin %q satisfies constraint %q (available: %v)",
		name, constraintStr, versions)
}
