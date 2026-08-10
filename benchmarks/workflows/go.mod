module github.com/cleat-team/cleat/benchmarks/workflows

go 1.25.11

require github.com/cleat-team/cleat/cleat v0.0.0

replace (
	github.com/cleat-team/cleat => ../../
	github.com/cleat-team/cleat/cleat => ../../cleat
)
