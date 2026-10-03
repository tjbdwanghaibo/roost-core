#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
COMPOSE=$ROOT/deploy/docker/docker-compose.prod.yaml
STATE_DIR=${ROOST_DEPLOY_STATE_DIR:-$ROOT/.roost-deploy}
ENV_FILE=${ROOST_ENV_FILE:-$ROOT/deploy/docker/.env.production}
if [ -z "${ROOST_IMAGE:-}" ]; then
  [ -f "$STATE_DIR/previous-image" ] || { printf 'no previous image recorded\n' >&2; exit 2; }
  ROOST_IMAGE=$(sed -n '1p' "$STATE_DIR/previous-image")
fi
case "$ROOST_IMAGE" in *@sha256:*) ;; *) printf 'rollback image must use an immutable digest\n' >&2; exit 2;; esac

cd "$ROOT"
export ROOST_IMAGE
docker compose --env-file "$ENV_FILE" -f "$COMPOSE" pull
docker compose --env-file "$ENV_FILE" -f "$COMPOSE" up -d --remove-orphans --wait --wait-timeout 120
printf '%s\n' "$ROOST_IMAGE" > "$STATE_DIR/current-image"
printf 'rolled back planet to %s\n' "$ROOST_IMAGE"
