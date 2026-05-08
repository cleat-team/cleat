package plugin

import (
	"fmt"
	"strings"
)

// DatabaseAccess represents the level of database access a plugin is granted.
type DatabaseAccess string

const (
	DatabaseAccessNone      DatabaseAccess = "none"
	DatabaseAccessReadOnly  DatabaseAccess = "read_only"
	DatabaseAccessReadWrite DatabaseAccess = "read_write"
)

// CapabilityLimits defines the maximum capabilities allowed for a class of plugins.
// Used by operators to restrict what third-party plugins can do.
type CapabilityLimits struct {
	Database         DatabaseAccess `json:"database"`
	StartWorkflow    bool           `json:"start_workflow"`
	SignalWorkflow   bool           `json:"signal_workflow"`
	HTTPRoutes       bool           `json:"http_routes"`
	HTTPMiddleware   bool           `json:"http_middleware"`
	BackgroundWorker bool           `json:"background_worker"`
	CallPlugin       []string       `json:"call_plugin"` // empty = deny all, ["*"] = allow all
}

// IsSet returns true if any capability limit is configured (non-default).
// Used to distinguish intentionally-set limits from a zero-value struct.
func (l CapabilityLimits) IsSet() bool {
	return l.Database != "" || l.StartWorkflow || l.SignalWorkflow ||
		l.HTTPRoutes || l.HTTPMiddleware || l.BackgroundWorker ||
		len(l.CallPlugin) > 0
}

// ValidateCapabilities checks whether declared capabilities are within the
// granted limits. Returns an error listing ALL violations, not just the first.
func ValidateCapabilities(declared, limits CapabilityLimits) error {
	var violations []string

	// Database access: declared level must be <= granted level.
	// Order: none < read_only < read_write.
	if !databaseAccessAllowed(declared.Database, limits.Database) {
		violations = append(violations, fmt.Sprintf("database access %q denied (max: %q)", declared.Database, limits.Database))
	}
	if declared.StartWorkflow && !limits.StartWorkflow {
		violations = append(violations, "start_workflow denied")
	}
	if declared.SignalWorkflow && !limits.SignalWorkflow {
		violations = append(violations, "signal_workflow denied")
	}
	if declared.HTTPRoutes && !limits.HTTPRoutes {
		violations = append(violations, "http_routes denied")
	}
	if declared.HTTPMiddleware && !limits.HTTPMiddleware {
		violations = append(violations, "http_middleware denied")
	}
	if declared.BackgroundWorker && !limits.BackgroundWorker {
		violations = append(violations, "background_worker denied")
	}

	// Check call_plugin: every plugin in declared must be in limits
	// (unless limits contains "*" which means allow all).
	if len(declared.CallPlugin) > 0 {
		allowAll := false
		for _, p := range limits.CallPlugin {
			if p == "*" {
				allowAll = true
				break
			}
		}
		if !allowAll {
			allowed := make(map[string]bool)
			for _, p := range limits.CallPlugin {
				allowed[p] = true
			}
			for _, p := range declared.CallPlugin {
				if !allowed[p] {
					violations = append(violations, fmt.Sprintf("call_plugin %q denied", p))
				}
			}
		}
	}

	if len(violations) > 0 {
		return fmt.Errorf("capability violations: %s", strings.Join(violations, "; "))
	}
	return nil
}

// databaseAccessAllowed returns true if the declared access level is within
// the granted limit. The hierarchy is: none < read_only < read_write.
func databaseAccessAllowed(declared, granted DatabaseAccess) bool {
	if granted == DatabaseAccessReadWrite {
		return true // read_write grants everything
	}
	if granted == DatabaseAccessReadOnly {
		return declared == DatabaseAccessNone || declared == DatabaseAccessReadOnly
	}
	// granted == none
	return declared == DatabaseAccessNone || declared == ""
}

// DefaultLimits returns the default CapabilityLimits for community plugins.
// By default, community plugins get NO database access, and signal_workflow,
// but NOT start_workflow or http_routes.
func DefaultLimits() CapabilityLimits {
	return CapabilityLimits{
		Database:         DatabaseAccessNone,
		StartWorkflow:    false,
		SignalWorkflow:   true,
		HTTPRoutes:       false,
		HTTPMiddleware:   false,
		BackgroundWorker: false,
		CallPlugin:       nil, // deny all by default
	}
}

// DeriveCapabilities derives CapabilityLimits from the optional interfaces a
// Go compile-time plugin implements. Called by the loader to determine what
// capabilities a Go plugin has based on which interfaces it satisfies.
func DeriveCapabilities(p Plugin) CapabilityLimits {
	caps := CapabilityLimits{}

	// Built-in infrastructure plugins that receive *sql.DB are granted
	// read-write access by their init registration (the default remains none).
	// This function only sets the capabilities that can be derived from
	// optional interface checks; the plugin's init() must set Database
	// explicitly via its PluginInfo if it needs access.

	if _, ok := p.(HasRoutes); ok {
		caps.HTTPRoutes = true
	}
	if _, ok := p.(HasMiddleware); ok {
		caps.HTTPMiddleware = true
	}
	if _, ok := p.(HasBackground); ok {
		caps.BackgroundWorker = true
	}
	// HasHostFunctions does not directly map to a capability -- it is what
	// the plugin provides, not what it consumes.

	return caps
}
