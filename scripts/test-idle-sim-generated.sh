#!/usr/bin/env bash
# 放置模拟业务接入纵切：正式生成器 + 业务模块（R9 批 1）+ 只手写模块没覆盖的部分。
# 模块：enter（建档；初始金币在 event.EnterFirst 的 Player 监听里发，初始宝石随放置存档一笔发）、wallet（金币）、bag（宝石是道具，Q2 B）、
# checkin（每日奖励：业务日、连续天数加成与“今天领过没有”）。
# 手写（scripts/testdata/idle-sim，样板 idle 模块“游标只前移整周期 + 单调序号 + 回拨拒绝”）：三工坊生产队列、
# 原料预扣与取消退款、收益窗口与倍率积分、产出缓冲与仓库容量、出售 / 订单、配置冻结写进存档、Player 上的唤醒定时器。
# 这些不是框架默认玩法，生成器也不生成这些文件。
# 缺省不连接外部存储：提交器是替身。ROOST_IDLE_SIM_REAL=1（并提供隔离 Mongo 副本集
# ROOST_DATAENGINE_IT_MONGO_URI）时追加：子进程提交（业务命令缺省 async）后、WAL 记录未投影时被父进程 SIGKILL，新进程
# 冷加载、由驱动投递循环补算一次离线收益并按时投递下一秒的刷新、领取结算，再由第三个进程冷加载核对并重试。使用本次唯一库，成功后删除。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="${ROOST_GENRE_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/roost-idle-sim.XXXXXX")}"
mkdir -p "$review_root"
project_root="$review_root/project"
echo "Idle sim generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_IDLE_SIM_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ -z "${ROOST_GENRE_OUTPUT:-}" && "$succeeded" == 1 && "${ROOST_IDLE_SIM_KEEP:-0}" != 1 ]]; then
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
# 模块清单写在脚本里（Q8：不建配方）；依赖由安装器连带装上，Player 的主 DAO、玩家 TCP 接入由 plan 代办。
genre_modules=(enter wallet bag checkin)
modules_sh="$repo_root/scripts/testdata/genre-common/modules.sh"
bash "$modules_sh" add "$review_root/roost" "${genre_modules[@]}" >> "$review_root/generate.log" 2>&1
# 生产与收益的唤醒是 Player 上的实体定时器（这几个模块都没有定时器）。
"$review_root/roost" add timer --entity Player >> "$review_root/generate.log" 2>&1
# 放置存档是 Player 主 DAO 自己的字段（追加在模块字段之后，字段名带玩法前缀）；宝石是道具表的一行（Q2 B）。
# DAO 定义与道具表是业务文件；生成组件、访问器、Sender 与 configs/data 由 CLI 生成。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys
root = Path(sys.argv[1])
p = root / 'db/def/player.go'
marker = 'type PlayerDao struct {'
text = p.read_text()
assert marker in text and 'db=main' in text, p  # 骨架写逻辑库 main；本次运行的唯一物理库由测试夹具的映射给出
fields = (
    '\n\t// IdleState 是放置模拟的存档（冻结的规则、原料、工坊队列、收益窗口、命令回执），严格 JSON。\n'
    '\tIdleState string `store:"idle_state" dao:"nosync"`\n'
    '\t// IdleWakeTimer 是下一次生产 / 收益唤醒在 Player 定时器里的节点号；0 表示没有期限。\n'
    '\tIdleWakeTimer int64 `store:"idle_wake_timer" dao:"nosync"`\n'
)
end = text.index('\n}', text.index(marker))
p.write_text(text[:end] + '\n' + fields + text[end:])
items = root / 'configs/table/item.csv'
rows = items.read_text()
assert rows.startswith('id,name,max_stack'), items
items.write_text(rows.rstrip('\n') + '\n1101,宝石,0\n')  # 与 idlesim.GemItem 一致
DEFS
fixture="$repo_root/scripts/testdata/idle-sim"
mkdir -p game/modules/idlesim
for name in rules state production; do cp "$fixture/$name.go.txt" "game/modules/idlesim/$name.go"; done
cp "$fixture/rules_test.go.txt" game/modules/idlesim/rules_test.go
cp "$fixture/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$fixture/first_enter.go.txt" game/entities/player/idle_sim.go
cp "$fixture/handlers.go.txt" game/handler/idle_sim.go
cp "$fixture/gameplay_test.go.txt" game/handler/idle_sim_test.go
gofmt -w db/def/player.go game/modules/idlesim/*.go game/entities/player/*.go game/handler/idle_sim*.go
python3 "$repo_root/scripts/testdata/robot-common/install.py" "$repo_root" "$project_root" idle-sim
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestIdleSim' ./game/modules/idlesim ./game/handler | tee "$review_root/test.log"
# 13 条场景（含业务 Robot）必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestIdleSim' "$review_root/test.log" || true)"
if [[ "$passed" != 13 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 13 passing TestIdleSim scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
# 已装模块自带的场景照跑：每个模块通过数 == 本检出 modules/<名>/module.yaml 的 verify.pass、0 skip（结果 module-verify.json）。
bash "$modules_sh" verify "$review_root/roost" "$repo_root" "$review_root/module-verify.json" "${genre_modules[@]}" | tee "$review_root/module-verify.txt"
# 真实依赖用例缺省模式也复制进工程并 vet（只编译不运行）：生成器签名变了，发版门禁里就能看到这些夹具编译失败。
for path in "$repo_root"/scripts/testdata/idle-sim/persistence/*_test.go; do cp "$path" "game/handler/idle_sim_$(basename "$path")"; done
gofmt -w game/handler/idle_sim_*_test.go
go vet -mod=mod -p 2 ./game/handler
if [[ "${ROOST_IDLE_SIM_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  export ROOST_IDLE_SIM_WAL="$review_root/persistence"
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestIdleSimRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
  if [[ "$(grep -c '^--- PASS: TestIdleSimRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
