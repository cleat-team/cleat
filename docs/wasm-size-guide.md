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

This reads the compiled WASM binary and attributes its code section to packages.
Real output, from `./testdata/basic/` on 2026-09-12:

```
  ===== WASM Size Report =====
  Binary: cancel_order.wasm (4.7 MB)
  Target: go
  Code section: 3.0 MB of 4.7 MB (64.5%)

  Code size by package (measured from the binary's name section;
  the Go linker encodes '/' as '_' in these names):
    runtime                                           1.1 MB  (24.0% of binary, 1262 funcs)
    encoding_json_v2                                437.0 KB  (9.1% of binary, 175 funcs)
    reflect                                         236.4 KB  (4.9% of binary, 208 funcs)
    encoding_json_jsontext                          189.2 KB  (3.9% of binary, 123 funcs)
    time                                            146.4 KB  (3.0% of binary, 76 funcs)
    slices                                          123.9 KB  (2.6% of binary, 63 funcs)
    fmt                                             110.4 KB  (2.3% of binary, 50 funcs)
    main                                             64.5 KB  (1.3% of binary, 60 funcs)
    ... and 52 more package(s)
    unattributed (compiler-generated)                28.1 KB  (0.6% of binary)
    non-code sections (data, types, names)            1.7 MB  (35.5% of binary)

  Recommendations:
    - reflect costs 236.4 KB (4.9%) here -- it is usually pulled in by encoding/json;
      a hand-written marshaller removes both
    - fmt costs 110.4 KB (2.3%) here -- h.DurableLog() and strconv avoid it
```

**The numbers are per-binary, and the recommendations quote what this binary
spends.** Two workflows give different answers: `testdata/minimal-wf` imports no
JSON at all and spends 1.9% on `reflect` where the one above spends 4.9%.

**Package names carry the Go linker's mangling.** `/`, `:`, `(`, `)` and `*` are
all encoded as `_` in the WASM name section, so `internal/abi` reads as
`internal_abi` and `encoding/json/v2` as `encoding_json_v2`. The report does not
invert that, because the inverse is ambiguous — a package whose name genuinely
contains `_` would be indistinguishable — and a size report that guesses at
identifiers is worse than one that shows you what is in the file.

**A stripped binary gets no breakdown, and says so.** Attribution needs the WASM
name section; without it the report gives the total and the section sizes and
states that the per-package breakdown is unavailable.

> **Until cleat#1314 this flag did not read the binary.** It multiplied the
> file's length by a table of hardcoded constants — `reflect` was always 25%,
> `net/http` always 20% — so every binary produced the same shape of answer. The
> measured figure for `reflect` on the two fixtures above is 4.9% and 1.9%. The
> example output previously printed in this section was illustrative and did not
> come from a build.

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
