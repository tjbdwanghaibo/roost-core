#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
OUTPUT_DIR=${OUTPUT_DIR:-"$ROOT/dist"}
GOOS=${GOOS:-linux}
GOARCH=${GOARCH:-amd64}
VERSION=${VERSION:-dev}
COMMIT=${COMMIT:-$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || printf unknown)}
BUILD_TIME=${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}

mkdir -p "$OUTPUT_DIR"
cd "$ROOT"
GOWORK=off go mod verify
CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath \
  -ldflags "-s -w -X github.com/tjbdwanghaibo/roost-core/app/buildinfo.Version=$VERSION -X github.com/tjbdwanghaibo/roost-core/app/buildinfo.Commit=$COMMIT -X github.com/tjbdwanghaibo/roost-core/app/buildinfo.BuildTime=$BUILD_TIME" \
  -o "$OUTPUT_DIR/planet" .
printf 'built %s\n' "$OUTPUT_DIR/planet"
