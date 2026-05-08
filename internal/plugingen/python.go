package plugingen

import (
	"fmt"
	"strings"
)

// GeneratePython generates Python source code from the IR.
func GeneratePython(ir *IR) (string, error) {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("# Auto-generated from plugin manifest: %s v%s\n", ir.PluginName, ir.PluginVersion))
	buf.WriteString("# Do not edit by hand.\n\n")
	buf.WriteString("from __future__ import annotations\n\n")
	buf.WriteString("import json\n")
	buf.WriteString("from dataclasses import dataclass, field\n")
	buf.WriteString("from typing import Any, Optional\n\n")

	// Collect referenced types.
	refs := collectReferencedTypes(ir)

	// Generate dataclasses for referenced types.
	emitted := make(map[string]bool)
	for _, typ := range ir.Types {
		if refs[typ.Name] {
			buf.WriteString(generatePyDataclass(typ, ir, emitted))
			buf.WriteString("\n")
		}
	}
	// Emit any remaining types.
	for _, typ := range ir.Types {
		if !emitted[typ.Name] {
			buf.WriteString(generatePyDataclass(typ, ir, emitted))
			buf.WriteString("\n")
		}
	}

	// Generate the plugin class.
	className := toPascalCase(ir.PluginName) + "Plugin"
	classSnake := toSnakeCase(className)
	_ = classSnake

	buf.WriteString(fmt.Sprintf("\nclass %s:\n", className))
	buf.WriteString("    \"\"\"Auto-generated wrapper for the " + ir.PluginName + " plugin.\"\"\"\n\n")
	buf.WriteString("    def __init__(self, host_calls: Any) -> None:\n")
	buf.WriteString("        self._h = host_calls\n\n")

	for _, fn := range ir.HostFunctions {
		buf.WriteString(generatePyMethod(fn, ir))
		buf.WriteString("\n")
	}

	return buf.String(), nil
}

// generatePyDataclass generates a Python @dataclass for a TypeIR.
func generatePyDataclass(typ TypeIR, ir *IR, emitted map[string]bool) string {
	if emitted[typ.Name] {
		return ""
	}
	emitted[typ.Name] = true

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("@dataclass\nclass %s:\n", typ.Name))
	buf.WriteString("    \"\"\"Auto-generated type for " + typ.Name + ".\"\"\"\n\n")

	sortFields(typ.Fields)
	for _, f := range typ.Fields {
		pyFT := pyFieldType(f)
		defaultVal := getPyDefault(f)
		desc := ""
		if f.Description != "" {
			desc = fmt.Sprintf("  # %s", f.Description)
		}
		if defaultVal != "" {
			buf.WriteString(fmt.Sprintf("    %s: %s = %s%s\n", f.Name, pyFT, defaultVal, desc))
		} else {
			buf.WriteString(fmt.Sprintf("    %s: %s%s\n", f.Name, pyFT, desc))
		}
	}

	if len(typ.Fields) == 0 {
		buf.WriteString("    pass\n")
	}

	return buf.String()
}

// getPyDefault returns the default value expression for a Python field.
func getPyDefault(f FieldIR) string {
	if f.Optional {
		return "None"
	}
	if f.Type == "string" || f.Type == "timestamp" || f.Type == "uuid" {
		return `""`
	}
	if f.Type == "bool" {
		return "False"
	}
	if f.Type == "int64" || f.Type == "float64" {
		return "0"
	}
	if f.Type == "array" || f.IsArrayType() || f.ArrayLike() {
		return "field(default_factory=list)"
	}
	if isMapType(f) {
		return "field(default_factory=dict)"
	}
	return ""
}

// generatePyMethod generates a Python method for a HostFuncIR.
func generatePyMethod(fn HostFuncIR, ir *IR) string {
	var buf strings.Builder

	// Docstring.
	buf.WriteString(fmt.Sprintf("    def %s(self", fn.Name))
	if !isBuiltinType(fn.InputType) && fn.InputType != "" {
		buf.WriteString(fmt.Sprintf(", input: %s", fn.InputType))
	} else if fn.InputType != "" {
		buf.WriteString(fmt.Sprintf(", input: %s", pyType(fn.InputType)))
	} else {
		buf.WriteString(", input: Any = None")
	}
	buf.WriteString(") -> ")

	if isBuiltinType(fn.OutputType) && fn.OutputType != "" {
		buf.WriteString(pyType(fn.OutputType))
	} else if fn.OutputType != "" {
		buf.WriteString(fn.OutputType)
	} else {
		buf.WriteString("Any")
	}
	buf.WriteString(":\n")

	// Docstring body.
	if fn.Description != "" {
		buf.WriteString("        \"\"\"" + fn.Description + ".\"\"\"\n")
	} else {
		buf.WriteString("        \"\"\"Call the " + fn.Name + " host function.\"\"\"\n")
	}

	if fn.Streaming {
		buf.WriteString(fmt.Sprintf("        return self._h.plugin_call_streaming(%q, %q, input or {})\n", ir.PluginName, fn.Name))
	} else if isBuiltinType(fn.OutputType) && fn.OutputType != "" {
		buf.WriteString(fmt.Sprintf("        return self._h.plugin_call(%q, %q, input or {})\n", ir.PluginName, fn.Name))
	} else if fn.OutputType != "" {
		buf.WriteString(fmt.Sprintf("        response = self._h.plugin_call(%q, %q, input or {})\n", ir.PluginName, fn.Name))
		buf.WriteString("        data = json.loads(response) if isinstance(response, str) else response\n")
		buf.WriteString(fmt.Sprintf("        return %s(**data)\n", fn.OutputType))
	} else {
		buf.WriteString(fmt.Sprintf("        return self._h.plugin_call(%q, %q, input or {})\n", ir.PluginName, fn.Name))
	}

	return buf.String()
}

// IsArrayType is a method on FieldIR to check if it's an array type.
func (f FieldIR) IsArrayType() bool {
	return f.Type == "array" || f.ItemsType != ""
}

// ArrayLike returns true if this field looks like an array.
func (f FieldIR) ArrayLike() bool {
	return f.Type == "array" || f.ItemsType != ""
}
