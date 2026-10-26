#!/usr/bin/env bash
# 原生物理 + Core NATS + Gate/TCP 的专用验收，未配置依赖时明确失败，不以 SKIP 充当通过。
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
: "${ROOST_PHYSICS_TEST_NATS_URL:?Set a private NATS URL for physics service validation}"
export ROOST_DATAENGINE_IT_NATS_URL="$ROOST_PHYSICS_TEST_NATS_URL"
cd "$root"
export GOWORK=off
[[ $(go env CGO_ENABLED) == 1 ]] || { echo "PhysX validation requires CGO_ENABLED=1" >&2; exit 1; }
go test -race -tags=physx -count=3 ./service/physicsroom ./wiring/physicsroom ./infra/network/servicerpc ./framework/nest ./infra/physics/physx ./gameplay/physics ./cmd/physics-viewer
go test -tags=integration,physx -race -count=3 -run 'TestRealNATSRoomLifecycle|TestRealGatePhysicsRoomRelay|TestRealGateUGCLevelAndInput' ./service/physicsroom ./wiring/gate ./cmd/physics-viewer
