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
	Name        string   `json:"name"`
	Fn          string   `json:"fn"`
	Parents     []string `json:"parents,omitempty"`
	Priority    int      `json:"priority,omitempty"`
	Description string   `json:"description,omitempty"`
	Contract    string   `json:"contract,omitempty"`
}

// ParseSpec decodes a JSON DAG spec and structurally validates it: valid
// JSON, at least one task, no duplicate task names, and every parent
// reference names a declared task.
//
// It does not resolve task functions or build a runtime DAG -- this
// package is host-side (used by `cleat dag validate` and code generation,
// neither of which needs the SDK). Function resolution and runtime DAG
// construction are guest-side, in cleat/dagrun.LoadFromJSON, which calls
// ParseSpec for this half and adds its own registry check on top. See this
// package's doc comment for the full split and why.
func ParseSpec(r io.Reader) (*DAGSpec, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("dag: read spec: %w", err)
	}
	j := strings.TrimSpace(string(raw))
	if len(j) == 0 || j[0] != '{' {
		return nil, fmt.Errorf("dag: decode spec: invalid JSON (v2)")
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

	// Validate: all parents reference declared task names.
	for _, ts := range spec.Tasks {
		for _, parent := range ts.Parents {
			if !seen[parent] {
				return nil, fmt.Errorf("dag: task %q references unknown parent %q", ts.Name, parent)
			}
		}
	}

	return &spec, nil
}

// parseDAGSpec parses a JSON DAG spec string into a DAGSpec.
func parseDAGSpec(j string) DAGSpec {
	return DAGSpec{
		Name:  ExtractJSONString(j, "name"),
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
			Name:        ExtractJSONString(obj, "name"),
			Fn:          ExtractJSONString(obj, "fn"),
			Parents:     parents,
			Priority:    ExtractJSONInt(obj, "priority"),
			Description: ExtractJSONString(obj, "description"),
			Contract:    ExtractJSONString(obj, "contract"),
		})
	}
	return specs
}

// ExtractJSONString extracts a quoted string value from a JSON object field.
// Unescapes \" and \\ escape sequences in the string.
func ExtractJSONString(j, key string) string {
	val := extractJSONValue(j, key)
	if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
		s := val[1 : len(val)-1]
		// Unescape \" and \\.
		var buf []byte
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					buf = append(buf, '\n')
				case 't':
					buf = append(buf, '\t')
				case 'r':
					buf = append(buf, '\r')
				case '"':
					buf = append(buf, '"')
				case '\\':
					buf = append(buf, '\\')
				case '/':
					buf = append(buf, '/')
				default:
					buf = append(buf, s[i])
				}
				continue
			}
			buf = append(buf, s[i])
		}
		return string(buf)
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
func ExtractJSONInt(j, key string) int {
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
		// Find the closing quote, skipping \" escape sequences.
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\\' && i+1 < len(rest) {
				i++ // skip escaped character
				continue
			}
			if rest[i] == '"' {
				return rest[:i+1]
			}
		}
		return ""
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
// Skips characters inside JSON strings to avoid false matches on braces in string values.
func findClosing(s string, open, close byte) int {
	depth := 0
	inString := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if c == '\\' {
				i++ // skip escaped character
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		if c == open {
			depth++
		} else if c == close {
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
	result := make([]string, 0, 8)
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
	result := make([]string, 0, 8)
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
