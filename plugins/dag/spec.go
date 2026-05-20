package dag

import (
	"fmt"
	"io"
	"strings"
)

// DAGSpec is a JSON-serializable DAG specification.
type DAGSpec struct {
	Name  string     `json:"name"`
	Tasks []TaskSpec `json:"tasks"`
}

// TaskSpec describes a single task in a DAG spec.
type TaskSpec struct {
	Name     string   `json:"name"`
	Fn       string   `json:"fn"`
	Parents  []string `json:"parents,omitempty"`
	Priority int      `json:"priority,omitempty"`
}

// TaskFunc is the signature for a DAG task function.
type TaskFunc func(ctx *TaskContext) (string, error)

// LoadFromJSON decodes a JSON DAG spec and constructs a *DAG.
func LoadFromJSON(r io.Reader, registry map[string]TaskFunc) (*DAG, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("dag: read spec: %w", err)
	}
	j := strings.TrimSpace(string(raw))
	if len(j) == 0 || j[0] != '{' {
		return nil, fmt.Errorf("dag: decode spec: invalid JSON")
	}
	// Quick validation: check balanced top-level braces.
	if findClosing(j, '{', '}') < 0 {
		return nil, fmt.Errorf("dag: decode spec: invalid JSON")
	}

	spec := parseDAGSpec(j)

	if len(spec.Tasks) == 0 {
		return nil, fmt.Errorf("dag: spec has no tasks")
	}

	// Validate: no duplicate task names.
	seen := make(map[string]bool)
	for _, ts := range spec.Tasks {
		if seen[ts.Name] {
			return nil, fmt.Errorf("dag: duplicate task name %q", ts.Name)
		}
		seen[ts.Name] = true
	}

	// Validate: all fn values exist in the registry.
	if registry != nil {
		for _, ts := range spec.Tasks {
			if _, ok := registry[ts.Fn]; !ok {
				return nil, fmt.Errorf("dag: task %q references unknown function %q", ts.Name, ts.Fn)
			}
		}
	}

	// Validate: all parents reference declared task names.
	for _, ts := range spec.Tasks {
		for _, parent := range ts.Parents {
			if !seen[parent] {
				return nil, fmt.Errorf("dag: task %q references unknown parent %q", ts.Name, parent)
			}
		}
	}

	// Build the DAG.
	d := NewDAG()
	for _, ts := range spec.Tasks {
		var fn func(ctx *TaskContext) (string, error)
		if registry != nil {
			if f, ok := registry[ts.Fn]; ok {
				fn = f
			}
		}
		d.AddTask(ts.Name, ts.Parents, fn, ts.Priority)
		if ts.Fn != "" {
			d.tasks[ts.Name].WorkflowName = ts.Fn
		}
	}

	return d, nil
}

// parseDAGSpec parses a JSON DAG spec string into a DAGSpec.
func parseDAGSpec(j string) DAGSpec {
	return DAGSpec{
		Name:  extractJSONString(j, "name"),
		Tasks: parseTaskSpecs(j),
	}
}

// parseTaskSpecs extracts the "tasks" array and parses each element.
func parseTaskSpecs(j string) []TaskSpec {
	arrContent := extractJSONArray(j, "tasks")
	if arrContent == "" {
		return nil
	}

	// Strip outer brackets before splitting objects.
	if len(arrContent) >= 2 && arrContent[0] == '[' {
		arrContent = arrContent[1 : len(arrContent)-1]
	}
	objs := splitJSONObjects(arrContent)
	specs := make([]TaskSpec, 0, len(objs))
	for _, obj := range objs {
		parentsRaw := extractJSONValue(obj, "parents")
		var parents []string
		if parentsRaw != "" && len(parentsRaw) > 0 && parentsRaw[0] == '[' {
			parents = splitJSONStringArray(parentsRaw)
		}
		specs = append(specs, TaskSpec{
			Name:     extractJSONString(obj, "name"),
			Fn:       extractJSONString(obj, "fn"),
			Parents:  parents,
			Priority: extractJSONInt(obj, "priority"),
		})
	}
	return specs
}

// extractJSONString extracts a quoted string value from a JSON object field.
func extractJSONString(j, key string) string {
	val := extractJSONValue(j, key)
	if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
		return val[1 : len(val)-1]
	}
	return val
}

// extractJSONArray extracts an array value from a JSON object field.
func extractJSONArray(j, key string) string {
	val := extractJSONValue(j, key)
	if len(val) > 0 && val[0] == '[' {
		return val
	}
	return ""
}

// extractJSONInt extracts an integer value from a JSON object field.
func extractJSONInt(j, key string) int {
	val := extractJSONValue(j, key)
	if val == "" {
		return 0
	}
	n := 0
	for _, c := range val {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			break
		}
	}
	return n
}

// extractJSONValue extracts the raw value of a field from a JSON object.
func extractJSONValue(j, key string) string {
	search := `"` + key + `":`
	idx := indexOfStr(j, search)
	if idx < 0 {
		return ""
	}
	rest := j[idx+len(search):]
	rest = strings.TrimLeft(rest, " \t\n\r")
	if len(rest) == 0 {
		return ""
	}

	switch rest[0] {
	case '"':
		end := indexOfStr(rest[1:], `"`)
		if end < 0 {
			return ""
		}
		return rest[:end+2]
	case '[':
		close := findClosing(rest, '[', ']')
		if close < 0 {
			return ""
		}
		return rest[:close+1]
	case '{':
		close := findClosing(rest, '{', '}')
		if close < 0 {
			return ""
		}
		return rest[:close+1]
	default:
		end := 0
		for end < len(rest) && rest[end] != ',' && rest[end] != '}' && rest[end] != '\n' && rest[end] != '\r' {
			end++
		}
		return strings.TrimSpace(rest[:end])
	}
}

// findClosing finds the matching closing bracket for the opening bracket at position 0.
func findClosing(s string, open, close byte) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		if s[i] == open {
			depth++
		} else if s[i] == close {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// splitJSONObjects splits a JSON array body like {...},{...} into individual object strings.
func splitJSONObjects(s string) []string {
	var result []string
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '{' {
			end := findClosing(s[i:], '{', '}')
			if end < 0 {
				break
			}
			result = append(result, s[i:i+end+1])
			i += end + 1
		} else {
			break
		}
	}
	return result
}

// splitJSONStringArray splits a JSON array body like "a","b","c" or ["a","b","c"] into individual strings.
func splitJSONStringArray(s string) []string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		s = s[1 : len(s)-1]
	}
	var result []string
	parts := strings.Split(s, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
			result = append(result, p[1:len(p)-1])
		} else if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func indexOfStr(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
