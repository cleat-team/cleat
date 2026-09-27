module github.com/cleat-team/cleat/benchmarks/workflows

go 1.27.0

toolchain go1.27.1

require github.com/cleat-team/cleat/cleat v0.0.0

replace (
	github.com/cleat-team/cleat => ../../
	github.com/cleat-team/cleat/cleat => ../../cleat
)
