// Package plugingen provides multi-language code generation from cleat
// plugin manifests. It normalizes a plugin.Manifest into an intermediate
// representation (IR), then feeds it to language-specific generators.
//
// Supported target languages: Go, TypeScript, Python, Rust.
//
// Key types:
//   - IR — intermediate representation of a plugin's API surface
//   - HostFuncIR — description of a single host function
//   - TypeIR — description of a named structured type
//
// Key functions:
//   - FromManifest — converts a plugin.Manifest into IR
//   - GenerateGo / GenerateTS / GeneratePython / GenerateRust — code gen
package plugingen
