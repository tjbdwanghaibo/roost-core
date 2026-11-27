#!/usr/bin/env bash
# Offline reconciliation of an unpacked subscriber-review archive. Does not run Go or any code from the archive.
set -euo pipefail
if [[ $# != 2 || $1 != check || ! -f $2/checked.json || ! -f $2/SHA256.json ]]; then
  echo 'usage: bash run.sh check <unpacked frozen evidence directory>' >&2
  exit 2
fi
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 -B "$here/verify.py" "$2"
