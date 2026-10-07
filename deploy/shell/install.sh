#!/usr/bin/env sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  printf 'install.sh must run as root\n' >&2
  exit 1
fi
if [ "$#" -lt 4 ] || [ "$#" -gt 5 ]; then
  printf 'usage: %s <service> <sid> <version> <production-config> [run-user]\n' "$0" >&2
  exit 2
fi

SERVICE=$1
SID=$2
VERSION=$3
CONFIG_SOURCE=$4
RUN_USER=${5:-roost}
case "$SERVICE" in *[!a-zA-Z0-9_-]*|'') printf 'invalid service: %s\n' "$SERVICE" >&2; exit 2;; esac
case "$SERVICE" in account|activity|chat|game|global|mail|match|platform|rank|session) ;; *) printf 'unknown service: %s (allowed: account activity chat game global mail match platform rank session)\n' "$SERVICE" >&2; exit 2;; esac
case "$SID" in *[!0-9]*|'0'|'') printf 'sid must be a positive integer\n' >&2; exit 2;; esac
case "$VERSION" in *[!a-zA-Z0-9._-]*|'') printf 'invalid version: %s\n' "$VERSION" >&2; exit 2;; esac
[ -f "$CONFIG_SOURCE" ] || { printf 'config not found: %s\n' "$CONFIG_SOURCE" >&2; exit 2; }

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
BINARY=${BINARY:-"$ROOT/dist/planet"}
[ -x "$BINARY" ] || { printf 'binary not found; run: sh deploy/shell/build.sh\n' >&2; exit 2; }
CONFIG_DATA=${CONFIG_DATA:-"$ROOT/configs/data"}
case "$SERVICE" in
  game)
    [ -f "$CONFIG_DATA/_manifest.json" ] || { printf 'config data not found: %s/_manifest.json; run make generate or set CONFIG_DATA\n' "$CONFIG_DATA" >&2; exit 2; }
    ;;
esac
ACTIVITY_GROUPS=${ACTIVITY_GROUPS:-"$ROOT/configs/activity_groups.yaml"}
[ -f "$ACTIVITY_GROUPS" ] || { printf 'activity groups file not found: %s; set ACTIVITY_GROUPS\n' "$ACTIVITY_GROUPS" >&2; exit 2; }

INSTANCE=planet-$SERVICE-$SID
APP_ROOT=${APP_ROOT:-/opt/roost/$INSTANCE}
STATE_ROOT=${STATE_ROOT:-/var/lib/roost/$INSTANCE}
LOG_ROOT=${LOG_ROOT:-/var/log/roost/$INSTANCE}
RELEASE_ROOT=$APP_ROOT/releases/$VERSION
UNIT_PATH=/etc/systemd/system/$INSTANCE.service
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
[ ! -e "$RELEASE_ROOT" ] || { printf 'release already exists and is immutable: %s\n' "$RELEASE_ROOT" >&2; exit 2; }

case "$SERVICE" in
  game)
    if ! grep -F "$STATE_ROOT/wal" "$CONFIG_SOURCE" >/dev/null 2>&1; then
      printf 'nest.wal.dir in %s must be %s/wal for service %s\n' "$CONFIG_SOURCE" "$STATE_ROOT" "$SERVICE" >&2
      exit 2
    fi
    ;;
esac

if ! getent passwd "$RUN_USER" >/dev/null 2>&1; then
  useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin "$RUN_USER"
fi
install -d -m 0755 "$APP_ROOT/releases" "$RELEASE_ROOT"
install -d -o "$RUN_USER" -g "$RUN_USER" -m 0750 "$STATE_ROOT/wal" "$LOG_ROOT"
install -m 0755 "$BINARY" "$RELEASE_ROOT/planet"
install -m 0640 -o root -g "$RUN_USER" "$CONFIG_SOURCE" "$RELEASE_ROOT/config.yaml"
# config_data.dir is relative (configs/data) and resolves under WorkingDirectory, the release.
case "$SERVICE" in
  game)
    install -d -m 0755 "$RELEASE_ROOT/configs"
    cp -R "$CONFIG_DATA" "$RELEASE_ROOT/configs/data"
    chmod -R u+rwX,go+rX,go-w "$RELEASE_ROOT/configs/data"
    ;;
