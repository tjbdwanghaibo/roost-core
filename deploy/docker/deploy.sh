#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
COMPOSE=$ROOT/deploy/docker/docker-compose.prod.yaml
STATE_DIR=${ROOST_DEPLOY_STATE_DIR:-$ROOT/.roost-deploy}
ENV_FILE=${ROOST_ENV_FILE:-$ROOT/deploy/docker/.env.production}
: "${ROOST_IMAGE:?ROOST_IMAGE must contain an immutable ghcr.io image digest}"
case "$ROOST_IMAGE" in *@sha256:*) ;; *) printf 'ROOST_IMAGE must use an immutable digest\n' >&2; exit 2;; esac
[ -f "$ENV_FILE" ] || { printf 'environment file not found: %s\n' "$ENV_FILE" >&2; exit 2; }

mkdir -p "$STATE_DIR"
PREVIOUS=
if [ -f "$STATE_DIR/current-image" ]; then
  PREVIOUS=$(sed -n '1p' "$STATE_DIR/current-image")
fi
printf '%s\n' "$ROOST_IMAGE" > "$STATE_DIR/pending-image"

cd "$ROOT"
export ROOST_IMAGE
docker compose --env-file "$ENV_FILE" -f "$COMPOSE" config --quiet
docker compose --env-file "$ENV_FILE" -f "$COMPOSE" pull
if docker compose --env-file "$ENV_FILE" -f "$COMPOSE" up -d --remove-orphans --wait --wait-timeout 120; then
  mv "$STATE_DIR/pending-image" "$STATE_DIR/current-image"
  [ -z "$PREVIOUS" ] || printf '%s\n' "$PREVIOUS" > "$STATE_DIR/previous-image"
  printf 'deployed planet image %s\n' "$ROOST_IMAGE"
  exit 0
fi

printf 'deployment failed readiness\n' >&2
rm -f "$STATE_DIR/pending-image"
if [ -n "$PREVIOUS" ]; then
  ROOST_IMAGE=$PREVIOUS sh "$ROOT/deploy/docker/rollback.sh"
fi
exit 1
