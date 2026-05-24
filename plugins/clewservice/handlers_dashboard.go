package clewservice

import (
	"net/http"
	"os"
	"sort"
)

// handleDashboardSummary returns aggregate stats from tasks.json.
func (p *Plugin) handleDashboardSummary(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	tasks, err := p.readTasksJSON(project)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "tasks.json not found")
			return
		}
		p.logger.Error("read tasks.json for dashboard", "project", project, "error", err)
		writeError(w, http.StatusInternalServerError, "read tasks.json: "+err.Error())
		return
	}

	summary := DashboardSummary{
		TotalTasks:     len(tasks.Tasks),
		TasksByStatus:  make(map[string]int),
		RecentActivity: make([]ActivityEntry, 0),
	}

	for _, t := range tasks.Tasks {
		summary.TasksByStatus[t.Status]++
		summary.TotalSpentUSD += t.Cost.SpentUSD
		summary.TotalBudgetUSD += t.Cost.BudgetUSD
	}

	// Collect recent activity: all tasks sorted by updated timestamp, most recent first.
	type entry struct {
		id      string
		subject string
		status  string
		updated string
	}
	var entries []entry
	for _, t := range tasks.Tasks {
		entries = append(entries, entry{
			id:      t.ID,
			subject: t.Subject,
			status:  t.Status,
			updated: t.Updated,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].updated > entries[j].updated
	})

	limit := 20
	if len(entries) < limit {
		limit = len(entries)
	}
	for i := 0; i < limit; i++ {
		summary.RecentActivity = append(summary.RecentActivity, ActivityEntry{
			TaskID:    entries[i].id,
			Timestamp: entries[i].updated,
			Action:    entries[i].status,
		})
	}

	writeJSON(w, http.StatusOK, summary)
}
