module cleat-wasm-demo

go 1.26

toolchain go1.26.2

require (
	github.com/cleat-team/cleat/cleat v0.0.0
	github.com/tetratelabs/wazero v1.11.1-0.20260508161934-e6dd6c0c144f
)

require golang.org/x/sys v0.44.0 // indirect

replace (
	github.com/cleat-team/cleat => ../
	github.com/cleat-team/cleat/cleat => ../cleat
)
