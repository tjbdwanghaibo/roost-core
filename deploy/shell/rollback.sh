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
case "$VERSION" in *[!a-zA-Z0-9._-]*|''|.|..) printf 'invalid version\n' >&2; exit 2;; esac

INSTANCE=planet-$SERVICE-$SID
APP_ROOT=${APP_ROOT:-/opt/roost/$INSTANCE}
UNIT_PATH=/etc/systemd/system/$INSTANCE.service
TARGET=$APP_ROOT/releases/$VERSION
CURRENT=$APP_ROOT/current
[ -d "$TARGET" ] || { printf 'release not installed: %s\n' "$TARGET" >&2; exit 2; }
PREVIOUS=$(readlink -f "$CURRENT" 2>/dev/null || true)
[ "$PREVIOUS" != "$TARGET" ] || { printf 'release %s is already current\n' "$VERSION"; exit 0; }
# Each release runs under the systemd unit it was installed with, recorded in
# $APP_ROOT/units/<version>.service and switched together with current.
record_unit() {
  install -d -m 0755 "$APP_ROOT/units"
  [ -n "$1" ] && [ -f "$UNIT_PATH" ] || return 0
  [ -e "$APP_ROOT/units/${1##*/}.service" ] || install -m 0644 "$UNIT_PATH" "$APP_ROOT/units/${1##*/}.service"
}
use_unit() {
  if [ ! -f "$APP_ROOT/units/${1##*/}.service" ]; then
    printf 'no unit recorded for release %s; keeping %s\n' "${1##*/}" "$UNIT_PATH" >&2
    return 0
  fi
  install -m 0644 "$APP_ROOT/units/${1##*/}.service" "$UNIT_PATH"
  systemctl daemon-reload
}
record_unit "$PREVIOUS"
NEXT=$APP_ROOT/.current.$$
trap 'rm -f "$NEXT"' EXIT HUP INT TERM
switch_current() {
  ln -s "$1" "$NEXT"
  mv -Tf "$NEXT" "$CURRENT"
}
# The running process stops under the unit it was started with (its
# TimeoutStopSec), so stop before loading another release's unit.
restore_previous() {
  systemctl stop "$INSTANCE.service" || true
  [ -n "$PREVIOUS" ] && [ -d "$PREVIOUS" ] || return 0
  switch_current "$PREVIOUS"
  use_unit "$PREVIOUS" || printf 'failed to restore the systemd unit of %s; manual recovery required\n' "${PREVIOUS##*/}" >&2
  systemctl start "$INSTANCE.service" || true
}
[ ! -f "$UNIT_PATH" ] || systemctl stop "$INSTANCE.service"
switch_current "$TARGET"
if ! use_unit "$TARGET"; then
  restore_previous
  printf 'failed to install the systemd unit of release %s; restored previous release\n' "$VERSION" >&2
  exit 1
fi

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
HEALTH_URL=${HEALTH_URL:-http://127.0.0.1:9100/readyz}
HEALTH_ATTEMPTS=${HEALTH_ATTEMPTS:-30}
if systemctl start "$INSTANCE.service"; then
  attempt=1
  while [ "$attempt" -le "$HEALTH_ATTEMPTS" ]; do
    if sh "$ROOT/deploy/shell/healthcheck.sh" "$HEALTH_URL" >/dev/null 2>&1; then
      printf 'rolled back %s to %s\n' "$INSTANCE" "$VERSION"
      exit 0
    fi
    attempt=$((attempt + 1))
    sleep 1
  done
fi
restore_previous
printf 'rollback target failed readiness; restored previous release\n' >&2
exit 1
