#!/usr/bin/env bash
#
# Assert that ABI.md agrees with the code it specifies.
#
# ABI.md is a public contract: it is what an SDK author in another language
# implements against. It has been wrong in both directions.
#
#   * It claimed ABI version 4 in the header and 5 in the changelog. The
#     shipped value has always been 1.
#   * It documented every output-buffer capacity as 65536 bytes, in 30 places,
#     while the host passes engine/memory.go's DefaultOutBufSize (1048576).
#     An SDK sized from the document under-allocates by 16x.
#   * It described the scratch region at a fixed 0xA00000/0xA10000 with a
#     10 MiB + 128 KiB growth target. The base is dynamic and the output
#     buffer sits 1 MiB past it, not 64 KiB.
#
# None of that is catchable by a test suite; the document is not executable.
# This script makes the two numbers that matter checkable.
#
# Usage: scripts/check-doc-consistency.sh

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

fail=0

note_failure() {
  echo "ERROR: $1" >&2
  fail=1
}

# --- 1. ABI version -------------------------------------------------------
# Code:  wasm/metadata.go   const CurrentABIVersion = 1
# Doc:   ABI.md            "ABI version: 1 — ..."
code_abi="$(grep -oE 'CurrentABIVersion[[:space:]]*=[[:space:]]*[0-9]+' wasm/metadata.go |
  grep -oE '[0-9]+$' | head -1)"
doc_abi="$(grep -oiE '^ABI version:[[:space:]]*[0-9]+' ABI.md |
  grep -oE '[0-9]+' | head -1)"

if [ -z "$code_abi" ]; then
  note_failure "could not find CurrentABIVersion in wasm/metadata.go"
elif [ -z "$doc_abi" ]; then
  note_failure "could not find an 'ABI version: N' line in ABI.md"
elif [ "$code_abi" != "$doc_abi" ]; then
  note_failure "ABI version mismatch: wasm/metadata.go says $code_abi, ABI.md says $doc_abi"
fi

# --- 2. Output buffer capacity -------------------------------------------
# Code: engine/memory.go   const DefaultOutBufSize = 1048576
# Doc:  ABI.md            "Output buffer capacity (1048576)" x N
#
# Every *_max_len parameter carries this number. 65536 is also the WASM page
# size and legitimately appears in the page-size note, so this checks that the
# buffer rows carry the code's value rather than banning a literal.
code_buf="$(grep -oE 'DefaultOutBufSize[[:space:]]*=[[:space:]]*[0-9]+' engine/memory.go |
  grep -oE '[0-9]+$' | head -1)"

if [ -z "$code_buf" ]; then
  note_failure "could not find DefaultOutBufSize in engine/memory.go"
else
  # Any buffer-capacity row quoting something other than $code_buf is stale.
  stale="$(grep -nE '(Output buffer capacity|Capacity of output buffer) \([0-9]+' ABI.md |
    grep -vE "\($code_buf" || true)"
  if [ -n "$stale" ]; then
    note_failure "ABI.md documents an output buffer capacity other than DefaultOutBufSize ($code_buf):"
    printf '%s\n' "$stale" | sed 's/^/    /' >&2
  fi

  # And there should be at least one such row, or the grep above is vacuous
  # and this check would silently pass on a rewritten document.
  count="$(grep -cE "(Output buffer capacity|Capacity of output buffer) \($code_buf" ABI.md)"
  if [ "$count" -eq 0 ]; then
    note_failure "ABI.md has no output-buffer-capacity rows quoting $code_buf; has the table format changed?"
  fi
fi

if [ "$fail" -ne 0 ]; then
  echo >&2
  echo "ABI.md is a public contract implemented by SDKs in other languages." >&2
  echo "Update it alongside the code, or update the code." >&2
  exit 1
fi

echo "OK: ABI.md agrees with the code (ABI version $code_abi, output buffer $code_buf bytes)."
