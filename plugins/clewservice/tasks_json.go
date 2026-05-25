package clewservice

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

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

	if tasks.Tasks == nil {
		tasks.Tasks = make(map[string]TaskEntry)
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
	data = append(data, '\n')

	return atomicWrite(path, data, 0644)
}
