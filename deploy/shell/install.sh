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

INSTANCE=planet-$SERVICE-$SID
APP_ROOT=${APP_ROOT:-/opt/roost/$INSTANCE}
STATE_ROOT=${STATE_ROOT:-/var/lib/roost/$INSTANCE}
LOG_ROOT=${LOG_ROOT:-/var/log/roost/$INSTANCE}
RELEASE_ROOT=$APP_ROOT/releases/$VERSION
UNIT_PATH=/etc/systemd/system/$INSTANCE.service
HEALTH_URL=${HEALTH_URL:-http://127.0.0.1:9100/readyz}
HEALTH_ATTEMPTS=${HEALTH_ATTEMPTS:-30}
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
(cd "$RELEASE_ROOT" && sha256sum planet config.yaml > SHA256SUMS && sha256sum -c SHA256SUMS >/dev/null)

cat >"$UNIT_PATH" <<EOF
[Unit]
Description=planet $SERVICE server (sid $SID)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$RUN_USER
Group=$RUN_USER
WorkingDirectory=$APP_ROOT
ExecStart=$APP_ROOT/current/planet $SERVICE --sid $SID --config $APP_ROOT/current/config.yaml
Restart=on-failure
RestartSec=2s
TimeoutStopSec=45s
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

PREVIOUS=$(readlink -f "$APP_ROOT/current" 2>/dev/null || true)
switch_release "$RELEASE_ROOT"
systemctl daemon-reload
systemctl enable "$INSTANCE.service"
if systemctl restart "$INSTANCE.service" && wait_ready; then
  printf 'deployed %s version %s; inspect with: systemctl status %s.service\n' "$INSTANCE" "$VERSION" "$INSTANCE"
  exit 0
fi

printf 'deployment health check failed for %s version %s; rolling back\n' "$INSTANCE" "$VERSION" >&2
if [ -n "$PREVIOUS" ] && [ -d "$PREVIOUS" ]; then
  switch_release "$PREVIOUS"
  systemctl restart "$INSTANCE.service" || true
  if ! wait_ready; then
    printf 'rollback also failed readiness; manual recovery required\n' >&2
  fi
else
  systemctl stop "$INSTANCE.service" || true
fi
exit 1
