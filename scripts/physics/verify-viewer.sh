#!/usr/bin/env bash
# 独占 19091 端口；不停止已有进程。只清理由本脚本启动的宿主。
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
project=${1:?Usage: verify-viewer.sh /absolute/unity/project [20|50]}
hz=${2:-20}
unity=${UNITY_EDITOR:-/Applications/Unity/Hub/Editor/6000.3.12f1/Unity.app/Contents/MacOS/Unity}
output="$root/artifacts/physics-validation/$(date +%Y%m%d-%H%M%S)-$hz"
mkdir -p "$output"
cd "$root"
GOWORK=off go build -tags physx -o artifacts/physics-comparison-host ./scripts/physics/comparison-host
artifacts/physics-comparison-host -send-hz "$hz" -duration 90s > "$output/host.log" 2>&1 &
server_pid=$!
cleanup(){ if jobs -pr | grep -qx "$server_pid"; then kill "$server_pid"; fi; wait "$server_pid" 2>/dev/null || true; }
trap cleanup EXIT
ready=false
for _ in {1..50}; do
 if grep -q 'physics viewer 127.0.0.1:19091' "$output/host.log"; then ready=true; break; fi
 if ! kill -0 "$server_pid" 2>/dev/null; then cat "$output/host.log" >&2; exit 1; fi
 sleep .1
done
[[ "$ready" == true ]] || { echo 'Server startup timeout' >&2; exit 1; }
"$unity" -batchmode -projectPath "$project" -executeMethod ViewerProject.VerifyConnection -logFile "$output/unity.log"
grep 'ROOST_VIEWER_TCP_PHYSICS_PASS' "$output/unity.log"
printf 'Evidence: %s\n' "$output"
