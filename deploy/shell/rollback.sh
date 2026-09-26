#!/usr/bin/env sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  printf 'rollback.sh must run as root\n' >&2
  exit 1
fi
if [ "$#" -ne 3 ]; then
  printf 'usage: %s <service> <sid> <version>\n' "$0" >&2
  exit 2
fi
SERVICE=$1
SID=$2
VERSION=$3
case "$SERVICE" in account|activity|chat|game|global|mail|match|platform|rank|session) ;; *) printf 'unknown service: %s\n' "$SERVICE" >&2; exit 2;; esac
case "$SID" in *[!0-9]*|'0'|'') printf 'sid must be a positive integer\n' >&2; exit 2;; esac
case "$VERSION" in *[!a-zA-Z0-9._-]*|'') printf 'invalid version\n' >&2; exit 2;; esac

INSTANCE=planet-$SERVICE-$SID
APP_ROOT=${APP_ROOT:-/opt/roost/$INSTANCE}
TARGET=$APP_ROOT/releases/$VERSION
CURRENT=$APP_ROOT/current
[ -d "$TARGET" ] || { printf 'release not installed: %s\n' "$TARGET" >&2; exit 2; }
PREVIOUS=$(readlink -f "$CURRENT" 2>/dev/null || true)
[ "$PREVIOUS" != "$TARGET" ] || { printf 'release %s is already current\n' "$VERSION"; exit 0; }
NEXT=$APP_ROOT/.current.$$
trap 'rm -f "$NEXT"' EXIT HUP INT TERM
ln -s "$TARGET" "$NEXT"
mv -Tf "$NEXT" "$CURRENT"
systemctl restart "$INSTANCE.service"

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
HEALTH_URL=${HEALTH_URL:-http://127.0.0.1:9100/readyz}
HEALTH_ATTEMPTS=${HEALTH_ATTEMPTS:-30}
attempt=1
while [ "$attempt" -le "$HEALTH_ATTEMPTS" ]; do
  if sh "$ROOT/deploy/shell/healthcheck.sh" "$HEALTH_URL" >/dev/null 2>&1; then
    printf 'rolled back %s to %s\n' "$INSTANCE" "$VERSION"
    exit 0
  fi
  attempt=$((attempt + 1))
  sleep 1
done
if [ -n "$PREVIOUS" ] && [ -d "$PREVIOUS" ]; then
  ln -s "$PREVIOUS" "$NEXT"
  mv -Tf "$NEXT" "$CURRENT"
  systemctl restart "$INSTANCE.service" || true
fi
printf 'rollback target failed readiness; restored previous release\n' >&2
exit 1
