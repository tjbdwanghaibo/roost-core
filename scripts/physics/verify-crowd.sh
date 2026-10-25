#!/usr/bin/env bash
# 真实 20 控制连接 + Unity 旁观验收；该轮资源数据含编辑器竞争，不作为性能结果。
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
project=${1:?Usage: verify-crowd.sh /absolute/unity/project}
unity=${UNITY_EDITOR:-/Applications/Unity/Hub/Editor/6000.3.12f1/Unity.app/Contents/MacOS/Unity}
output="$root/artifacts/physics-crowd/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$output"
cd "$root"
GOWORK=off go build -tags physx -o artifacts/physics-viewer ./cmd/physics-viewer
python3 scripts/physics/measure-crowd.py --seconds 45 --hz 20 --port 19091 --output "$output" > "$output/clients.log" 2>&1 &
load_pid=$!
# 正常等待负载进程结束，由其负责宿主与客户端的排空；不停止未知端口持有者。
cleanup(){ wait "$load_pid" 2>/dev/null || true; }
trap cleanup EXIT
ready=false
for _ in {1..50}; do
 if [[ -f "$output/host.log" ]] && grep -q 'physics viewer 127.0.0.1:19091' "$output/host.log"; then ready=true; break; fi
 sleep .1
done
[[ "$ready" == true ]] || { echo 'Crowd startup timeout' >&2; exit 1; }
"$unity" -batchmode -projectPath "$project" -executeMethod ViewerProject.VerifyCrowd -logFile "$output/unity.log"
grep 'ROOST_VIEWER_TCP_PHYSICS_PASS' "$output/unity.log"
wait "$load_pid"
printf 'Evidence: %s\n' "$output"