esac
# activity.groups_file is relative (configs/activity_groups.yaml) and resolves under WorkingDirectory, the release.
install -d -m 0755 "$RELEASE_ROOT/configs"
install -m 0644 "$ACTIVITY_GROUPS" "$RELEASE_ROOT/configs/activity_groups.yaml"
# stats_log.dir is relative (log) and resolves under WorkingDirectory.
ln -sfn "$LOG_ROOT" "$RELEASE_ROOT/log"
(cd "$RELEASE_ROOT" && sha256sum planet config.yaml > SHA256SUMS && sha256sum -c SHA256SUMS >/dev/null)

# TimeoutStopSec: max(the service's generated shutdown.total_timeout, the total
# its configs set) + 5s. Run roost project sync after raising total_timeout.
case "$SERVICE" in
  account) STOP_TIMEOUT=28s ;;
  activity) STOP_TIMEOUT=28s ;;
  chat) STOP_TIMEOUT=28s ;;
  game) STOP_TIMEOUT=116s ;;
  global) STOP_TIMEOUT=28s ;;
  mail) STOP_TIMEOUT=28s ;;
  match) STOP_TIMEOUT=28s ;;
  platform) STOP_TIMEOUT=28s ;;
  rank) STOP_TIMEOUT=28s ;;
  session) STOP_TIMEOUT=28s ;;
esac

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
PREVIOUS=$(readlink -f "$APP_ROOT/current" 2>/dev/null || true)
record_unit "$PREVIOUS"
cat >"$APP_ROOT/units/$VERSION.service" <<EOF
[Unit]
Description=planet $SERVICE server (sid $SID)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$RUN_USER
Group=$RUN_USER
WorkingDirectory=$APP_ROOT/current
ExecStart=$APP_ROOT/current/planet $SERVICE --sid $SID --config $APP_ROOT/current/config.yaml
Restart=on-failure
RestartSec=2s
TimeoutStopSec=$STOP_TIMEOUT
KillSignal=SIGTERM
LimitNOFILE=1048576
UMask=0027
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
LockPersonality=true
RestrictSUIDSGID=true
RestrictRealtime=true
SystemCallArchitectures=native
ReadWritePaths=$STATE_ROOT $LOG_ROOT

[Install]
WantedBy=multi-user.target
EOF

switch_release() {
  target=$1
  next=$APP_ROOT/.current.$$
  rm -f "$next"
  ln -s "$target" "$next"
  mv -Tf "$next" "$APP_ROOT/current"
}

wait_ready() {
  attempt=1
  while [ "$attempt" -le "$HEALTH_ATTEMPTS" ]; do
    if sh "$ROOT/deploy/shell/healthcheck.sh" "$HEALTH_URL" >/dev/null 2>&1; then
      return 0
    fi
    attempt=$((attempt + 1))
    sleep 1
  done
  return 1
}

# Stop before switching: the running process stops under the unit it was
# started with (its TimeoutStopSec), not the one loaded for the next release.
[ ! -f "$UNIT_PATH" ] || systemctl stop "$INSTANCE.service"
switch_release "$RELEASE_ROOT"
if ! { use_unit "$RELEASE_ROOT" && systemctl enable "$INSTANCE.service"; }; then
  printf 'failed to install the systemd unit for %s version %s; rolling back\n' "$INSTANCE" "$VERSION" >&2
elif systemctl start "$INSTANCE.service" && wait_ready; then
  printf 'deployed %s version %s; inspect with: systemctl status %s.service\n' "$INSTANCE" "$VERSION" "$INSTANCE"
  exit 0
else
  printf 'deployment health check failed for %s version %s; rolling back\n' "$INSTANCE" "$VERSION" >&2
fi

systemctl stop "$INSTANCE.service" || true
if [ -n "$PREVIOUS" ] && [ -d "$PREVIOUS" ]; then
  switch_release "$PREVIOUS"
  use_unit "$PREVIOUS" || printf 'failed to restore the systemd unit of %s\n' "${PREVIOUS##*/}" >&2
  systemctl start "$INSTANCE.service" || true
  if ! wait_ready; then
    printf 'rollback also failed readiness; manual recovery required\n' >&2
  fi
fi
exit 1
