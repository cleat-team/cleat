#!/usr/bin/env bash
# Install the componentize-py version python-sdk/componentize-py-requirements.txt pins (cleat#4355), from that file and nowhere else,
# and check that the install is that version.
#
#   0  installed, and `componentize-py --version` is the pinned version
#   2  could not establish that: the pin could not be read, or the installed version is not it. A failure of this step, not a finding
#      about the tree.
#
# `pip install --require-hashes` already refuses a file whose hash is not listed. The version check is for the other way this goes
# wrong: an earlier step, or the runner image, already put a different componentize-py on PATH and pip reported "already satisfied".
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
req="${COMPONENTIZE_PY_REQUIREMENTS:-$here/../python-sdk/componentize-py-requirements.txt}"

if [ ! -r "$req" ]; then
  echo "UNMEASURED: cannot read $req. This is a failure of the check, not a finding about the tree." >&2
  exit 2
fi
# Exactly one `componentize-py==<version>` line, anchored at the start of the line: a comment that names a version cannot be it.
want=$(sed -nE 's/^componentize-py==([0-9][0-9A-Za-z.+!-]*)([[:space:]]*\\)?[[:space:]]*$/\1/p' "$req")
if [ "$(printf '%s\n' "$want" | grep -c .)" -ne 1 ]; then
  echo "UNMEASURED: expected exactly one 'componentize-py==<version>' line in $req, found: '$want'. This is a failure of the check, not a finding about the tree." >&2
  exit 2
fi

python -m pip install --require-hashes --no-deps --force-reinstall -r "$req"

got=$(componentize-py --version)
echo "$got"
if [ "$got" != "componentize-py $want" ]; then
  echo "UNMEASURED: expected 'componentize-py $want', got '$got'. This is a failure of the check, not a finding about the tree." >&2
  exit 2
fi
