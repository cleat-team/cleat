#!/usr/bin/env bash
#
# Generate a TypeScript client from the worker's own /api/openapi.json and
# require it to type-check a correct call and to REFUSE a misspelled one
# (cleat#2972, cleat#1980's acceptance item 3).
#
# ---------------------------------------------------------------------------
# WHAT THIS DOES AND DOES NOT COVER. Read this before trusting a green.
#
# EXERCISED
#   cleat build -> the real sidecar it writes (workflow.wasm.schema.json,
#     a map of engine.EntryPointSchema keyed by WASM export name)
#   -> the real handler, reading it the way a served request would
#   -> the document
#   -> openapi-typescript
#   -> tsc
#
# NOT EXERCISED, and a green here says nothing about them:
#   * the sidecar READ in `cleatctl deploy` (cmd/cleatctl/deploy.go)
#   * the deploy itself
#   * the HTTP routing registration and a worker actually serving the route
#
# Those have their own tests. The build -> deploy JOIN is cleat#1980's
# acceptance item 1 and is deliberately not claimed here.
#
# ---------------------------------------------------------------------------
# THE NEGATIVE CONTROL IS THE DELIVERABLE, so it is worth being precise about
# what it can and cannot catch.
#
# The emitted schema sets additionalProperties:true, deliberately: it mirrors
# the binding, which ignores keys it does not know (cleat#1690). So a typo in
# an EXTRA key COMPILES -- openapi-typescript emits an index signature -- and
# the misspelled-field case has to mean a misspelled REQUIRED field, where the
# required key is consequently absent. That is the case built here, and the
# extra-key case is asserted to compile so the boundary is documented rather
# than discovered.
#
# ---------------------------------------------------------------------------
# EXIT STATUS. Three outcomes, because a check that measured nothing agrees
# with every tree:
#
#   0  the document supports a typed client: the correct call compiles and the
#      misspelled one does not.
#   1  a finding about the DOCUMENT: the generated types accept a call the
#      endpoint would reject, or reject one it would accept.
#   2  the check could not establish what it was measuring -- no node,
#      `cleat build` failed, the document was not produced, the generator
#      failed. This is a failure of the CHECK, not of the document.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 2

# Pinned exactly, per the repo's convention for tooling a check depends on:
# a floating generator version turns "the document changed" into "the tool
# changed".
OPENAPI_TS_VERSION="${OPENAPI_TS_VERSION:-7.10.1}"
TYPESCRIPT_VERSION="${TYPESCRIPT_VERSION:-5.9.3}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0

# unmeasured <reason> -- exit 2 territory: the check could not look.
unmeasured() {
  echo "UNMEASURED: $1" >&2
  echo "  This is a failure of the CHECK, not a finding about the document." >&2
  exit 2
}

command -v node >/dev/null 2>&1 || unmeasured "node is not installed"
command -v npx >/dev/null 2>&1 || unmeasured "npx is not installed"

# --- 1. Build the worker's own cleat binary, then a real fixture workflow. ----
echo "==> building cleat"
if ! go build -o "$WORK/cleat" ./cmd/cleat 2>"$WORK/cleat-build.err"; then
  cat "$WORK/cleat-build.err" >&2
  unmeasured "go build ./cmd/cleat failed"
fi

# testdata/optionalparam is the fixture internal/jsonschema's own tests use:
# ApplyCoupon(h, userID string, promo *Coupon) -- one required parameter and one
# pointer (optional) one, so the negative control has a required field to
# misspell.
echo "==> cleat build testdata/optionalparam"
if ! "$WORK/cleat" build -o "$WORK/fixture" ./testdata/optionalparam >"$WORK/cleat-build.log" 2>&1; then
  cat "$WORK/cleat-build.log" >&2
  unmeasured "cleat build failed for testdata/optionalparam"
fi

SIDECAR="$WORK/fixture/workflow.wasm.schema.json"
if [[ ! -s "$SIDECAR" ]]; then
  # The sidecar is the whole input. Its absence is not "a document with no
  # schemas" -- it is the check having nothing to measure.
  unmeasured "cleat build produced no schema sidecar at $SIDECAR"
