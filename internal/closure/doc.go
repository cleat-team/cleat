// Package closure computes the transitive closure of cleat workflow
// functions — the set of all functions that transitively reach a cleat
// leaf (a function calling HostCalls). It validates supported Go
// constructs and verifies HostCalls threading through the closure.
//
// Key types:
//   - Result — annotated functions with durability tags and validation errors
//
// Key functions:
//   - Compute — computes the cleat closure from analysis and call graph
package closure
