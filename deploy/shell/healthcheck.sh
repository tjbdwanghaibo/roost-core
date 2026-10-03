#!/usr/bin/env sh
set -eu
URL=${1:-http://127.0.0.1:9100/readyz}
if command -v curl >/dev/null 2>&1; then
  curl --fail --silent --show-error --max-time 3 "$URL" >/dev/null
elif command -v wget >/dev/null 2>&1; then
  wget -q -T 3 -O /dev/null "$URL"
else
  printf 'curl or wget is required\n' >&2
  exit 2
fi
printf 'ready: %s\n' "$URL"
