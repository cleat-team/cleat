// tests/cross-language is its own module because it imports the cleat/ SDK
// (cleat/wasmtest) and the root module must not, at any depth including tests:
// cleat/ requires the root module back, and the pair of `replace` directives
// that used to resolve that cycle is what made `go install
// github.com/cleat-team/cleat/cmd/cleat@vX` refuse to run. See CLAUDE.md.
//
// The replaces below are safe where the root module's were not. `go install
// pkg@version` reads the go.mod of the module *containing pkg* -- the root
// one -- and never descends into a nested module, so nothing here can reach a
// user installing the CLI.
//
// Consequence to know about: `go test ./tests/cross-language/...` from the repo
// root no longer works. It does not match zero packages, it fails outright
// ("directory prefix tests/cross-language does not contain main module or its
// selected dependencies"). e2e-cross-language.yml runs it with a
// working-directory instead.
module github.com/cleat-team/cleat/tests/cross-language

go 1.26.0

require (
	github.com/cleat-team/cleat v0.0.0
	github.com/cleat-team/cleat/cleat v0.0.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/bytecodealliance/wasmtime-go/v44 v44.0.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-sql-driver/mysql v1.10.1 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.30.0 // indirect
	github.com/lib/pq v1.12.3 // indirect
	github.com/microsoft/go-mssqldb v1.11.0 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/tetratelabs/wazero v1.12.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.46.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/sdk v1.46.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.0 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/tools v0.48.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260819154853-08b0e4226688 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260819154853-08b0e4226688 // indirect
	google.golang.org/grpc v1.83.1 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	github.com/cleat-team/cleat => ../../
	github.com/cleat-team/cleat/cleat => ../../cleat
)
