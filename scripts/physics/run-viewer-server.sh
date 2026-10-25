#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"
if [[ ! -f infra/physics/physx/lib/libroost_physx.dylib && ! -f infra/physics/physx/lib/libroost_physx.so ]]; then
 echo 'Build PhysX first: scripts/physics/build-native.sh' >&2;exit 1
fi
mkdir -p artifacts
GOWORK=off go build -tags physx -o artifacts/physics-viewer ./cmd/physics-viewer
exec artifacts/physics-viewer "$@"
