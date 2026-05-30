package clewservice

import (
	"net/http"
	"os"
	"strings"
)

// handleLessonsGet lists lessons_learned files.
func (p *Plugin) handleLessonsGet(w http.ResponseWriter, r *http.Request) {
	lessonsDir, err := p.safePath("task_state/lessons_learned")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	entries, err := os.ReadDir(lessonsDir)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"lessons": []any{}})
			return
		}
		p.logger.Error("read lessons dir", "error", err)
		writeError(w, http.StatusInternalServerError, "read lessons dir: "+err.Error())
		return
	}

	type lessonEntry struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	var lessons []lessonEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		lessons = append(lessons, lessonEntry{
			Name: name,
			Path: "task_state/lessons_learned/" + name,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"lessons": lessons})
}

// handleContentGet serves static content files (task artifacts, plans, etc.).
func (p *Plugin) handleContentGet(w http.ResponseWriter, r *http.Request) {
	contentPath := r.PathValue("path")
	if contentPath == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}

	safePath, err := p.safePath(contentPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	data, err := os.ReadFile(safePath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not found: "+contentPath)
			return
		}
		writeError(w, http.StatusInternalServerError, "read file: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}
