module github.com/cleat-team/cleat

go 1.25.11

// The root module deliberately does NOT require github.com/cleat-team/cleat/cleat
// (the SDK module), and must not start. cleat/ requires the root module back --
// cleat/cleattest imports engine, cleat/dagrun imports plugins/dag -- so a
// require in this direction is a module cycle, resolvable only by a matching
// pair of `replace` directives. `go install pkg@version` refuses any module
// whose go.mod carries a replace, so that pair made `go install
// github.com/cleat-team/cleat/cmd/cleat@vX` impossible for every published tag.
// Removed 2026-08-10; see CLAUDE.md. Re-derive that the edge is still absent:
//
//	go list -deps ./... | grep -c cleat-team/cleat/cleat   # must be 0
require (
	github.com/fsnotify/fsnotify v1.10.1
	github.com/go-sql-driver/mysql v1.8.1
	github.com/google/uuid v1.6.0
	github.com/lib/pq v1.12.3
	github.com/microsoft/go-mssqldb v1.10.0
	github.com/minio/minio-go/v7 v7.1.0
	github.com/sendgrid/rest v2.6.9+incompatible
	github.com/sendgrid/sendgrid-go v3.16.1+incompatible
	github.com/tetratelabs/wazero v1.11.1-0.20260508161934-e6dd6c0c144f
	go.opentelemetry.io/otel v1.43.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.43.0
	go.opentelemetry.io/otel/metric v1.43.0
	go.opentelemetry.io/otel/sdk v1.43.0
	go.opentelemetry.io/otel/sdk/metric v1.43.0
	go.opentelemetry.io/otel/trace v1.43.0
	golang.org/x/mod v0.36.0
	golang.org/x/sys v0.45.0
	golang.org/x/time v0.15.0
	golang.org/x/tools v0.44.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/bytecodealliance/wasmtime-go/v44 v44.0.0
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-ini/ini v1.67.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/klauspost/compress v1.18.2 // indirect
	github.com/klauspost/cpuid/v2 v2.2.11 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/tinylib/msgp v1.6.1 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.43.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260414002931-afd174a4e478 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
	google.golang.org/grpc v1.82.1 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// No replace directives, on purpose. `go install pkg@version` fails outright on
// a module whose go.mod has any of them ("...contains one or more replace
// directives"), and cmd/cleat, cmd/cleatctl and cmd/cleat-worker are all meant
// to be installable that way. Adding one here re-breaks `go install` for every
// tag cut afterwards, and the failure appears only to users, never in CI.
