#!/usr/bin/env bash
# Render packaging/homebrew/Formula/cleat.rb.tmpl into an installable
# formula, by substituting its two placeholder tokens (__CLEAT_TAG__,
# __CLEAT_SHA256__) for real values. Used by:
#
#   - .github/workflows/release.yml's homebrew-bump job, with a real tag and
#     a sha256 computed from the tag's actual source tarball.
#   - packaging/homebrew/formula_test.go's TestRenderProducesAPinnedTaggedFormula,
#     with a fake tag and a dummy sha256 -- the known-positive dry run for
#     cleat#2068: it proves the substitution mechanism works without needing
#     a real release or network access.
#
# Usage: render-homebrew-formula.sh <tag> <sha256> [template] [output]
#   <tag>      e.g. v0.3.0 -- must match vX.Y.Z, the same shape the release
#              workflow's tag trigger requires.
#   <sha256>   64 lowercase hex characters.
#   [template] defaults to packaging/homebrew/Formula/cleat.rb.tmpl
#   [output]   defaults to stdout
set -euo pipefail

if [ "$#" -lt 2 ] || [ "$#" -gt 4 ]; then
  echo "usage: $0 <tag> <sha256> [template] [output]" >&2
  exit 2
fi

TAG="$1"
SHA="$2"
TEMPLATE="${3:-packaging/homebrew/Formula/cleat.rb.tmpl}"
OUTPUT="${4:-}"

if ! [[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::tag '$TAG' is not vX.Y.Z -- refusing to render a formula from it" >&2
  exit 1
fi
if ! [[ "$SHA" =~ ^[0-9a-f]{64}$ ]]; then
  echo "::error::sha256 '$SHA' is not 64 lowercase hex characters" >&2
  exit 1
fi
if [ ! -f "$TEMPLATE" ]; then
  echo "::error::template '$TEMPLATE' does not exist" >&2
  exit 1
fi

# Neither substitution value can contain a '#', so it is safe as the sed
# delimiter without escaping the tag's own slashes.
RENDERED="$(sed \
  -e "s#__CLEAT_TAG__#${TAG}#g" \
  -e "s#__CLEAT_SHA256__#${SHA}#g" \
  "$TEMPLATE")"

# The template's own guard: if either token survives, the sed above matched
# nothing, which means the template's placeholder text moved or was
# mistyped. Failing loudly here is what stops a formula with a literal
# "__CLEAT_TAG__" in its url from ever reaching the tap.
if grep -q '__CLEAT_TAG__\|__CLEAT_SHA256__' <<<"$RENDERED"; then
  echo "::error::a placeholder token survived rendering -- the template no longer matches what this script substitutes" >&2
  exit 1
fi

if [ -n "$OUTPUT" ]; then
  printf '%s\n' "$RENDERED" > "$OUTPUT"
else
  printf '%s\n' "$RENDERED"
fi
