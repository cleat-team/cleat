// Package analyzer loads Go packages with full type information and
// builds the internal representation used by the rest of the WASM
// compilation pipeline: call graph construction, closure computation,
// and AST transformation.
//
// Key types:
//   - AnalysisResult — loaded packages with all functions and entry points
//   - Package — a loaded Go package with its ASTs and type information
//   - FuncDecl — a function or method with resolved type information
//
// Key functions:
//   - LoadPackages — loads packages matching a pattern with full type info
package analyzer
