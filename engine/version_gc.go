package engine

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"
)

const (
	// DefaultMinVersionsToKeep is the minimum number of recent versions to
	// retain during GC, regardless of age or activity.
	DefaultMinVersionsToKeep = 3

	// DefaultMaxVersionAge is the maximum age of a deprecated version before
	// it becomes eligible for GC.
	DefaultMaxVersionAge = 30 * 24 * time.Hour // 30 days

	// DefaultGCOffset is the default age threshold for purging versions
	// that have zero active instances.
	DefaultGCOffset = 7 * 24 * time.Hour // 7 days
)

// GCOptions controls the behavior of version garbage collection.
type GCOptions struct {
	// MinVersionsToKeep retains at least this many recent versions for each
	// workflow, even if they are deprecated and have no active instances.
	MinVersionsToKeep int

	// MaxVersionAge is the maximum age of a deprecated version before it is
	// eligible for removal.
	MaxVersionAge time.Duration

	// DryRun logs what would be removed without actually deleting anything.
	DryRun bool

	// Now is the reference time for age calculations. Defaults to time.Now().
	Now time.Time
}

// DefaultGCOptions returns sensible defaults for version GC.
func DefaultGCOptions() GCOptions {
	return GCOptions{
		MinVersionsToKeep: DefaultMinVersionsToKeep,
		MaxVersionAge:     DefaultMaxVersionAge,
		DryRun:            false,
	}
}

// GCResult summarizes the outcome of a garbage collection run.
type GCResult struct {
	// VersionsRemoved is the number of workflow definition versions deleted.
	VersionsRemoved int

	// VersionsSkipped is the number of deprecated versions that were
	// considered but skipped (e.g., because they still have active instances
	// or are protected by MinVersionsToKeep).
	VersionsSkipped int

	// Errors holds per-version errors that did not abort the entire GC run.
	Errors []error
}

// GarbageCollectVersions removes deprecated workflow definition versions that
// are no longer needed. It considers age, active instance counts, and the
// configured minimum versions to keep.
func GarbageCollectVersions(ctx context.Context, store WorkflowStore, opts GCOptions) (*GCResult, error) {
	if opts.MinVersionsToKeep <= 0 {
		opts.MinVersionsToKeep = DefaultMinVersionsToKeep
	}
	if opts.MaxVersionAge <= 0 {
		opts.MaxVersionAge = DefaultMaxVersionAge
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	result := &GCResult{}

	// List all workflow definitions across all workflows.
	defs, err := store.ListWorkflowDefs(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list all workflow defs: %w", err)
	}

	// Get active instance counts for version-aware GC decisions.
	activeCounts, err := store.GetActiveInstanceCountsByVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("get active instance counts: %w", err)
	}

	// Group definitions by name.
	byName := make(map[string][]WorkflowDef)
	for i := range defs {
		byName[defs[i].Name] = append(byName[defs[i].Name], defs[i])
	}

	for _, versions := range byName {
		// Sort by version descending (newest first).
		sort.Slice(versions, func(i, j int) bool {
			return versions[i].Version > versions[j].Version
		})

		for i, def := range versions {
			if i < opts.MinVersionsToKeep {
				// Protected by the minimum count.
				continue
			}
			if !def.Deprecated {
				continue
			}
			if now.Sub(def.CreatedAt) < opts.MaxVersionAge {
				// Not old enough.
				continue
			}

			key := fmt.Sprintf("%s:%d", def.Name, def.Version)
			if activeCounts[key] > 0 {
				result.VersionsSkipped++
				continue
			}

			if opts.DryRun {
				log.Printf("[cleat][gc] would purge %s v%d (created %s)",
					def.Name, def.Version, def.CreatedAt.Format(time.RFC3339))
				result.VersionsRemoved++
				continue
			}

			if err := store.PurgeWorkflowDef(ctx, def.Name, def.Version); err != nil {
				err = fmt.Errorf("purge %s v%d: %w", def.Name, def.Version, err)
				result.Errors = append(result.Errors, err)
				continue
			}
			result.VersionsRemoved++
		}
	}

	return result, nil
}

// PurgeVersions removes all deprecated versions for a specific workflow that
// have zero active instances and are older than the given offset. Unlike
// GarbageCollectVersions, this operates on a single named workflow.
func PurgeVersions(ctx context.Context, store WorkflowStore, workflowName string, olderThan time.Duration) (*GCResult, error) {
	result := &GCResult{}

	defs, err := store.ListWorkflowDefs(ctx, workflowName)
	if err != nil {
		return nil, fmt.Errorf("list defs for %s: %w", workflowName, err)
	}

	cutoff := time.Now().Add(-olderThan)

	for _, def := range defs {
		if !def.Deprecated {
			continue
		}
		if def.CreatedAt.After(cutoff) {
			continue
		}

		count, err := store.CountActiveInstances(ctx, def.Name, def.Version)
		if err != nil {
			err = fmt.Errorf("count active instances for %s v%d: %w", def.Name, def.Version, err)
			result.Errors = append(result.Errors, err)
			continue
		}
		if count > 0 {
			result.VersionsSkipped++
			continue
		}

		if err := store.PurgeWorkflowDef(ctx, def.Name, def.Version); err != nil {
			err = fmt.Errorf("purge %s v%d: %w", def.Name, def.Version, err)
			result.Errors = append(result.Errors, err)
			continue
		}
		result.VersionsRemoved++
	}

	return result, nil
}
