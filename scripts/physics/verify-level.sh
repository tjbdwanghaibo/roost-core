#!/usr/bin/env bash
# 三进程联机演练：physicsroom-local 的 physicsroom / game / gate 各起一个进程，game 用导出的示例关卡（带流程），
# level-drill 客户端经真实 Gate 走完“检查点 → 掉落 → 重生 → 终点”；然后依次 SIGTERM gate、game、physicsroom，
# 要求三个进程都以 0 退出、日志里没有 WARN / ERROR。
#
# 给一个参数（create-viewer.sh 建好、脚本是当前版本的 Unity 工程的绝对路径）时，客户端换成 Unity 观察器：
# 批处理模式进 Play Mode 执行 ViewerProject.VerifyLevel（1 号槽自动走出地面，要求收到流程视图并被重生）。
#
# 需要一个 NATS 和一个 etcd（不会去动它们的其他数据：键前缀与 subject 前缀带本次运行的进程号）：
#   ROOST_PHYSICS_TEST_NATS_URL=nats://127.0.0.1:4222 ROOST_PHYSICS_TEST_ETCD=127.0.0.1:2379 scripts/physics/verify-level.sh
# Gate 的监听地址 127.0.0.1:19093 写在 physicsroom-local 里；端口被占用时本脚本直接退出，不停止已有进程。
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
nats=${ROOST_PHYSICS_TEST_NATS_URL:?set ROOST_PHYSICS_TEST_NATS_URL, e.g. nats://127.0.0.1:4222}
etcd=${ROOST_PHYSICS_TEST_ETCD:?set ROOST_PHYSICS_TEST_ETCD, e.g. 127.0.0.1:2379}
slot=${ROOST_PHYSICS_DRILL_SLOT:-1}
project=${1:-}
unity=${UNITY_EDITOR:-/Applications/Unity/Hub/Editor/6000.3.12f1/Unity.app/Contents/MacOS/Unity}
output="$root/artifacts/physics-level-drill/$(date +%Y%m%d-%H%M%S)"
if nc -z 127.0.0.1 19093 2>/dev/null; then
 echo '127.0.0.1:19093 is in use; stop the other Gate first' >&2; exit 1
fi
mkdir -p "$output"
cd "$root"
CGO_ENABLED=1 GOWORK=off go build -tags physx -o "$output/physicsroom-local" ./wiring/physicsroom/cmd/physicsroom-local
GOWORK=off go build -o "$output/level-drill" ./scripts/physics/level-drill
for name in physicsroom game gate; do
 sed -e "s#nats://127.0.0.1:4222#$nats#" -e "s#127.0.0.1:2379#$etcd#" -e "s#roost-physics-local#roost-physics-drill-$$#g" \
  "wiring/physicsroom/cmd/physicsroom-local/configs/config.$name.yaml" > "$output/config.$name.yaml"
done
"$output/physicsroom-local" export-level "$output/obstacles.json"

pids=()
stop_all(){ for pid in "${pids[@]:-}"; do [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap stop_all EXIT
"$output/physicsroom-local" physicsroom --config "$output/config.physicsroom.yaml" > "$output/physicsroom.log" 2>&1 &
pids+=($!)
"$output/physicsroom-local" game --level "$output/obstacles.json" --config "$output/config.game.yaml" > "$output/game.log" 2>&1 &
pids+=($!)
"$output/physicsroom-local" gate --config "$output/config.gate.yaml" > "$output/gate.log" 2>&1 &
pids+=($!)
ready=false
for _ in {1..100}; do
 for pid in "${pids[@]}"; do
  kill -0 "$pid" 2>/dev/null || { echo 'a process exited during startup' >&2; tail -5 "$output"/*.log >&2; exit 1; }
 done
 if nc -z 127.0.0.1 19093 2>/dev/null; then ready=true; break; fi
 sleep .1
done
[[ "$ready" == true ]] || { echo 'Gate startup timeout' >&2; exit 1; }
sleep 1 # Gate 监听之后还要从 etcd 发现 game

result=0
if [[ -n "$project" ]]; then
 "$unity" -batchmode -nographics -projectPath "$project" -executeMethod ViewerProject.VerifyLevel -logFile "$output/unity.log" || result=1
 grep -h 'ROOST_VIEWER_TCP_PHYSICS_' "$output/unity.log" | tee "$output/drill.log" || result=1
else
 "$output/level-drill" -slot "$slot" | tee "$output/drill.log" || result=1
fi

# 依次停：先 Gate（断开玩家），再 Game，最后房间 owner。
names=(physicsroom game gate)
for index in 2 1 0; do
 kill -TERM "${pids[$index]}"
 code=0; wait "${pids[$index]}" || code=$?
 printf '%s exit=%d\n' "${names[$index]}" "$code" | tee -a "$output/drill.log"
 [[ "$code" == 0 ]] || result=1
 pids[$index]=''
done
if grep -n -E 'level=(WARN|ERROR)|"level":"(WARN|ERROR)"|panic:' "$output"/physicsroom.log "$output"/game.log "$output"/gate.log > "$output/problems.log"; then
 echo 'WARN / ERROR in the service logs:' >&2; head -20 "$output/problems.log" >&2; result=1
fi
grep -q -E 'LEVEL_DRILL_PASS|ROOST_VIEWER_TCP_PHYSICS_PASS' "$output/drill.log" || result=1
printf 'Evidence: %s\n' "$output"
exit "$result"
