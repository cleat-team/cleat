// Package callgraph builds a directed graph of function calls within
// user packages and identifies cleat leaves — functions that directly
// call HostCalls methods.
//
// The graph is used by the closure package to compute transitive
// closure for WASM compilation.
//
// Key types:
//   - Graph — directed call graph with forward and reverse edges
//
// Key functions:
//   - Build — constructs a call graph from analyzer.AnalysisResult
package callgraph
