// Package wasm provides WASM compilation, binary scanning, metadata
// embedding, and component model support for cleat workflows.
//
// It builds WASM modules from Go source, embeds custom sections
// ("cleat.metadata"), scans binaries for exports and imports, and
// manages concurrent compilation with build locks.
//
// Key types:
//   - BuildConfig — parameters for WASM build directory assembly
//   - Metadata — workflow metadata embedded in WASM custom sections
//   - ScanResult — exports, imports, and memory info from a WASM binary
package wasm
