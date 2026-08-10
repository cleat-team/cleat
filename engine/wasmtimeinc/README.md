# Vendored wasmtime C headers

These are the C headers from `github.com/bytecodealliance/wasmtime-go/v44`'s
`build/include` directory, copied verbatim. They are **not** maintained here:
`wasmtime_headers_test.go` diffs this tree against the wasmtime-go module in the
build's module cache and fails on any divergence, so a `go.mod` bump that is not
accompanied by re-copying these files is a red test rather than a silent header/
library skew.

## Why they are vendored

`engine/component_cgo.go` calls wasmtime's Component Model C API directly,
because wasmtime-go v44 exposes no Go binding for it — the module ships exactly
one component-related Go file, `config_feat_component_model.go`, which is a
config flag and nothing more. Types like `wasmtime_component_val_t` are reachable
only from C.

cgo therefore needs an `-I` to these headers, and there is no way to point one at
another module: `${SRCDIR}` in a `#cgo` directive expands to the directory of the
file containing it, never to an imported module's directory, and the module cache
path varies with `GOMODCACHE`. That left the native component path behind the
`wasmtime_component_cgo` build tag, which **no build, CI job, Makefile or
Dockerfile set** — so every build got the stub, and every Component Model guest
(i.e. every Python workflow) fell through to the hand-rolled decomposition path,
where it does not instantiate. See IMPROVEMENT-PLAN.md §2.72 and §1.5/§2.28.

Vendoring is what makes `-I${SRCDIR}/wasmtimeinc` expressible, so a plain
`go build ./...` — and anything embedding the engine as a library, which
`cleat/embedded` exists to support — carries the component path with no flags to
remember.

## Scope

C headers only (39 files). The `.hh` C++ headers in the upstream directory are
not used by any cgo file here and are deliberately not copied; verified by
building and running the component path against this tree alone.

No `.a`/`.dylib` is vendored. Linking against `libwasmtime` already works through
wasmtime-go's own `#cgo LDFLAGS` in `ffi.go`, which is combined at final link
time — which is also why these headers must stay in lockstep with the module
version, and why the drift test exists.

## Upstream

Apache-2.0 WITH LLVM-exception; each file carries its own upstream header. See
https://github.com/bytecodealliance/wasmtime.

## Updating

    WTDIR=$(go list -m -f '{{.Dir}}' github.com/bytecodealliance/wasmtime-go/v44)
    rm -rf engine/wasmtimeinc/*.h engine/wasmtimeinc/wasmtime
    (cd "$WTDIR/build/include" && find . -name '*.h' | while read -r f; do
        mkdir -p "$OLDPWD/engine/wasmtimeinc/$(dirname "$f")"
        cp "$f" "$OLDPWD/engine/wasmtimeinc/$f"
    done)
    chmod -R u+w engine/wasmtimeinc
