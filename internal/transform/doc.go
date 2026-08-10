// Package transform implements AST source-to-source transformation that
// automatically threads cleat.HostCalls through the cleat closure.
//
// Developers declare a package-level var h *cleat.HostCalls. Functions
// in the closure that reference this global get h inserted as a first
// parameter; call sites are updated to pass h through.
//
// Key types:
//   - Config — transformation inputs (analysis, call graph, closure result)
//   - Result — transformed files and metadata
//
// Key functions:
//   - Run — applies the transformation to the analyzed packages
package transform
