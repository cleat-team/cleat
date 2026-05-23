package clewservice

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// readTasksJSON reads and parses the tasks.json file for a given project.
// If project is empty or "clew", reads from task_state/tasks.json.
// Otherwise reads from projects/<project>/tasks.json.
func (p *Plugin) readTasksJSON(project string) (*TasksJSON, error) {
	var path string
	if project == "" || project == "clew" {
		var err error
		path, err = p.safePath(filepath.Join("task_state", "tasks.json"))
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		path, err = p.safePath(filepath.Join("projects", project, "tasks.json"))
		if err != nil {
			return nil, err
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tasks.json: %w", err)
	}

	var tasks TasksJSON
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("parse tasks.json: %w", err)
	}
	return &tasks, nil
}

// writeTasksJSON atomically writes tasks.json.
func (p *Plugin) writeTasksJSON(project string, tasks *TasksJSON) error {
	var path string
	if project == "" || project == "clew" {
		var err error
		path, err = p.safePath(filepath.Join("task_state", "tasks.json"))
		if err != nil {
			return err
		}
	} else {
		var err error
		path, err = p.safePath(filepath.Join("projects", project, "tasks.json"))
		if err != nil {
			return err
		}
	}

	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tasks.json: %w", err)
	}
	// Ensure trailing newline.
	data = append(data, '\n')

	return atomicWrite(path, data, 0644)
}
