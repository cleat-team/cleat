#!/usr/bin/env bash
#
# Build the Python quick start's workflow with NO checkout anywhere in sight, and
# require the build to have used the cleat-sdk it installed. cleat#3793.
#
# WHY THIS EXISTS
#
# `pip install cleat-sdk` shipped neither the WIT nor build_wasm.py, so
# `cleat build --target python` outside a checkout failed with "finding repo root:
# go.mod not found". Every Python build this repository ran, in CI and in its tests,
# ran INSIDE the checkout, where the files are found by walking up to the repo root
# -- so the defect was invisible to all of them, and `build-documented-examples.sh`
# runs from the checkout for the same reason.
#
# A build run from the checkout cannot say whether the installed package is enough,
# so this one is run from a temporary directory with a COPY of the CLI binary:
# `cleat` also looks for python-sdk/ beside its own executable, and a binary left in
# .bin/ would find the checkout that way and make the check vacuous.
#
# THE WORKFLOW IS THE DOCUMENT'S. hello.py is extracted from the tutorial, so the
# file a reader copies is the file built here, and an edit to the page that breaks
# the build is caught.
#
# Exit status: 0 built, 1 a finding, 2 could not establish what was being measured.
set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 2
repo="$(pwd)"

CLEAT_BIN="${CLEAT_BIN:-$repo/.bin/cleat}"
if [[ ! -x "$CLEAT_BIN" ]]; then
  echo "UNMEASURED: no cleat binary at $CLEAT_BIN. Build it first:" >&2
  echo "  go build -o .bin/cleat ./cmd/cleat" >&2
  exit 2
fi

doc="docs/tutorials/python-quick-start.md"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/proj"

# The workflow, from the page.
if ! python3 - "$doc" "$work/proj/hello.py" <<'PY'
import re, sys
text = open(sys.argv[1]).read()
m = re.search(r"Save this as `hello\.py`:\n\n```python\n(.*?)```", text, re.S)
if not m:
    sys.exit(1)
open(sys.argv[2], "w").write(m.group(1))
PY
then
  echo "UNMEASURED: $doc has no 'Save this as \`hello.py\`' block, so there is nothing to build." >&2
  exit 2
fi

cp "$CLEAT_BIN" "$work/bin/cleat"

# A copy of the SDK source: pip builds in the tree it is given, and the checkout
# should not grow a build/ directory.
cp -R python-sdk "$work/sdk-src"
rm -rf "$work/sdk-src/build" "$work/sdk-src/dist"

python3 -m venv "$work/venv" || { echo "UNMEASURED: could not create a virtual environment." >&2; exit 2; }
# shellcheck disable=SC1091
. "$work/venv/bin/activate"
if ! pip install --quiet "$work/sdk-src" 'componentize-py>=0.12.0' >"$work/pip.log" 2>&1; then
  echo "UNMEASURED: pip install failed, so the build below would measure nothing:" >&2
  tail -20 "$work/pip.log" >&2
  exit 2
fi

# Evidence that nothing but the installed package can be found: no go.mod above the
# working directory. (mktemp is outside the checkout; this is a check on that.)
dir="$work/proj"
while [[ "$dir" != "/" ]]; do
  if [[ -e "$dir/go.mod" ]]; then
    echo "UNMEASURED: $dir/go.mod exists above the scratch directory, so a build here could find a Go module root." >&2
    exit 2
  fi
  dir="$(dirname "$dir")"
done

mkdir -p "$work/proj/out"
cd "$work/proj" || exit 2
if ! "$work/bin/cleat" build --target python -o out ./hello.py >"$work/build.log" 2>&1; then
  echo "FAIL: cleat build --target python failed outside a checkout:" >&2
  tail -30 "$work/build.log" >&2
  exit 1
fi

# It built. Did it build from the INSTALLED package? build_wasm.py prints the WIT
# directory it used.
if ! grep -q "WIT: .*$work/venv/.*/cleat_sdk/wit" "$work/build.log"; then
  echo "FAIL: the build succeeded but did not report the installed package's WIT. It found its inputs somewhere else:" >&2
  grep -E 'WIT:' "$work/build.log" >&2 || echo "  (no WIT: line at all)" >&2
  exit 1
fi

# And produced a component: the WebAssembly magic number.
if [[ "$(head -c 4 out/hello.wasm | od -An -tx1 | tr -d ' \n')" != "0061736d" ]]; then
  echo "FAIL: out/hello.wasm is not a WebAssembly binary." >&2
  exit 1
fi

echo "OK: the Python quick start's hello.py built from the installed cleat-sdk, with no checkout."
