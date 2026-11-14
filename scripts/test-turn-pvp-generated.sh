#!/usr/bin/env bash
set -euo pipefail
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
ROOST_GENRE_OUTPUT="$review_root" bash "$repo_root/scripts/testdata/genre-common/run.sh" turn-pvp attack
module="${ROOST_GENRE_MODULE:-example.com/turn-pvp-practice}"
sed "s|TURN_MODULE|$module|g" "$repo_root/scripts/testdata/turn-pvp-real/timer_test.go.txt" >"$review_root/project/game/handler/turn_timer_real_test.go"
gofmt -w "$review_root/project/game/handler/turn_timer_real_test.go"
cd "$review_root/project"
# 默认档也编译 / vet 恢复夹具；只有显式 REAL 才接真实存储，不能以 SKIP 冒充验收。
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
  # 配方的 Room/Turn 模块和上述 GameRoom 纵切是两套业务模型，分别实跑，互不替代。
  bash "$repo_root/scripts/test-turn-pvp-real.sh"
fi
succeeded=1
