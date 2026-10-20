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
# Each release runs under the systemd unit it was installed with: install.sh
# writes it to $APP_ROOT/units/<version>.service, and it is switched together
# with current.
require_unit() {
  if [ -z "$1" ] || [ -f "$APP_ROOT/units/${1##*/}.service" ]; then
    return 0
  fi
  printf 'no systemd unit recorded for release %s: it was installed before releases recorded their unit; stop %s.service, remove %s/current and %s/releases, then install again\n' "${1##*/}" "$INSTANCE" "$APP_ROOT" "$APP_ROOT" >&2
  return 1
}
use_unit() {
  require_unit "$1" || return 1
  install -m 0644 "$APP_ROOT/units/${1##*/}.service" "$UNIT_PATH"
  systemctl daemon-reload
}
require_unit "$PREVIOUS" || exit 1
require_unit "$TARGET" || exit 1
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
# Readiness attempts, one a second. A service with singleton.enabled may first wait up to
# singleton.startup_wait for the lock a previous process still holds, then replay its WAL
# (dataengine.startup_timeout); raise HEALTH_ATTEMPTS when raising either.
case "$SERVICE" in
  account) DEFAULT_HEALTH_ATTEMPTS=30 ;;
  activity) DEFAULT_HEALTH_ATTEMPTS=30 ;;
  chat) DEFAULT_HEALTH_ATTEMPTS=30 ;;
  game) DEFAULT_HEALTH_ATTEMPTS=60 ;;
  global) DEFAULT_HEALTH_ATTEMPTS=30 ;;
  mail) DEFAULT_HEALTH_ATTEMPTS=30 ;;
  match) DEFAULT_HEALTH_ATTEMPTS=30 ;;
  platform) DEFAULT_HEALTH_ATTEMPTS=30 ;;
  rank) DEFAULT_HEALTH_ATTEMPTS=30 ;;
  session) DEFAULT_HEALTH_ATTEMPTS=30 ;;
  *) DEFAULT_HEALTH_ATTEMPTS=30 ;;
esac
HEALTH_ATTEMPTS=${HEALTH_ATTEMPTS:-$DEFAULT_HEALTH_ATTEMPTS}
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
