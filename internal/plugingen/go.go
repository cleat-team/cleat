package plugingen

import (
	"fmt"
	"strings"
)

// GenerateGo generates Go source code from the IR.
func GenerateGo(ir *IR) (string, error) {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("// Auto-generated from plugin manifest: %s v%s\n", ir.PluginName, ir.PluginVersion))
	buf.WriteString("// Do not edit by hand.\n\n")
	buf.WriteString("package plugin\n\n")
	buf.WriteString("import \"encoding/json\"\n\n")

	// Collect referenced types.
	refs := collectReferencedTypes(ir)

	// Generate structs for referenced types.
	emitted := make(map[string]bool)
	for _, typ := range ir.Types {
		if refs[typ.Name] {
			buf.WriteString(generateGoStruct(typ, ir, emitted))
			buf.WriteString("\n")
		}
	}
	// Emit any remaining types.
	for _, typ := range ir.Types {
		if !emitted[typ.Name] {
			buf.WriteString(generateGoStruct(typ, ir, emitted))
			buf.WriteString("\n")
		}
	}

	// Generate the plugin struct.
	structName := toPascalCase(ir.PluginName) + "Plugin"
	buf.WriteString(fmt.Sprintf("// %s provides typed access to the %s plugin.\n", structName, ir.PluginName))
	buf.WriteString(fmt.Sprintf("type %s struct {\n", structName))
	buf.WriteString("    client pluginCaller\n")
	buf.WriteString("}\n\n")

	buf.WriteString(fmt.Sprintf("func New%s(client pluginCaller) *%s {\n", structName, structName))
	buf.WriteString(fmt.Sprintf("    return &%s{client: client}\n", structName))
	buf.WriteString("}\n\n")

	for _, fn := range ir.HostFunctions {
		buf.WriteString(generateGoMethod(fn, ir))
		buf.WriteString("\n")
	}

	return buf.String(), nil
}

// generateGoStruct generates a Go struct with JSON tags.
func generateGoStruct(typ TypeIR, ir *IR, emitted map[string]bool) string {
	if emitted[typ.Name] {
		return ""
	}
	emitted[typ.Name] = true

	var buf strings.Builder
	desc := typ.Name
	if len(typ.Fields) > 0 && typ.Fields[0].Description != "" {
		desc = typ.Fields[0].Description
	}
	buf.WriteString(fmt.Sprintf("// %s %s\n", typ.Name, desc))
	buf.WriteString(fmt.Sprintf("type %s struct {\n", typ.Name))

	sortFields(typ.Fields)
	for _, f := range typ.Fields {
		goFT := goFieldType(f)
		jsonTag := fmt.Sprintf("`json:\"%s", f.Name)
		if f.Optional {
			jsonTag += ",omitempty"
		}
		jsonTag += "\"`"
		desc := ""
		if f.Description != "" {
			desc = fmt.Sprintf(" // %s", f.Description)
		}
		// Capitalize field name.
		goName := toPascalCase(f.Name)
		if f.Optional {
			buf.WriteString(fmt.Sprintf("    %s %s %s%s\n", goName, goFT, jsonTag, desc))
		} else {
			buf.WriteString(fmt.Sprintf("    %s %s %s%s\n", goName, goFT, jsonTag, desc))
		}
	}

	buf.WriteString("}\n")
	return buf.String()
}

// generateGoMethod generates a Go method for a HostFuncIR.
func generateGoMethod(fn HostFuncIR, ir *IR) string {
	var buf strings.Builder

	desc := fn.Description
	if desc == "" {
		desc = fmt.Sprintf("Call the %s host function.", fn.Name)
	}

	// Method name in PascalCase.
	methodName := toPascalCase(fn.Name)

	// Input type.
	goInputType := "[]byte"
	if !isBuiltinType(fn.InputType) && fn.InputType != "" {
		goInputType = fmt.Sprintf("*%s", fn.InputType)
	} else if fn.InputType != "" {
		goInputType = goType(fn.InputType)
	}

	// Output type.
	goOutputType := "([]byte, error)"
	if !isBuiltinType(fn.OutputType) && fn.OutputType != "" {
		goOutputType = fmt.Sprintf("(*%s, error)", fn.OutputType)
	} else if fn.OutputType != "" {
		goOutputType = fmt.Sprintf("(%s, error)", goType(fn.OutputType))
	}

	if fn.Streaming {
		buf.WriteString(fmt.Sprintf("// %s %s (streaming)\n", methodName, desc))
		buf.WriteString(fmt.Sprintf("func (p *%s) %s(ctx context.Context, input %s) (<-chan StreamEvent, error) {\n",
			toPascalCase(ir.PluginName)+"Plugin", methodName, goInputType))
		buf.WriteString("    return p.client.PluginCallStreaming(ctx, ")
		buf.WriteString(fmt.Sprintf("%q, %q, input)\n", ir.PluginName, fn.Name))
		buf.WriteString("}\n")
	} else {
		buf.WriteString(fmt.Sprintf("// %s %s\n", methodName, desc))
		buf.WriteString(fmt.Sprintf("func (p *%s) %s(ctx context.Context, input %s) %s {\n",
			toPascalCase(ir.PluginName)+"Plugin", methodName, goInputType, goOutputType))

		if !isBuiltinType(fn.OutputType) && fn.OutputType != "" {
			buf.WriteString(fmt.Sprintf("    resp, err := p.client.PluginCall(ctx, %q, %q, input)\n", ir.PluginName, fn.Name))
			buf.WriteString("    if err != nil {\n")
			buf.WriteString("        return nil, err\n")
			buf.WriteString("    }\n")
			buf.WriteString(fmt.Sprintf("    var out %s\n", fn.OutputType))
			buf.WriteString("    if err := json.Unmarshal(resp, &out); err != nil {\n")
			buf.WriteString("        return nil, err\n")
			buf.WriteString("    }\n")
			buf.WriteString("    return &out, nil\n")
		} else {
			buf.WriteString(fmt.Sprintf("    return p.client.PluginCall(ctx, %q, %q, input)\n", ir.PluginName, fn.Name))
		}
		buf.WriteString("}\n")
	}

	return buf.String()
}
