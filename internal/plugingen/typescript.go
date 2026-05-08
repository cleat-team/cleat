package plugingen

import (
	"fmt"
	"sort"
	"strings"
)

// GenerateTypeScript generates TypeScript source code from the IR.
func GenerateTypeScript(ir *IR) (string, error) {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("// Auto-generated from plugin manifest: %s v%s\n", ir.PluginName, ir.PluginVersion))
	buf.WriteString("// Do not edit by hand.\n\n")

	// Collect referenced types to decide what to emit.
	refs := collectReferencedTypes(ir)

	// Emit type interfaces (only those referenced by host functions, plus
	// transitive dependencies).
	emitted := make(map[string]bool)
	for _, typ := range ir.Types {
		if refs[typ.Name] {
			buf.WriteString(generateTSInterface(typ, ir, emitted))
			buf.WriteString("\n")
		}
	}

	// Emit any non-referenced types too (not just refs), filtering to avoid
	// synthetic function-specific types that were already emitted.
	for _, typ := range ir.Types {
		if !emitted[typ.Name] {
			buf.WriteString(generateTSInterface(typ, ir, emitted))
			buf.WriteString("\n")
		}
	}

	// Generate the plugin class.
	className := toPascalCase(ir.PluginName) + "Plugin"
	buf.WriteString(fmt.Sprintf("export class %s {\n", className))
	buf.WriteString("  constructor(private hostCalls: HostCalls) {}\n\n")

	for _, fn := range ir.HostFunctions {
		buf.WriteString(generateTSMethod(fn, ir))
		buf.WriteString("\n")
	}

	buf.WriteString("}\n")
	return buf.String(), nil
}

// generateTSInterface generates a TypeScript interface for a TypeIR.
func generateTSInterface(typ TypeIR, ir *IR, emitted map[string]bool) string {
	if emitted[typ.Name] {
		return ""
	}
	emitted[typ.Name] = true

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("export interface %s {\n", typ.Name))
	for _, f := range typ.Fields {
		opt := ""
		if f.Optional {
			opt = "?"
		}
		ft := tsFieldType(f, ir)
		desc := ""
		if f.Description != "" {
			desc = fmt.Sprintf(" // %s", f.Description)
		}
		buf.WriteString(fmt.Sprintf("  %s%s: %s;%s\n", f.Name, opt, ft, desc))
	}
	buf.WriteString("}\n")
	return buf.String()
}

// generateTSMethod generates a TypeScript method for a HostFuncIR.
func generateTSMethod(fn HostFuncIR, ir *IR) string {
	var buf strings.Builder

	// JSDoc comment.
	hasDescription := fn.Description != ""
	hasInput := fn.InputType != "" && !isBuiltinType(fn.InputType)
	hasOutput := fn.OutputType != "" && !isBuiltinType(fn.OutputType)

	if hasDescription || hasInput || hasOutput {
		buf.WriteString("  /**\n")
		if hasDescription {
			buf.WriteString(fmt.Sprintf("   * %s\n", fn.Description))
		}
		if hasInput {
			buf.WriteString(fmt.Sprintf("   * @param input - Input of type %s\n", fn.InputType))
		}
		if hasOutput {
			buf.WriteString(fmt.Sprintf("   * @returns Promise resolving to %s\n", fn.OutputType))
		}
		buf.WriteString("   */\n")
	}

	// Determine input type annotation.
	inputType := "any"
	if isBuiltinType(fn.InputType) {
		if fn.InputType != "" {
			inputType = tsType(fn.InputType, ir)
		}
	} else {
		inputType = fn.InputType
	}

	// Determine output type annotation.
	outputType := "any"
	if isBuiltinType(fn.OutputType) {
		if fn.OutputType != "" {
			outputType = tsType(fn.OutputType, ir)
		}
	} else {
		outputType = fn.OutputType
	}

	if fn.Streaming {
		// Streaming method yields events.
		buf.WriteString(fmt.Sprintf("  async %s(input: %s): Promise<AsyncIterableIterator<any>> {\n", fn.Name, inputType))
		buf.WriteString(fmt.Sprintf("    return this.hostCalls.pluginCallStreaming(%q, %q, input);\n", ir.PluginName, fn.Name))
		buf.WriteString("  }\n")
	} else {
		buf.WriteString(fmt.Sprintf("  async %s(input: %s): Promise<%s> {\n", fn.Name, inputType, outputType))
		buf.WriteString(fmt.Sprintf("    const response = await this.hostCalls.pluginCall(%q, %q, input);\n", ir.PluginName, fn.Name))
		buf.WriteString("    return JSON.parse(response);\n")
		buf.WriteString("  }\n")
	}

	return buf.String()
}

// tsFieldType returns the TypeScript type annotation for a FieldIR.
func tsFieldType(fir FieldIR, ir *IR) string {
	if fir.Type == "array" || fir.Type == "" && fir.ItemsType != "" {
		itemsType := "any"
		if fir.ItemsType != "" {
			if isSimpleType(fir.ItemsType) {
				itemsType = tsType(fir.ItemsType, ir)
			} else {
				itemsType = fir.ItemsType
			}
		}
		return itemsType + "[]"
	}
	if isMapType(fir) {
		valType := "any"
		if fir.ValueType != "" {
			valType = tsType(fir.ValueType, ir)
		}
		return fmt.Sprintf("Record<string, %s>", valType)
	}
	if fir.Nested != nil {
		// Inline object.
		return "Record<string, any>"
	}
	if isSimpleType(fir.Type) {
		return tsType(fir.Type, ir)
	}
	// Might be a named type or other.
	if fir.Type != "" {
		return fir.Type
	}
	return "any"
}

// sortFields sorts fields by name for deterministic output.
func sortFields(fields []FieldIR) {
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].Name < fields[j].Name
	})
}
