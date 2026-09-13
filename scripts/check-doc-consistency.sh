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

# --- 3. The documented host-call set vs the engine's exports ---------------
# Code: engine/imports.go  builder...Export("cleat_call")
# Doc:  ABI.md             #### 2.1 `cleat_call`
#
# Added after ABI.md spent two days announcing seven entries it no longer
# contained, and shipped a table naming two host calls by a binding name no
# guest can import -- `cleat_plugin_call` for `plugin_call`. Checks 1 and 2
# above are two numbers; this is the membership, which is what an SDK author
# actually implements against.
#
# ANCHOR TO WHERE THE ARTIFACT LIVES. An entry is a heading. A retraction is
# prose in a body and can never start one, so `^####` tells a host call from a
# sentence about a removed host call -- which `grep -c <name>` cannot, and did
# not: its count ROSE as removals were documented more thoroughly.
#
# Do NOT put `cleat_` in either pattern. Three exports carry no prefix
# (plugin_call, plugin_call_streaming, set_query_state), and a prefix-anchored
# scan drops all three while still returning a plausible total.
# shellcheck disable=SC2016  # the backticks are literal: ABI.md wraps each
# host call in a markdown code span, so the heading is exactly "#### 2.1 `name`".
doc_calls="$(grep -oE '^#### 2\.[0-9]+[a-z]? `[a-z_]+`' ABI.md |
  sed 's/.*`\(.*\)`/\1/' | sort -u)"
code_calls="$(grep -oE '\.Export\("[^"]+"\)' engine/imports.go |
  sed 's/.*Export("//;s/")//' | sort -u)"

n_doc="$(printf '%s\n' "$doc_calls" | grep -c . || true)"
n_code="$(printf '%s\n' "$code_calls" | grep -c . || true)"

# Vacuity guard first: an extractor that matches nothing agrees with everything.
if [ "$n_doc" -eq 0 ]; then
  note_failure "extracted 0 host-call entries from ABI.md; has the '#### 2.N \`name\`' heading format changed?"
elif [ "$n_code" -eq 0 ]; then
  note_failure "extracted 0 exports from engine/imports.go; has the builder.Export(...) form changed?"
else
  only_doc="$(comm -23 <(printf '%s\n' "$doc_calls") <(printf '%s\n' "$code_calls"))"
  only_code="$(comm -13 <(printf '%s\n' "$doc_calls") <(printf '%s\n' "$code_calls"))"
  if [ -n "$only_doc" ]; then
    note_failure "ABI.md documents host calls the engine does not export (an SDK binding one gets a module that fails to instantiate):"
    printf '%s\n' "$only_doc" | sed 's/^/    /' >&2
  fi
  if [ -n "$only_code" ]; then
    note_failure "engine/imports.go exports host calls ABI.md does not document:"
    printf '%s\n' "$only_code" | sed 's/^/    /' >&2
  fi
fi

# --- 4. No other document restates the count ------------------------------
# Section 3 holds ABI.md's host-call SET to the code. Nothing held any other
# document to anything, and on 2026-09-13 that showed: eight tracked documents
# stated a count and seven were wrong, three of them by five (cleat#1414). The
# one that was right is the one section 3 checks.
#
# Delegated to Python rather than written here, because the claim wraps across
# lines -- "HostCall imports" and the number were never on one line together, so
# a line-oriented grep for it returned empty and was nearly published as "no
# other occurrences". A guard for something this shape should not be a shell
# script; see CLAUDE.md's zsh/bash section for why.
if ! python3 scripts/check-host-call-counts.py; then
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo >&2
  echo "ABI.md is a public contract implemented by SDKs in other languages." >&2
  echo "Update it alongside the code, or update the code." >&2
  exit 1
fi

echo "OK: ABI.md agrees with the code (ABI version $code_abi, output buffer $code_buf bytes, $n_doc host calls)."
