package clewexecutor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// sessionRecord stores the full output plus phase for idempotent caching.
// When a cache hit occurs, these fields are returned directly as runPhaseOutput.
// WorkflowPhase distinguishes runs of different workflow phases that share the
// same STATUS.md phase (e.g., review_plan and implement both see "plan_review").
type sessionRecord struct {
	Phase            string   `json:"phase"`
	WorkflowPhase    string   `json:"wf_phase,omitempty"`
	ExitCode         int      `json:"exit_code"`
	PhaseChanged     bool     `json:"phase_changed"`
	NewPhase         string   `json:"new_phase"`
	ReviewOutcome    string   `json:"review_outcome"`
	ArtifactsWritten []string `json:"artifacts_written"`
	FindingsCount    int      `json:"findings_count"`
	Started          string   `json:"started"`
	Ended            string   `json:"ended"`
	Status           string   `json:"status"`
	CrashLog         string   `json:"crash_log,omitempty"`
	DurationMs       int64    `json:"duration_ms,omitempty"`
}

// readSession reads and unmarshals session.json. Returns nil if file doesn't
// exist, is corrupt, or can't be parsed.
func readSession(path string) (*sessionRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read session: %w", err)
	}
	var rec sessionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, nil // corrupt JSON → treat as missing
	}
	return &rec, nil
}

// writeSession writes session record as JSON to path atomically (write to
// temp file + rename to avoid partial writes).
func writeSession(path string, rec *sessionRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write session tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename session tmp: %w", err)
	}
	return nil
}

var phaseRe = regexp.MustCompile(`\*\*Phase:\*\*\s*(\w+)`)

// extractPhase reads STATUS.md and extracts **Phase:** <value> via regex.
func extractPhase(statusPath string) (string, error) {
	data, err := os.ReadFile(statusPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("STATUS.md not found at %s", statusPath)
		}
		return "", fmt.Errorf("read STATUS.md: %w", err)
	}
	matches := phaseRe.FindSubmatch(data)
	if len(matches) < 2 {
		return "", fmt.Errorf("no Phase line found in %s", statusPath)
	}
	return string(matches[1]), nil
}

// roleForPhase maps Clew task phases to agent roles (matching clew-run.sh).
func roleForPhase(phase string) (string, error) {
	switch phase {
	case "queued", "exploring":
		return "explorer", nil
	case "planning":
		return "planner", nil
	case "plan_review":
		return "reviewer", nil
	case "implementing":
		return "developer", nil
	case "impl_review":
		return "reviewer", nil
	case "create_pr", "ci_fix", "merge":
		return "developer", nil
	case "surveying":
		return "explorer", nil // CTO lap survey phase
	case "deciding", "brief":
		return "reviewer", nil // CTO lap decision/brief phases
	case "done":
		return "", fmt.Errorf("task is already done")
	case "blocked", "failed":
		return "", fmt.Errorf("task is %s — unblock or re-scope before running", phase)
	default:
		return "", fmt.Errorf("unknown phase: %s", phase)
	}
}

// taskDir returns the task directory path.
func taskDir(projectRoot, project, taskID string) string {
	if project == "clew" {
		return filepath.Join(projectRoot, "task_state", taskID)
	}
	return filepath.Join(projectRoot, "projects", project, taskID)
}
