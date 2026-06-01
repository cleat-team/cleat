package engine

import (
	"fmt"
)

// ValidateVersionCompatibility checks whether a workflow instance running
// the old definition can be migrated to the new definition via
// ContinueAsNewWithVersion. Returns an error describing the first
// incompatibility found, or nil if the migration is safe.
func ValidateVersionCompatibility(oldDef, newDef *WorkflowDef) error {
	if newDef == nil {
		return fmt.Errorf("new workflow definition is nil")
	}
	if oldDef == nil {
		return fmt.Errorf("old workflow definition is nil")
	}

	// The new version must be strictly greater than the old version.
	if newDef.Version <= oldDef.Version {
		return fmt.Errorf(
			"new version %d must be greater than old version %d",
			newDef.Version, oldDef.Version,
		)
	}

	// The old version's MinVersion must be <= the new version.
	// This ensures backward compatibility: old workflows running at version
	// X can only migrate to versions >= X.MinVersion.
	if newDef.Version < oldDef.MinVersion {
		return fmt.Errorf(
			"new version %d is below old version's MinVersion %d",
			newDef.Version, oldDef.MinVersion,
		)
	}

	// Both definitions must use the same ABI version.
	if newDef.ABIVersion != oldDef.ABIVersion {
		return fmt.Errorf(
			"ABI version mismatch: old=%d new=%d",
			oldDef.ABIVersion, newDef.ABIVersion,
		)
	}

	// The new definition's MinVersion must be <= the old version.
	// This enforces that the new definition is willing to accept events
	// produced by the old version.
	if oldDef.Version < newDef.MinVersion {
		return fmt.Errorf(
			"old version %d is below new definition's MinVersion %d",
			oldDef.Version, newDef.MinVersion,
		)
	}

	// Check plugin dependency compatibility: every plugin the old definition
	// depends on must also be declared (or absent) in the new definition's
	// plugin deps. This is a structural check; runtime availability is
	// validated separately.
	for pluginName, oldVersion := range oldDef.PluginDeps {
		newVersion, ok := newDef.PluginDeps[pluginName]
		if !ok {
			return fmt.Errorf(
				"new definition is missing plugin dependency %q required by old version %d",
				pluginName, oldDef.Version,
			)
		}
		if newVersion != oldVersion {
			return fmt.Errorf(
				"plugin %q version mismatch: old=%s new=%s",
				pluginName, oldVersion, newVersion,
			)
		}
	}

	return nil
}
