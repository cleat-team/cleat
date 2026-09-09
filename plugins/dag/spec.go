package dag

import (
	"encoding/json"
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

	spec, err := parseDAGSpec(j)
	if err != nil {
		return nil, err
	}

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
func parseDAGSpec(j string) (DAGSpec, error) {
	tasks, err := parseTaskSpecs(j)
	if err != nil {
		return DAGSpec{}, err
	}
	return DAGSpec{
		Name:  ExtractJSONString(j, "name"),
		Tasks: tasks,
	}, nil
}

// parseTaskSpecs extracts the "tasks" array and parses each element.
//
// Returns an error rather than a best effort, because a spec field that is
// present and malformed is a validation failure and this is reached from
// `cleat dag validate`. Before cleat#1051 a bad `priority` bound 0 and the
// command printed "DAG spec is valid."
func parseTaskSpecs(j string) ([]TaskSpec, error) {
	arrContent := extractJSONArray(j, "tasks")
	if arrContent == "" {
		return nil, nil
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
		name := ExtractJSONString(obj, "name")
		priority, err := ExtractJSONInt(obj, "priority")
		if err != nil {
			return nil, fmt.Errorf("dag: task %q: %w", name, err)
		}
		specs = append(specs, TaskSpec{
			Name:        name,
			Fn:          ExtractJSONString(obj, "fn"),
			Parents:     parents,
			Priority:    priority,
			Description: ExtractJSONString(obj, "description"),
			Contract:    ExtractJSONString(obj, "contract"),
		})
	}
	return specs, nil
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

// ExtractJSONInt extracts an integer value from a JSON object field.
//
// An absent field is 0 with no error, which is the documented default for
// `priority`. A field that is PRESENT and not an integer is an error.
//
// cleat#1051: this was a hand-rolled digit scanner --
//
//	for _, c := range val {
//	    if c >= '0' && c <= '9' { n = n*10 + int(c-'0') } else { break }
//	}
//
// -- with no sign handling, no error return and no bounds check. It stopped at
// the first non-digit, so a leading `-` broke the loop immediately and the
// field bound 0 having consumed nothing. Measured on a real spec through
// `cleat dag validate`:
//
//	"priority": -5     -> 0     the defect
//	"priority": 10     -> 10
//	"priority": 7.9    -> 7     truncated silently
//	"priority": "3"    -> 0     quoted, silently zeroed
//
// and the command printed "DAG spec is valid." in every case.
//
// A negative priority is meaningful rather than malformed: the column is
// `priority INTEGER NOT NULL DEFAULT 0` with no CHECK and dispatch orders by
// `priority ASC`, so negative is how you put a task ahead of normal work
// without renumbering everything already at 0. The scanner made that
// unexpressible, and silently -- `Priority` is consumed as the child's real
// dispatch priority (dagrun.go) and for ordering within the DAG.
//
// Twin of the WASM-codegen defect fixed in cleat#1036, same function body in a
// different package.
func ExtractJSONInt(j, key string) (int, error) {
	val := extractJSONValue(j, key)
	if val == "" {
		return 0, nil
	}
	var n int
	if err := json.Unmarshal([]byte(val), &n); err != nil {
		return 0, fmt.Errorf("field %q: %w", key, err)
	}
	return n, nil
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
