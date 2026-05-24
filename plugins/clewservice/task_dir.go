package clewservice

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// safePath resolves a subpath relative to the project root and validates
// it does not escape via path traversal.
func (p *Plugin) safePath(subpath string) (string, error) {
	clean := filepath.Clean(filepath.Join(p.projectRoot, subpath))
	if !strings.HasPrefix(clean, p.projectRoot) {
		return "", fmt.Errorf("path traversal: %s", subpath)
	}
	return clean, nil
}

// atomicWrite writes data to a temp file then atomically renames it into place.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("write tempfile: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("atomic rename: %w", err)
	}
	return nil
}

// taskDir returns the task directory path for a given task ID.
func (p *Plugin) taskDir(taskID string) (string, error) {
	return p.safePath(filepath.Join("task_state", taskID))
}

// readTaskFile reads a raw file from a task directory.
func (p *Plugin) readTaskFile(taskID, filename string) ([]byte, error) {
	dir, err := p.taskDir(taskID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, filename)
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, p.projectRoot) {
		return nil, fmt.Errorf("path traversal: %s", filename)
	}
	return os.ReadFile(clean)
}

// writeTaskFile atomically writes a file in a task directory.
func (p *Plugin) writeTaskFile(taskID, filename string, data []byte, perm os.FileMode) error {
	dir, err := p.taskDir(taskID)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, filename)
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, p.projectRoot) {
		return fmt.Errorf("path traversal: %s", filename)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create task dir: %w", err)
	}
	return atomicWrite(clean, data, perm)
}

// parseStatusMD extracts fields from STATUS.md content.
func parseStatusMD(content []byte) TaskStatus {
	s := TaskStatus{}
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "**Phase:**") {
			s.Phase = strings.TrimSpace(strings.TrimPrefix(trimmed, "**Phase:**"))
		}
		if strings.HasPrefix(trimmed, "**Phase Updated:**") {
			s.PhaseUpdate = strings.TrimSpace(strings.TrimPrefix(trimmed, "**Phase Updated:**"))
		}
		if strings.HasPrefix(trimmed, "**Updated:**") {
			s.Updated = strings.TrimSpace(strings.TrimPrefix(trimmed, "**Updated:**"))
		}
	}
	if s.Phase == "" {
		s.Phase = "queued"
	}
	return s
}

// buildStatusMD creates STATUS.md content matching new-task.sh format.
func buildStatusMD(taskID, phase string, budget int) []byte {
	return []byte(fmt.Sprintf(
		"# Status — %s\n\n**Phase:** %s\n**Assigned:** none\n**Budget:** $%d\n**Spent:** $0\n**Created:** %s\n**Updated:** %s\n",
		taskID, phase, budget, TimestampDate(), TimestampDate(),
	))
}

// listDir returns filenames in dirPath. If limit > 0, caps the result.
// If reverse is true, sorts by name descending (newest-first for logs).
// Returns empty slice (not error) when the directory does not exist.
func listDir(dirPath string, limit int, reverse bool) ([]string, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if reverse {
		for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
			names[i], names[j] = names[j], names[i]
		}
	}
	if limit > 0 && len(names) > limit {
		names = names[:limit]
	}
	return names, nil
}

// patchStatusMD updates only the Phase, Phase Updated, and Updated lines in an
// existing STATUS.md, preserving all other content (review outcomes, history, etc.).
func patchStatusMD(existing []byte, status TaskStatus) []byte {
	if len(existing) == 0 {
		return buildStatusMD(status.Notes, status.Phase, 0)
	}
	lines := strings.Split(string(existing), "\n")
	var out []string
	foundPhase := false
	foundPhaseUpdated := false
	foundUpdated := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "**Phase:**") {
			out = append(out, "**Phase:** "+status.Phase)
			foundPhase = true
		} else if strings.HasPrefix(trimmed, "**Phase Updated:**") {
			if status.PhaseUpdate != "" {
				out = append(out, "**Phase Updated:** "+status.PhaseUpdate)
			}
			foundPhaseUpdated = true
		} else if strings.HasPrefix(trimmed, "**Updated:**") {
			if status.Updated != "" {
				out = append(out, "**Updated:** "+status.Updated)
			}
			foundUpdated = true
		} else {
			out = append(out, line)
		}
	}
	if !foundPhase {
		out = append(out, "**Phase:** "+status.Phase)
	}
	if !foundPhaseUpdated && status.PhaseUpdate != "" {
		out = append(out, "**Phase Updated:** "+status.PhaseUpdate)
	}
	if !foundUpdated && status.Updated != "" {
		out = append(out, "**Updated:** "+status.Updated)
	}
	return []byte(strings.Join(out, "\n"))
}

// readSessionJSON reads and parses session.json for a task.
func (p *Plugin) readSessionJSON(taskID string) (*SessionJSON, error) {
	data, err := p.readTaskFile(taskID, "session.json")
	if err != nil {
		return nil, err
	}
	var session SessionJSON
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("parse session.json: %w", err)
	}
	return &session, nil
}

// writeSessionJSON atomically writes session.json for a task.
func (p *Plugin) writeSessionJSON(taskID string, session *SessionJSON) error {
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session.json: %w", err)
	}
	data = append(data, '\n')
	return p.writeTaskFile(taskID, "session.json", data, 0644)
}
