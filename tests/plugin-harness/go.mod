// tests/plugin-harness is its own module because it imports the cleat/ SDK
// (cleat/cleattest and cleat/wasmtest) and the root module must not, at any depth including tests:
// cleat/ requires the root module back, and the pair of `replace` directives
// that used to resolve that cycle is what made `go install
// github.com/cleat-team/cleat/cmd/cleat@vX` refuse to run. See CLAUDE.md.
//
// The replaces below are safe where the root module's were not. `go install
// pkg@version` reads the go.mod of the module *containing pkg* -- the root
// one -- and never descends into a nested module, so nothing here can reach a
// user installing the CLI.
//
// Consequence to know about: `go test ./tests/plugin-harness/...` from the repo
// root no longer works. It does not match zero packages, it fails outright
// ("directory prefix tests/plugin-harness does not contain main module or its
// selected dependencies"). plugin-harness-ci.yml runs it with a
// working-directory instead.
module github.com/cleat-team/cleat/tests/plugin-harness

go 1.25.11

require (
	github.com/cleat-team/cleat v0.0.0
	github.com/cleat-team/cleat/cleat v0.0.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/bytecodealliance/wasmtime-go/v44 v44.0.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-ini/ini v1.67.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-sql-driver/mysql v1.8.1 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/klauspost/compress v1.18.2 // indirect
	github.com/klauspost/cpuid/v2 v2.2.11 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/lib/pq v1.12.3 // indirect
	github.com/microsoft/go-mssqldb v1.10.0 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/minio/minio-go/v7 v7.1.0 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/tetratelabs/wazero v1.11.1-0.20260508161934-e6dd6c0c144f // indirect
	github.com/tinylib/msgp v1.6.1 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.43.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.43.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/sdk v1.43.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/mod v0.37.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	golang.org/x/tools v0.47.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260414002931-afd174a4e478 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
	google.golang.org/grpc v1.82.1 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	github.com/cleat-team/cleat => ../../
	github.com/cleat-team/cleat/cleat => ../../cleat
)
