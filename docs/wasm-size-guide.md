# WASM Binary Size Guide

WebAssembly binaries produced by `cleat build` vary significantly in size
depending on the Go standard library packages used by the workflow code. This
document explains which Go features cause bloat, provides typical size ranges,
and offers guidance for minimizing binary size.

---

## Why WASM Binary Size Matters

Large WASM binaries affect cleat in three ways:

1. **Cold start latency** — The binary is loaded from PostgreSQL on first use
2. **Module cache memory** — Each cached module occupies RAM in the worker
3. **Storage** — `workflow_defs.wasm_bytes` stores each deployed version

---

## Go Features That Cause Bloat

Go WASM binaries include the Go runtime, garbage collector,
and all transitively-referenced packages. The following features
can still add significant weight:

| Feature / Package | Approx Size Increase | Reason |
|-------------------|---------------------|--------|
| `reflect` | +2-3 MB | Full type system metadata, method tables, reflect.Value support |
| `encoding/json` | +1-2 MB | Includes reflect-based marshaler/unmarshaler, decoder tables |
| `fmt` | +500 KB - 1 MB | Printf format string parser, reflection-based printing |
| `net/url` | +300-500 KB | URL parsing tables, character class tables, IDNA tables |
| `regexp` | +200-400 KB | Regexp compiler, syntax tree, execution engine |
| `net/http` | +3-5 MB | Full HTTP client/server, DNS resolver, TLS (if included) |
| `time` (full) | +200-400 KB | Timezone database, location tables, formatting tables |
| `crypto/tls` | +2-4 MB | Certificate parsing, cipher suite implementations, key exchange |
| `os` (with exec) | +500 KB - 1 MB | Process management, file I/O, env var handling |
| `database/sql` | +1-2 MB | Driver interfaces, connection pooling, type conversion |
| `text/template` | +500 KB - 1 MB | Template parser, execution engine, function maps |

### Cumulative Effect

| Import Set | Standard Go Size |
|------------|-----------------|
| No imports (hello world) | ~4 MB |
| `fmt` only | ~5 MB |
| `encoding/json` | ~6 MB |
| `reflect` + `fmt` + `encoding/json` | ~8 MB |
| `net/http` | ~10 MB |
| Full workflow (calls, sleep, signals) | ~6-8 MB |

---

## Actual Size Measurements

Builds from the cleat test suite:

| Workflow | Standard Go |
|----------|-------------|
| `testdata/basic/order.go` (basic order workflow) | 5.2 MB |
| `testdata/vet-checks/go` | 4.8 MB |
| Rust workflow (`examples/rust-workflow`) | 1.2 MB (wasm) |
| AssemblyScript workflow (`examples/as-workflow`) | 13 KB |

---

## The `cleat build --size-report` Flag

To help developers understand what contributes to their WASM binary size, the
`cleat build` command supports a `--size-report` flag:

```bash
cleat build --size-report ./my-workflow/
```

This analyzes the compiled WASM binary and outputs a breakdown by package:

```
WASM Binary: my_workflow.wasm (6,234,112 bytes)

Size breakdown by package:
  main                    1,024,000 bytes  (16.4%)
  fmt                       896,000 bytes  (14.4%)
  encoding/json             512,000 bytes  (8.2%)
  reflect                 2,048,000 bytes  (32.8%)
  runtime                 1,024,000 bytes  (16.4%)
  sync                      256,000 bytes  (4.1%)
  strconv                   192,000 bytes  (3.1%)
  sort                       96,000 bytes  (1.5%)
  math                      128,000 bytes  (2.1%)
  os                         48,000 bytes  (0.8%)
  unicode/utf8               16,000 bytes  (0.3%)
  unicode                    8,000 bytes   (0.1%)

Recommendations:
  - Remove "reflect" import: saves ~2 MB
  - Replace "encoding/json" with "github.com/goccy/go-json": saves ~500 KB
```

> **Note**: The `--size-report` flag is available in cleat v0.4+.

---

## Best Practices for Minimizing Binary Size

### 1. Audit Your Imports

Run `cleat vet` to see which packages your workflow imports. Remove unnecessary
imports, especially:

- `fmt` → use `h.DurableLog()` for logging
- `reflect` → use compile-time generics where possible
- `encoding/json` → consider lighter alternatives (e.g., `github.com/goccy/go-json`,
  `github.com/json-iterator/go`, or manual serialization for simple types)
- `regexp` → use `strings.Contains()` / `strings.HasPrefix()` where possible

### 2. Use Build Tags for Debug Code

```go
// workflow.go
package main

func process(input string) string {
    result := transform(input)
    debugLog(result) // excluded from production builds
    return result
}

// debug.go
//go:build debug
package main

func debugLog(msg string) {
    println(msg) // adds fmt import only in debug builds
}

// release.go
//go:build !debug
package main

func debugLog(string) {} // no-op, no import overhead
```

### 3. Avoid Large Initialization Tables

Package-level `var` declarations with large literal data (e.g., lookup tables
with hundreds of entries) are included in the binary even if only a small subset
is used. Consider generating these at initialization or loading from a compressed
resource.

---

## Comparison: cleat vs Other WASM Runtimes

| Runtime | Hello World | Typical Workflow | Notes |
|---------|-------------|------------------|-------|
| cleat (Go, wasip1) | ~4 MB | 6-10 MB | Bundles Go runtime |
| cleat (Rust) | ~1-2 MB | 2-4 MB | Rust std is smaller |
| cleat (AssemblyScript) | ~5 KB | 10-50 KB | Minimal overhead |

---

## References

- [Workflow Go Constraints](./workflow-go-constraints.md)
- [WASM Binary Toolkit (wabt)](https://github.com/WebAssembly/wabt) — for manual binary inspection
- [Twiggy](https://github.com/rustwasm/twiggy) — WASM binary size profiler (Rust)
