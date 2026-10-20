#!/usr/bin/env bash
# 放置模拟业务接入纵切：正式生成器、Player DAO、Player 实体定时器与 Nest。业务规则（三工坊生产队列、原料预扣与
# 取消退款、收益窗口与倍率积分、产出缓冲与仓库、出售 / 订单 / 每日奖励）只在 scripts/testdata/idle-sim，
# 不是框架默认玩法，生成器也不生成这些文件。
# 缺省不连接外部存储：提交器是替身。ROOST_IDLE_SIM_REAL=1（并提供隔离 Mongo 副本集
# ROOST_DATAENGINE_IT_MONGO_URI）时追加：子进程 strict 提交后、WAL 记录未投影时被父进程 SIGKILL，新进程
# 冷加载、由驱动补算一次离线收益、领取结算，再由第三个进程冷加载核对并重试。使用本次唯一库，成功后删除。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-idle-sim.XXXXXX")"
project_root="$review_root/project"
echo "Idle sim generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_IDLE_SIM_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ "$succeeded" == 1 && "${ROOST_IDLE_SIM_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "Idle sim generated consumer kept: $review_root" >&2
    # 真实依赖模式失败时 DAO 库与 WAL 留作证据，查完手动删除。
    if [[ "${ROOST_IDLE_SIM_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then
      echo "Idle sim real-persistence database left for inspection: ${ROOST_IDLE_SIM_DB:-unset}" >&2
    fi
  fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new idle-sim-practice -module example.com/idle-sim-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
# 先指向本检出：后面 generate 里的 go mod tidy 不去解析可能尚未发布的最低 core 版本，生成物也按本检出编译。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_IDLE_SIM_DB="roost_idle_sim_it_${PPID}_$$"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity Player >> "$review_root/generate.log" 2>&1
# DAO 定义是业务文件；只修改它，生成组件、访问器与 Sender 由 CLI 生成。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys, os
root = Path(sys.argv[1])
p = root / 'db/def/player.go'
fields = [('State', 'string', 'state'), ('WakeTimer', 'int64', 'wake_timer')]
marker = 'type PlayerDao struct {'
text = p.read_text().replace(marker, marker + '\n' + ''.join(f'{field} {typ} `bson:"{key}" dao:"persist"`\n' for field, typ, key in fields))
text = text.replace('db=idle_sim_practice', 'db=' + os.environ['ROOST_IDLE_SIM_DB'])
p.write_text(text)
DEFS
mkdir -p game/idlesim
for name in rules state production; do cp "$repo_root/scripts/testdata/idle-sim/$name.go.txt" "game/idlesim/$name.go"; done
cp "$repo_root/scripts/testdata/idle-sim/rules_test.go.txt" game/idlesim/rules_test.go
cp "$repo_root/scripts/testdata/idle-sim/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$repo_root/scripts/testdata/idle-sim/handlers.go.txt" game/handler/idle_sim.go
cp "$repo_root/scripts/testdata/idle-sim/gameplay_test.go.txt" game/handler/idle_sim_test.go
gofmt -w db/def/player.go game/idlesim/*.go game/entities/player/timer_handlers.go game/handler/idle_sim*.go
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestIdleSim' ./game/idlesim ./game/handler | tee "$review_root/test.log"
# 12 条场景必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestIdleSim' "$review_root/test.log" || true)"
if [[ "$passed" != 12 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 12 passing TestIdleSim scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
if [[ "${ROOST_IDLE_SIM_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  for path in "$repo_root"/scripts/testdata/idle-sim/persistence/*_test.go; do cp "$path" "game/handler/idle_sim_$(basename "$path")"; done
  gofmt -w game/handler/idle_sim_*_test.go
  export ROOST_IDLE_SIM_WAL="$review_root/persistence"
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestIdleSimRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
  if [[ "$(grep -c '^--- PASS: TestIdleSimRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
