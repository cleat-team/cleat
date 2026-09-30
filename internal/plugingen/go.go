package plugingen

import (
	"fmt"
	"go/format"
	"strings"
	"unicode"
)

// GenerateGo generates Go source implementing plugin.FuncRegistry
// registration/dispatch code for a first-party Go plugin -- the boilerplate
// docs/contributor/plugins/plugin-migration-guide.md's Go path promises to
// replace (hand-written JSON parsing and registration inside
// RegisterHostFunctions). cleat#2655: this used to generate a CALLER-side
// client (a struct wrapping plugin calls OUT to a plugin), which is a
// different job than the guide describes, matches no real interface in the
// codebase, and does not compile on its own terms (undefined pluginCaller
// type, missing "context" import, a []byte/string mismatch against the
// real PluginCall). This generates the callee/registration side instead,
// matching plugin.FuncOptions/FuncRegistry/StreamFuncRegistry
// (plugin/plugin.go) and the shape every hand-written plugin already uses
// -- see plugins/llm/host_functions.go, which this mirrors for the
// streaming type-assertion.
//
// The plugin author still writes the business logic: for host function
// "do_thing", the generated code calls p.doThing(ctx, input) and the
// author implements that method on the Plugin struct. A host function with
// no declared InputType/OutputType in the manifest is passed through with
// no generated wrapper at all -- p.<method> is registered directly, since
// its signature already matches plugin.PluginFunc exactly, the same as
// llm's hand-written p.chat.
func GenerateGo(ir *IR) (string, error) {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("// Auto-generated from plugin manifest: %s v%s\n", ir.PluginName, ir.PluginVersion))
	buf.WriteString("// Do not edit by hand.\n\n")
	buf.WriteString(fmt.Sprintf("package %s\n\n", goPackageName(ir.PluginName)))

	// "context" and "encoding/json"/"fmt" are only used inside a generated
	// dispatch WRAPPER -- a host function whose input and output (input
	// only, for streaming) are both untyped registers its business method
	// directly with no wrapper at all (see generateGoRegisterCall), and
	// writes neither identifier anywhere. Importing them unconditionally,
	// as this used to, produces "imported and not used" on exactly that
	// case -- caught by assertGoCompiles on an empty-manifest and an
	// all-untyped-function fixture, neither of which any substring-only
	// test could see.
	needsWrapperImports := false
	for _, fn := range ir.HostFunctions {
		_, inRaw := goHandlerType(fn.InputType)
		if fn.Streaming {
			if !inRaw {
				needsWrapperImports = true
			}
			continue
		}
		_, outRaw := goHandlerType(fn.OutputType)
		if !inRaw || !outRaw {
			needsWrapperImports = true
		}
	}

	buf.WriteString("import (\n")
	if needsWrapperImports {
		buf.WriteString("\t\"context\"\n")
		buf.WriteString("\t\"encoding/json\"\n")
		buf.WriteString("\t\"fmt\"\n\n")
	}
	buf.WriteString("\t\"github.com/cleat-team/cleat/plugin\"\n")
	buf.WriteString(")\n\n")

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

	buf.WriteString(fmt.Sprintf("// RegisterHostFunctions registers %s's workflow-callable functions.\n", ir.PluginName))
	buf.WriteString("// Generated code handles registration, JSON parsing, and marshaling; the business\n")
	buf.WriteString("// logic lives in the plain methods it calls, named after each host function.\n")
	buf.WriteString("func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {\n")

	var streaming []HostFuncIR
	for _, fn := range ir.HostFunctions {
		if fn.Streaming {
			streaming = append(streaming, fn)
			continue
		}
		buf.WriteString(generateGoRegisterCall(fn, ir))
	}

	if len(streaming) > 0 {
		buf.WriteString("\tif streamScope, ok := scope.(plugin.StreamFuncRegistry); ok {\n")
		for _, fn := range streaming {
			buf.WriteString(generateGoRegisterStreamCall(fn, ir))
		}
		buf.WriteString("\t}\n")
	}

	buf.WriteString("\treturn nil\n")
	buf.WriteString("}\n")

	// gofmt the output rather than hand-formatting every WriteString call
	// consistently (struct fields used 4 spaces, function bodies used tabs,
	// pre-existing before this rewrite) -- go/format.Source both formats
	// and is itself a parse check, catching a malformed literal before a
	// caller ever writes it to disk.
	formatted, err := format.Source([]byte(buf.String()))
	if err != nil {
		return "", fmt.Errorf("plugingen: generated Go source does not parse: %w", err)
	}
	return string(formatted), nil
}

// goPackageName derives the plugin's own Go package name from its manifest
// name, the same way every first-party plugin directory already does it --
// plugins/slacknotify/ for "slack-notify", plugins/scheduledbackup/ for
// "scheduled-backup" (CLAUDE.md's "Plugin development" section: plugin
// names are hyphenated, and the package name is not).
func goPackageName(pluginName string) string {
	return strings.ToLower(strings.ReplaceAll(pluginName, "-", ""))
}

// goHandlerType maps one side (input or output) of a host function's wire
// type to the Go type the author-implemented business method uses, and
// reports whether that side is RAW: no type was declared in the manifest,
// so the generated wrapper passes the JSON string straight through rather
// than unmarshaling/marshaling it. Raw is a plain "string", never "[]byte"
// -- plugin.PluginFunc's own inputJSON/result are both string
// (plugin/plugin.go), which is the mismatch cleat#2655 reported against
// the old generator's assumption.
func goHandlerType(t string) (goT string, raw bool) {
	if t == "" {
		return "string", true
	}
	if isBuiltinType(t) {
		return goType(t), false
	}
	return t, false
}

