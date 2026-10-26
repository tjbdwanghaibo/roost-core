#!/usr/bin/env bash
# 原生 PhysX 与 Unity PhysicsComparison 的对照验收宿主（只监听 loopback）。房间 / Gate 链路走正式服务，
# 见 wiring/physicsroom/cmd/physicsroom-local。
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"
if [[ ! -f infra/physics/physx/lib/libroost_physx.dylib && ! -f infra/physics/physx/lib/libroost_physx.so ]]; then
 echo 'Build PhysX first: scripts/physics/build-native.sh' >&2;exit 1
fi
mkdir -p artifacts
GOWORK=off go build -tags physx -o artifacts/physics-comparison-host ./scripts/physics/comparison-host
exec artifacts/physics-comparison-host "$@"