fi
echo "    sidecar: $(wc -c <"$SIDECAR") bytes"

# --- 2. Emit the document through the real handler. -------------------------
echo "==> emitting the document from the real handler"
if ! CLEAT_OPENAPI_SCHEMA_SIDECAR="$SIDECAR" \
     CLEAT_OPENAPI_DOC_OUT="$WORK/openapi.json" \
     go test ./cmd/cleat-worker/ \
       -run TestEmitOpenAPIDocumentForTheGeneratedClientCheck -count=1 >"$WORK/emit.log" 2>&1; then
  cat "$WORK/emit.log" >&2
  unmeasured "the document-emitting test failed"
fi
if [[ ! -s "$WORK/openapi.json" ]]; then
  unmeasured "the handler emitted no document"
fi
echo "    document: $(wc -c <"$WORK/openapi.json") bytes"

# --- 3. Generate the client. ------------------------------------------------
echo "==> openapi-typescript@$OPENAPI_TS_VERSION"
if ! npx --yes "openapi-typescript@$OPENAPI_TS_VERSION" \
       "$WORK/openapi.json" -o "$WORK/api.d.ts" >"$WORK/gen.log" 2>&1; then
  cat "$WORK/gen.log" >&2
  unmeasured "openapi-typescript failed"
fi
if [[ ! -s "$WORK/api.d.ts" ]]; then
  unmeasured "openapi-typescript produced no output"
fi

# --- 4. Compile the fixtures. ----------------------------------------------
cp scripts/openapi-client-check/*.ts scripts/openapi-client-check/tsconfig.*.json "$WORK/"

# tsc's exit code alone cannot separate "the types rejected this" from "tsc
# could not run", so the output is read for a type error rather than the status
# being trusted on its own. Measured: tsc exits 2 on TS2322, which is also the
# status some tooling failures produce.
#
# AND THE CODE RANGE IS LOAD-BEARING, not decoration. A first version matched a
# bare 'error TS' -- which is also what a CONFIGURATION error looks like. With
# the tsconfig accidentally not copied into the work directory, tsc emitted
# TS5058 ("the specified path does not exist"), the classifier called it a type
# error, and the script reported a broken check as a finding about the
# document. Semantic diagnostics are TS1xxx/TS2xxx; TS5xxx is the compiler
# telling you it could not set itself up. Matching only the former keeps
# "tsc could not run" (exit 2) apart from "the types rejected this" (exit 1).
compile() { # <name> <tsconfig> -> "ok" | "typeerror" | "toolerror"
  local out="$WORK/$1.out"
  if npx --yes --package "typescript@$TYPESCRIPT_VERSION" tsc -p "$WORK/$2" >"$out" 2>&1; then
    echo ok
  elif grep -qE 'error TS[12][0-9]{3}' "$out"; then
    echo typeerror
  else
    echo toolerror
  fi
}

echo "==> tsc: a correct call must COMPILE"
positive="$(compile positive tsconfig.positive.json)"
case "$positive" in
  ok) echo "    ok" ;;
  typeerror)
    echo "FAIL: the generated types reject a call the endpoint accepts:" >&2
    cat "$WORK/positive.out" >&2
    failures=$((failures + 1))
    ;;
  *) cat "$WORK/positive.out" >&2; unmeasured "tsc could not run for the positive fixtures" ;;
esac

echo "==> tsc: a misspelled required field must FAIL"
negative="$(compile negative tsconfig.negative.json)"
case "$negative" in
  typeerror) echo "    ok (rejected, as required)" ;;
  ok)
    echo "FAIL: a client generated from this document ACCEPTS a misspelled required field." >&2
    echo "  The document's types do not constrain the call, so a client generated" >&2
    echo "  from it cannot catch the error the acceptance is about." >&2
    failures=$((failures + 1))
    ;;
  *) cat "$WORK/negative.out" >&2; unmeasured "tsc could not run for the negative fixture" ;;
esac

echo
if (( failures > 0 )); then
  echo "FAILED: $failures finding(s) about the document."
  exit 1
fi
echo "OK: the document supports a typed client, and the negative control was refused."
exit 0