// generateGoRegisterCall generates one scope.Register call for a
// non-streaming host function, matching plugin.FuncRegistry.Register's
// signature (plugin.FuncOptions, plugin.PluginFunc).
func generateGoRegisterCall(fn HostFuncIR, ir *IR) string {
	var buf strings.Builder

	methodName := toPascalCase(fn.Name)
	camelName := toCamelCase(fn.Name)
	inType, inRaw := goHandlerType(fn.InputType)
	_, outRaw := goHandlerType(fn.OutputType)
	opts := goFuncOptions(fn)

	if inRaw && outRaw {
		// The business method's own signature already matches
		// plugin.PluginFunc exactly -- no wrapper needed, the same as
		// llm's hand-written p.chat.
		buf.WriteString(fmt.Sprintf("\tif err := scope.Register(%s, p.%s); err != nil {\n", opts, camelName))
		buf.WriteString("\t\treturn err\n")
		buf.WriteString("\t}\n")
		return buf.String()
	}

	if fn.Description != "" {
		buf.WriteString(fmt.Sprintf("\t// %s %s\n", methodName, fn.Description))
	}
	buf.WriteString(fmt.Sprintf("\tif err := scope.Register(%s, func(ctx context.Context, inputJSON string) (string, error) {\n", opts))

	callArg := "inputJSON"
	if !inRaw {
		buf.WriteString(fmt.Sprintf("\t\tvar input %s\n", inType))
		buf.WriteString("\t\tif err := json.Unmarshal([]byte(inputJSON), &input); err != nil {\n")
		buf.WriteString(fmt.Sprintf("\t\t\treturn \"\", fmt.Errorf(%q, err)\n", ir.PluginName+"."+fn.Name+": invalid input: %w"))
		buf.WriteString("\t\t}\n")
		callArg = "input"
	}

	if outRaw {
		buf.WriteString(fmt.Sprintf("\t\treturn p.%s(ctx, %s)\n", camelName, callArg))
	} else {
		buf.WriteString(fmt.Sprintf("\t\toutput, err := p.%s(ctx, %s)\n", camelName, callArg))
		buf.WriteString("\t\tif err != nil {\n")
		buf.WriteString("\t\t\treturn \"\", err\n")
		buf.WriteString("\t\t}\n")
		buf.WriteString("\t\toutputJSON, err := json.Marshal(output)\n")
		buf.WriteString("\t\tif err != nil {\n")
		buf.WriteString(fmt.Sprintf("\t\t\treturn \"\", fmt.Errorf(%q, err)\n", ir.PluginName+"."+fn.Name+": marshal output: %w"))
		buf.WriteString("\t\t}\n")
		buf.WriteString("\t\treturn string(outputJSON), nil\n")
	}

	buf.WriteString("\t}); err != nil {\n")
	buf.WriteString("\t\treturn err\n")
	buf.WriteString("\t}\n")
	return buf.String()
}

// generateGoRegisterStreamCall generates one streamScope.RegisterStream
// call, matching plugin.StreamFuncRegistry.RegisterStream's signature
// (plugin.FuncOptions, plugin.PluginStreamFunc). A streaming function's
// OUTPUT is always a channel of plugin.StreamEvent, never a manifest type
// -- only its INPUT can be typed.
func generateGoRegisterStreamCall(fn HostFuncIR, ir *IR) string {
	var buf strings.Builder

	camelName := toCamelCase(fn.Name)
	inType, inRaw := goHandlerType(fn.InputType)
	opts := goFuncOptions(fn)

	if inRaw {
		buf.WriteString(fmt.Sprintf("\t\tif err := streamScope.RegisterStream(%s, p.%s); err != nil {\n", opts, camelName))
		buf.WriteString("\t\t\treturn err\n")
		buf.WriteString("\t\t}\n")
		return buf.String()
	}

	buf.WriteString(fmt.Sprintf("\t\tif err := streamScope.RegisterStream(%s, func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {\n", opts))
	buf.WriteString(fmt.Sprintf("\t\t\tvar input %s\n", inType))
	buf.WriteString("\t\t\tif err := json.Unmarshal([]byte(inputJSON), &input); err != nil {\n")
	buf.WriteString(fmt.Sprintf("\t\t\t\treturn nil, fmt.Errorf(%q, err)\n", ir.PluginName+"."+fn.Name+": invalid input: %w"))
	buf.WriteString("\t\t\t}\n")
	buf.WriteString(fmt.Sprintf("\t\t\treturn p.%s(ctx, input)\n", camelName))
	buf.WriteString("\t\t}); err != nil {\n")
	buf.WriteString("\t\t\treturn err\n")
	buf.WriteString("\t\t}\n")
	return buf.String()
}

// goFuncOptions renders a plugin.FuncOptions literal for fn. Only Name and
// Idempotent are sourced from the IR today -- SameValueOnReplay and
// SecretOnlyFields have no manifest representation yet.
func goFuncOptions(fn HostFuncIR) string {
	if fn.Idempotent {
		return fmt.Sprintf("plugin.FuncOptions{Name: %q, Idempotent: true}", fn.Name)
	}
	return fmt.Sprintf("plugin.FuncOptions{Name: %q}", fn.Name)
}

// toCamelCase converts a snake_case or kebab-case string to camelCase --
// the unexported business-method name generated code calls (toPascalCase,
// from_manifest.go, is exported-name case for the same input).
func toCamelCase(s string) string {
	p := toPascalCase(s)
	if p == "" {
		return p
	}
	r := []rune(p)
	r[0] = unicode.ToLower(r[0])
	return string(r)
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
		buf.WriteString(fmt.Sprintf("    %s %s %s%s\n", goName, goFT, jsonTag, desc))
	}

	buf.WriteString("}\n")
	return buf.String()
}
