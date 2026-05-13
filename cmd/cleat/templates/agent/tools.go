//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/cleat-team/cleat/cleat"
)

// ---- Tool definitions ----

// getTools returns the tools available to the AI agent.
func getTools() []Tool {
	return []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "calculator",
				Description: "Evaluate a mathematical expression. Supports +, -, *, /, sqrt, power.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"expression": map[string]any{
							"type":        "string",
							"description": "Math expression to evaluate",
						},
					},
					"required": []string{"expression"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "web_search",
				Description: "Search the web for information. Returns relevant snippets.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "Search query",
						},
					},
					"required": []string{"query"},
				},
			},
		},
	}
}

// executeTool runs a tool and returns its result as a JSON string.
func executeTool(h cleat.HostCalls, name, argsJSON string) string {
	switch name {
	case "calculator":
		var args struct{ Expression string }
		json.Unmarshal([]byte(argsJSON), &args)
		result := evaluateExpression(args.Expression)
		r, _ := json.Marshal(map[string]string{"result": result})
		return string(r)

	case "web_search":
		var args struct{ Query string }
		json.Unmarshal([]byte(argsJSON), &args)
		result := fmt.Sprintf("Web search for '%s' would execute here via a cleat HTTP call to a search API.", args.Query)
		r, _ := json.Marshal(map[string]any{"results": []string{result}})
		return string(r)

	default:
		r, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("unknown tool: %s", name)})
		return string(r)
	}
}

// evaluateExpression evaluates simple mathematical expressions safely.
func evaluateExpression(expr string) string {
	expr = strings.TrimSpace(expr)

	// sqrt(x)
	if strings.HasPrefix(expr, "sqrt(") && strings.HasSuffix(expr, ")") {
		inner := expr[5 : len(expr)-1]
		if val, err := strconv.ParseFloat(strings.TrimSpace(inner), 64); err == nil {
			return fmt.Sprintf("%.4f", math.Sqrt(val))
		}
	}

	// power(x, y)
	if strings.HasPrefix(expr, "power(") && strings.HasSuffix(expr, ")") {
		inner := expr[6 : len(expr)-1]
		parts := strings.SplitN(inner, ",", 2)
		if len(parts) == 2 {
			x, _ := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
			y, _ := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
			return fmt.Sprintf("%.4f", math.Pow(x, y))
		}
	}

	// Basic arithmetic: a + b, a - b, a * b, a / b
	for _, op := range []string{"+", "-", "*", "/"} {
		if parts := strings.SplitN(expr, op, 2); len(parts) == 2 {
			a, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
			b, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
			if err1 == nil && err2 == nil {
				switch op {
				case "+":
					return fmt.Sprintf("%.4f", a+b)
				case "-":
					return fmt.Sprintf("%.4f", a-b)
				case "*":
					return fmt.Sprintf("%.4f", a*b)
				case "/":
					if b != 0 {
						return fmt.Sprintf("%.4f", a/b)
					}
				}
			}
		}
	}

	return expr
}
