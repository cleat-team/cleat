package plugingen

import (
	"fmt"
	"strings"
)

// GenerateRust generates Rust source code from the IR.
func GenerateRust(ir *IR) (string, error) {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("// Auto-generated from plugin manifest: %s v%s\n", ir.PluginName, ir.PluginVersion))
	buf.WriteString("// Do not edit by hand.\n\n")
	buf.WriteString("use serde::{Deserialize, Serialize};\n\n")

	// Collect referenced types.
	refs := collectReferencedTypes(ir)

	// Generate structs for referenced types.
	emitted := make(map[string]bool)
	for _, typ := range ir.Types {
		if refs[typ.Name] {
			buf.WriteString(generateRustStruct(typ, ir, emitted))
			buf.WriteString("\n")
		}
	}
	// Emit any remaining types.
	for _, typ := range ir.Types {
		if !emitted[typ.Name] {
			buf.WriteString(generateRustStruct(typ, ir, emitted))
			buf.WriteString("\n")
		}
	}

	// Generate the plugin struct.
	structName := toPascalCase(ir.PluginName) + "Plugin"
	buf.WriteString(fmt.Sprintf("pub struct %s {\n", structName))
	buf.WriteString("    host_calls: crate::HostCalls,\n")
	buf.WriteString("}\n\n")

	buf.WriteString(fmt.Sprintf("impl %s {\n", structName))
	buf.WriteString("    pub fn new(host_calls: crate::HostCalls) -> Self {\n")
	buf.WriteString("        Self { host_calls }\n")
	buf.WriteString("    }\n\n")

	for _, fn := range ir.HostFunctions {
		buf.WriteString(generateRustMethod(fn, ir))
		buf.WriteString("\n")
	}

	buf.WriteString("}\n")
	return buf.String(), nil
}

// generateRustStruct generates a Rust struct with Serialize/Deserialize derives.
func generateRustStruct(typ TypeIR, ir *IR, emitted map[string]bool) string {
	if emitted[typ.Name] {
		return ""
	}
	emitted[typ.Name] = true

	var buf strings.Builder
	desc := ""
	if len(typ.Fields) > 0 {
		firstField := typ.Fields[0]
		if firstField.Description != "" {
			desc = fmt.Sprintf("/// %s\n", firstField.Description)
			// Only use first field's description as a generic description.
		}
	}
	buf.WriteString(fmt.Sprintf("%s#[derive(Debug, Clone, Serialize, Deserialize)]\n", desc))
	buf.WriteString(fmt.Sprintf("pub struct %s {\n", typ.Name))

	sortFields(typ.Fields)
	for _, f := range typ.Fields {
		rustFT := rustFieldType(f)
		serdeAttr := getRustSerdeAttr(f)
		desc := ""
		if f.Description != "" {
			desc = fmt.Sprintf(" // %s", f.Description)
		}
		if serdeAttr != "" {
			buf.WriteString(fmt.Sprintf("    %s\n", serdeAttr))
		}
		if f.Optional {
			buf.WriteString(fmt.Sprintf("    pub %s: Option<%s>,%s\n", f.Name, rustFT, desc))
		} else {
			buf.WriteString(fmt.Sprintf("    pub %s: %s,%s\n", f.Name, rustFT, desc))
		}
	}

	buf.WriteString("}\n")
	return buf.String()
}

// getRustSerdeAttr returns serde attributes for a field.
func getRustSerdeAttr(f FieldIR) string {
	if f.Optional {
		return "    #[serde(default)]"
	}
	return ""
}

// generateRustMethod generates a Rust method for a HostFuncIR.
func generateRustMethod(fn HostFuncIR, ir *IR) string {
	var buf strings.Builder

	// Doc comment.
	desc := fn.Description
	if desc == "" {
		desc = fmt.Sprintf("Call the %s host function.", fn.Name)
	}
	buf.WriteString(fmt.Sprintf("    /// %s\n", desc))

	// Input type.
	inputRef := ""
	if isBuiltinType(fn.InputType) || fn.InputType == "" {
		inputRef = "&str"
	} else {
		inputRef = fmt.Sprintf("&%s", fn.InputType)
	}

	// Output type.
	outputRef := ""
	if isBuiltinType(fn.OutputType) || fn.OutputType == "" {
		outputRef = "String"
	} else {
		outputRef = fn.OutputType
	}

	if fn.Streaming {
		buf.WriteString(fmt.Sprintf("    pub async fn %s(&self, input: %s) -> Result<impl futures::Stream<Item = crate::StreamEvent>, String> {\n", fn.Name, inputRef))
		buf.WriteString("        unimplemented!(\"streaming not yet generated\")\n")
		buf.WriteString("    }\n")
	} else {
		buf.WriteString(fmt.Sprintf("    pub async fn %s(&self, input: %s) -> Result<%s, String> {\n", fn.Name, inputRef, outputRef))
		buf.WriteString(fmt.Sprintf("        let resp = self.host_calls.plugin_call(%q, %q, input).await?;\n", ir.PluginName, fn.Name))
		if !isBuiltinType(fn.OutputType) && fn.OutputType != "" {
			buf.WriteString("        serde_json::from_str(&resp).map_err(|e| format!(\"deserialize error: {}\", e))\n")
		} else {
			buf.WriteString("        Ok(resp)\n")
		}
		buf.WriteString("    }\n")
	}

	return buf.String()
}
