package engine

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"
)

// VersionMetrics holds operational metrics for a single workflow version.
type VersionMetrics struct {
	Name            string    `json:"name"`
	Version         int       `json:"version"`
	Deprecated      bool      `json:"deprecated"`
	CreatedAt       time.Time `json:"created_at"`
	Age             string    `json:"age"` // human-readable age
	ActiveInstances int       `json:"active_instances"`
	ABIVersion      int       `json:"abi_version"`
	MinVersion      int       `json:"min_version"`
}

// VersionMetricsSummary is a complete summary of version metrics across all
// workflow definitions.
type VersionMetricsSummary struct {
	TotalVersions        int              `json:"total_versions"`
	ActiveVersions       int              `json:"active_versions"`
	Deprecated           int              `json:"deprecated"`
	TotalActiveInstances int              `json:"total_active_instances"`
	Workflows            []VersionMetrics `json:"workflows"`
}

// StaleVersionAlert describes a workflow version that may need attention.
type StaleVersionAlert struct {
	Name             string `json:"name"`
	Version          int    `json:"version"`
	Deprecated       bool   `json:"deprecated"`
	ActiveInstances  int    `json:"active_instances"`
	DaysSinceCreated int    `json:"days_since_created"`
	Message          string `json:"message"`
}

// CollectVersionMetrics gathers metrics for all deployed workflow versions.
func CollectVersionMetrics(ctx context.Context, store WorkflowStore) (*VersionMetricsSummary, error) {
	defs, err := store.ListWorkflowDefs(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list all workflow defs: %w", err)
	}

	activeCounts, err := store.GetActiveInstanceCountsByVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("get active instance counts: %w", err)
	}

	summary := &VersionMetricsSummary{
		TotalVersions: len(defs),
	}

	for _, def := range defs {
		key := fmt.Sprintf("%s:%d", def.Name, def.Version)
		count := activeCounts[key]

		vm := VersionMetrics{
			Name:            def.Name,
			Version:         def.Version,
			Deprecated:      def.Deprecated,
			CreatedAt:       def.CreatedAt,
			Age:             formatDuration(time.Since(def.CreatedAt)),
			ActiveInstances: count,
			ABIVersion:      def.ABIVersion,
			MinVersion:      def.MinVersion,
		}

		summary.Workflows = append(summary.Workflows, vm)
		summary.TotalActiveInstances += count

		if def.Deprecated {
			summary.Deprecated++
		} else {
			summary.ActiveVersions++
		}
	}

	// Sort by name then version descending.
	sort.Slice(summary.Workflows, func(i, j int) bool {
		if summary.Workflows[i].Name != summary.Workflows[j].Name {
			return summary.Workflows[i].Name < summary.Workflows[j].Name
		}
		return summary.Workflows[i].Version > summary.Workflows[j].Version
	})

	return summary, nil
}

// CheckStaleVersions scans for versions that may need attention:
//   - Non-deprecated versions older than staleThreshold that still have
//     active instances (potential migration candidates).
//   - Deprecated versions older than purgeThreshold with zero active
//     instances (GC candidates).
func CheckStaleVersions(ctx context.Context, store WorkflowStore, staleThreshold, purgeThreshold time.Duration) ([]StaleVersionAlert, error) {
	defs, err := store.ListWorkflowDefs(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list all workflow defs: %w", err)
	}

	activeCounts, err := store.GetActiveInstanceCountsByVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("get active instance counts: %w", err)
	}

	now := time.Now()
	var alerts []StaleVersionAlert

	for _, def := range defs {
		key := fmt.Sprintf("%s:%d", def.Name, def.Version)
		count := activeCounts[key]
		daysSinceCreated := int(now.Sub(def.CreatedAt).Hours() / 24)

		if def.Deprecated {
			if count == 0 && now.Sub(def.CreatedAt) >= purgeThreshold {
				alerts = append(alerts, StaleVersionAlert{
					Name:             def.Name,
					Version:          def.Version,
					Deprecated:       true,
					ActiveInstances:  0,
					DaysSinceCreated: daysSinceCreated,
					Message:          fmt.Sprintf("deprecated v%d has no active instances, eligible for GC (%d days old)", def.Version, daysSinceCreated),
				})
			} else if count > 0 && now.Sub(def.CreatedAt) >= staleThreshold {
				alerts = append(alerts, StaleVersionAlert{
					Name:             def.Name,
					Version:          def.Version,
					Deprecated:       true,
					ActiveInstances:  count,
					DaysSinceCreated: daysSinceCreated,
					Message:          fmt.Sprintf("deprecated v%d still has %d active instance(s) (%d days old)", def.Version, count, daysSinceCreated),
				})
			}
		} else {
			if now.Sub(def.CreatedAt) >= staleThreshold {
				alerts = append(alerts, StaleVersionAlert{
					Name:             def.Name,
					Version:          def.Version,
					Deprecated:       false,
					ActiveInstances:  count,
					DaysSinceCreated: daysSinceCreated,
					Message:          fmt.Sprintf("non-deprecated v%d is %d days old with %d active instance(s), consider migration", def.Version, daysSinceCreated, count),
				})
			}
		}
	}

	return alerts, nil
}

// LogStaleAlerts prints stale version alerts to the standard logger.
func LogStaleAlerts(alerts []StaleVersionAlert) {
	if len(alerts) == 0 {
		return
	}
	log.Printf("[cleat][versions] %d stale version alert(s):", len(alerts))
	for _, a := range alerts {
		log.Printf("[cleat][versions]   %s: %s", a.Name, a.Message)
	}
}

// formatDuration returns a human-readable duration string.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		days := int(d.Hours() / 24)
		return fmt.Sprintf("%dd", days)
	}
}
