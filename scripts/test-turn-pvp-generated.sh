#!/usr/bin/env bash
# 回合 PvP：turn（轮到谁、命令序号与重放、持久回合期限与超时推进；连带 room 的席位、代际、连接与在线，
# GameRoom 并入 Room，Q3 A）+ wallet（命中奖励）。
# 手写（scripts/testdata/turn-pvp）：生命、胜负、带内容的攻击回执、名单准入（RoomJoining 监听），以及对 turn 组件的两处
# 扩展（超时扣行动方生命、分出胜负即结束；turn README“超时要做别的事改 OnDeadline”）。
# 纵切与配方 -recipe turn-pvp 从此是同一套 Room / Turn 模型（Q7 A）：REAL 档的定时器用例测 Room 上 turn 的 TurnDeadline。
set -euo pipefail
genre_modules=(turn wallet)
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="${ROOST_GENRE_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/roost-turn-pvp.XXXXXX")}"
succeeded=0
timer_database=""
cleanup() {
  if [[ -n "$timer_database" ]]; then
    mongosh --quiet "$ROOST_DATAENGINE_IT_MONGO_URI" --eval "db.getSiblingDB('$timer_database').dropDatabase(); db.getSiblingDB('${timer_database}_0').dropDatabase()" >/dev/null 2>&1 || true
  fi
  if [[ "$succeeded" == 1 && -z "${ROOST_GENRE_OUTPUT:-}" && "${ROOST_GENRE_KEEP:-0}" != 1 ]]; then rm -rf "$review_root"; else echo "Generated consumer and evidence: $review_root"; fi
}
trap cleanup EXIT
export GOWORK=off
ROOST_GENRE_OUTPUT="$review_root" GENRE_MODULES="${genre_modules[*]}" bash "$repo_root/scripts/testdata/genre-common/run.sh" turn-pvp attack
module="${ROOST_GENRE_MODULE:-example.com/turn-pvp-practice}"
sed "s|TURN_MODULE|$module|g" "$repo_root/scripts/testdata/turn-pvp-real/timer_test.go.txt" >"$review_root/project/game/handler/turn_timer_real_test.go"
gofmt -w "$review_root/project/game/handler/turn_timer_real_test.go"
cd "$review_root/project"
# 默认档也编译 / vet 恢复夹具；只有显式 REAL 才接真实存储，不能以 SKIP 冒充验收。
# 名字虽以 TestTurn 开头，turn 的 module verify 只运行、只统计 turn 装进工程的测试文件里的用例（维护者 2026-11-17），
# 不会把它缺省档的 SKIP 算进 turn 的条数，所以不需要构建标签。
go test -mod=mod -run '^$' ./game/handler
go vet -mod=mod ./game/handler
if [[ "${ROOST_TURN_PVP_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo replica set required}"
  command -v mongosh >/dev/null
  timer_database="roost_turn_it_$(date +%s)_$$"
  export TURN_TIMER_DATABASE="$timer_database" TURN_TIMER_ROOT="$review_root/timer" ROOST_TURN_PVP_REAL=1
  mkdir -p "$TURN_TIMER_ROOT"
  for phase in seed recover; do
    TURN_TIMER_PHASE="$phase" go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestTurnPVPRealTimerRecovery$' ./game/handler >"$review_root/timer-$phase.log" 2>&1 || { cat "$review_root/timer-$phase.log"; exit 1; }
    grep -q '^--- PASS: TestTurnPVPRealTimerRecovery' "$review_root/timer-$phase.log"
    if grep -q -- '--- SKIP:' "$review_root/timer-$phase.log"; then echo "unexpected timer SKIP" >&2; exit 1; fi
    grep 'PASS real timer' "$review_root/timer-$phase.log"
  done
  # 纵切与配方用同一套 Room / Turn 模型（Q7 A）；配方另验真实登录 / 匹配 / Gate / NATS 与 SIGKILL 重启，两者分别计数。
  bash "$repo_root/scripts/test-turn-pvp-real.sh"
fi
succeeded=1
