module durable-wasm-demo

go 1.26

toolchain go1.26.2

require (
	github.com/rcownie/durable v0.0.0
	github.com/tetratelabs/wazero v1.9.0
)

replace github.com/rcownie/durable => ../
