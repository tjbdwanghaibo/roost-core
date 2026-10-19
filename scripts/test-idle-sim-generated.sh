#!/usr/bin/env bash
# 放置 / 模拟经营正式生成工程：三工坊、收益窗口、倍率积分、跨天奖励与持久生产计时。
# 缺省提交器为替身；ROOST_IDLE_SIM_REAL=1 增加独立 Mongo / 文件 WAL 的 SIGKILL 恢复生产队列、离线收益与领奖重放。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-idle-sim.XXXXXX")"
succeeded=0
cleanup() {
 if [[ "$succeeded" == 1 && "${ROOST_IDLE_SIM_KEEP:-0}" != 1 ]]; then rm -rf "$review_root"; else
  echo "Idle sim consumer kept: $review_root" >&2
  if [[ "${ROOST_IDLE_SIM_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then echo "Idle sim database namespace for diagnosis: ${ROOST_IDLE_SIM_DB:-unset}" >&2; fi
 fi
}
trap cleanup EXIT
export GOWORK=off GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
echo "Idle sim generated consumer and logs: $review_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new idle-sim-practice -module example.com/idle-sim-practice -out "$review_root/project" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$review_root/project"
# 后续 add / generate 也始终使用本检出；消费者不修改 core 自己的 go.mod / go.sum。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_IDLE_SIM_DB="roost_idle_sim_it_${PPID}_$$"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity Player >> "$review_root/generate.log" 2>&1
python3 - <<'DEFS'
from pathlib import Path
import os
p=Path('db/def/player.go');text=p.read_text().replace('type PlayerDao struct {','type PlayerDao struct {\nState string `bson:"state" dao:"persist"`\nWakeTimer int64 `bson:"wake_timer" dao:"persist"`\n').replace('db=idle_sim_practice','db='+os.environ['ROOST_IDLE_SIM_DB']);p.write_text(text)
DEFS
mkdir -p game/idlesim
for name in rules state production; do cp "$repo_root/scripts/testdata/idle-sim/$name.go.txt" "game/idlesim/$name.go"; done
cp "$repo_root/scripts/testdata/idle-sim/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$repo_root/scripts/testdata/idle-sim/handlers.go.txt" game/handler/idle_sim.go
cp "$repo_root/scripts/testdata/idle-sim/gameplay_test.go.txt" game/handler/idle_sim_test.go
cp "$repo_root/scripts/testdata/idle-sim/rules_test.go.txt" game/idlesim/rules_test.go
gofmt -w db/def/player.go game/idlesim/*.go game/entities/player/timer_handlers.go game/handler/idle_sim*.go
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestIdleSim' ./game/idlesim ./game/handler | tee "$review_root/test.log"
passed="$(grep -c '^--- PASS: TestIdleSim' "$review_root/test.log" || true)"
if [[ "$passed" != 12 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then echo "expected 12 passing Idle sim scenarios, got $passed" >&2; exit 1; fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
if [[ "${ROOST_IDLE_SIM_REAL:-0}" == 1 ]]; then
 : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
 for path in "$repo_root"/scripts/testdata/idle-sim/persistence/*_test.go; do cp "$path" "game/handler/idle_sim_$(basename "$path")"; done
 export ROOST_IDLE_SIM_WAL="$review_root/persistence"
 go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestIdleSimRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
 if [[ "$(grep -c '^--- PASS: TestIdleSimRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
 go vet -mod=mod -p 2 ./...
fi
succeeded=1
