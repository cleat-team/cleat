// Package cleat (version.go) — Build-time version stamping for Go workflows.
//
// These variables are injected at build time via -ldflags:
//
//	tinygo build -ldflags="-X 'github.com/rcownie/cleat/cleat.WorkflowName=PlaceOrder'
//	                          -X 'github.com/rcownie/cleat/cleat.WorkflowVersion=3'
//	                          -X 'github.com/rcownie/cleat/cleat.MinVersion=1'
//	                          -X 'github.com/rcownie/cleat/cleat.ABIVersion=1'
//	                          -X 'github.com/rcownie/cleat/cleat.PluginDeps={\"llm\":\">=1.2.0\"}'"
//	                          ...
//
//	go build -ldflags="-X 'github.com/rcownie/cleat/cleat.WorkflowName=PlaceOrder' ..."
//
// Default values are "unknown" / 0 / "{}" so that unconfigured builds still
// produce valid WASM modules (with obvious placeholder metadata).
package cleat

var (
	// WorkflowName is the human-readable name of this workflow definition.
	// Set at build time: -X cleat.WorkflowName=PlaceOrder
	WorkflowName = "unknown"

	// WorkflowVersion is the monotonic version number for this workflow def.
	// Set at build time: -X cleat.WorkflowVersion=3
	WorkflowVersion = 0

	// MinVersion is the minimum compatible workflow definition version.
	// Child workflows check this to ensure compatibility.
	// Set at build time: -X cleat.MinVersion=1
	MinVersion = 1

	// ABIVersion is the WASM host ABI version this module targets.
	// Set at build time: -X cleat.ABIVersion=1
	ABIVersion = 1

	// PluginDeps is a JSON object mapping plugin names to semver constraints.
	// Example: {"llm": ">=1.2.0", "blobstore": "~2.0.0"}
	// Set at build time: -X cleat.PluginDeps=<json-string>
	PluginDeps = "{}"
)
