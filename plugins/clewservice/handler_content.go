package clewservice

import (
	"net/http"
	"os"
	"path/filepath"
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

// handleContentGet serves markdown files from task_state/ or projects/.
// GET /api/content/{path...}
func (p *Plugin) handleContentGet(w http.ResponseWriter, r *http.Request) {
	subpath := r.PathValue("path")
	if subpath == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}

	// Only .md files.
	if !strings.HasSuffix(subpath, ".md") {
		writeError(w, http.StatusBadRequest, "only .md files are supported")
		return
	}

	// Resolve the full path with traversal protection.
	var resolved string
	var err error
	for _, prefix := range []string{"task_state", "projects"} {
		resolved, err = p.safePath(filepath.Join(prefix, subpath))
		if err != nil {
			writeError(w, http.StatusForbidden, "path traversal denied")
			return
		}
		if _, statErr := os.Stat(resolved); statErr == nil {
			break
		}
		resolved = ""
	}

	if resolved == "" {
		writeError(w, http.StatusNotFound, "not found: "+subpath)
		return
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not found: "+subpath)
			return
		}
		p.logger.Error("read content file", "path", resolved, "error", err)
		writeError(w, http.StatusInternalServerError, "read file: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}
