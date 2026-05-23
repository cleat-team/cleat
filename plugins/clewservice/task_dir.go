package clewservice

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// readTaskFile reads a raw file from a task directory.
func (p *Plugin) readTaskFile(taskID, filename string) ([]byte, error) {
	dir, err := p.taskDir(taskID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, filename)
	// Re-validate the final path.
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
	// Ensure directory exists.
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

// buildStatusMD creates STATUS.md content for a given phase.
func buildStatusMD(status TaskStatus) []byte {
	content := fmt.Sprintf("# Status — %s\n\n**Phase:** %s\n", status.Notes, status.Phase)
	if status.PhaseUpdate != "" {
		content += fmt.Sprintf("**Phase Updated:** %s\n", status.PhaseUpdate)
	}
	return []byte(content)
}

// patchStatusMD updates only the Phase, Phase Updated, and Updated lines in an
// existing STATUS.md, preserving all other content (review outcomes, history, etc.).
func patchStatusMD(existing []byte, status TaskStatus) []byte {
	if len(existing) == 0 {
		return buildStatusMD(status)
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

// listDirFiles returns sorted file names in a task subdirectory.
func (p *Plugin) listDirFiles(taskID, subdir string) ([]string, error) {
	dir, err := p.taskDir(taskID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, subdir)
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, p.projectRoot) {
		return nil, fmt.Errorf("path traversal: %s", subdir)
	}

	entries, err := os.ReadDir(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}
