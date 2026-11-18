#!/usr/bin/env bash
# 合作塔防业务接入纵切：正式生成器 + 业务模块（R9 批 0 试点）+ 只手写模块没覆盖的部分。
# 模块：room（席位、代际与连接、在线；塔防房并入 room 的 Room 实体，Q3）、wallet（玩家金币）、enter（建档；Q6 只装 enter）。
# 手写（scripts/testdata/tower-defense）：配置校验、5×5 地图与阻路 BFS、共享金币建造 / 升级 / 出售、准备与托管、
# 波次战斗与 Room 上的波次定时器、基地胜负、团队奖励（Claimed 在 Room，金币经 wallet）、名单准入（RoomJoining 监听）。
# 这些不是框架默认玩法，生成器也不生成这些文件。
# 缺省不连接外部存储：提交器是替身。ROOST_TOWER_DEFENSE_REAL=1（并提供隔离 Mongo 副本集
# ROOST_DATAENGINE_IT_MONGO_URI）时追加：子进程在第一波打完第一步后（已回复、WAL 未投影）被父进程 SIGKILL，
# 新进程冷加载、由驱动投递循环补触发一步并打完三波、领奖，再由第三个进程冷加载核对并重试。使用本次唯一库，成功后删除。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="${ROOST_GENRE_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/roost-tower-defense.XXXXXX")}"
mkdir -p "$review_root"
project_root="$review_root/project"
echo "Tower defense generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_TOWER_DEFENSE_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ -z "${ROOST_GENRE_OUTPUT:-}" && "$succeeded" == 1 && "${ROOST_TOWER_DEFENSE_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "Tower defense generated consumer kept: $review_root" >&2
    # 真实依赖模式失败时 DAO 库与 WAL 留作证据，查完手动删除。
    if [[ "${ROOST_TOWER_DEFENSE_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then
      echo "Tower defense real-persistence database left for inspection: ${ROOST_TOWER_DEFENSE_DB:-unset}" >&2
    fi
  fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new tower-defense-practice -module example.com/tower-defense-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
# 先指向本检出：后面 generate 里的 go mod tidy 不去解析可能尚未发布的最低 core 版本，生成物也按本检出编译。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_TOWER_DEFENSE_DB="roost_tower_defense_it_${PPID}_$$"
# 模块清单写在脚本里（Q8：不建配方）；依赖由安装器连带装上，Player / Room 的主 DAO、玩家 TCP 接入由 plan 代办。
genre_modules=(room wallet enter)
modules_sh="$repo_root/scripts/testdata/genre-common/modules.sh"
bash "$modules_sh" add "$review_root/roost" "${genre_modules[@]}" >> "$review_root/generate.log" 2>&1
# 波次期限是 Room 上的实体定时器（room 模块没有定时器；以后装 turn 时它复用这个组件）。
"$review_root/roost" add timer --entity Room >> "$review_root/generate.log" 2>&1
# 塔防状态是 Room 主 DAO 自己的字段（追加在模块字段之后）；DAO 定义是业务文件，生成组件、访问器与 Sender 由 CLI 生成。
# Room 是 World 锁档（room 模块的实体注解），领奖的 [Player, Room] 多实体锁序固定，不再手改类别。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys
root = Path(sys.argv[1])
p = root / 'db/def/room.go'
marker = 'type RoomDao struct {'
text = p.read_text()
assert marker in text and 'db=main' in text, p  # 骨架写逻辑库 main；本次运行的唯一物理库由测试夹具的映射给出
fields = (
    '\n\t// TowerState 是塔防的整局状态（地图、波次、共享金币、每席位的准备 / 领奖 / 命令回执），严格 JSON。\n'
    '\tTowerState string `store:"tower_state" dao:"nosync"`\n'
    '\t// TowerWakeTimer 是当前波次期限在 Room 定时器里的节点号；0 表示没有期限（终局）。\n'
    '\tTowerWakeTimer int64 `store:"tower_wake_timer" dao:"nosync"`\n'
)
end = text.index('\n}', text.index(marker))
p.write_text(text[:end] + '\n' + fields + text[end:])
DEFS
fixture="$repo_root/scripts/testdata/tower-defense"
mkdir -p game/modules/towerdefense
for name in rules state match; do cp "$fixture/$name.go.txt" "game/modules/towerdefense/$name.go"; done
cp "$fixture/rules_test.go.txt" game/modules/towerdefense/rules_test.go
cp "$fixture/timer_handlers.go.txt" game/entities/room/timer_handlers.go
cp "$fixture/room_roster.go.txt" game/entities/room/tower.go
cp "$fixture/handlers.go.txt" game/handler/tower_defense.go
cp "$fixture/gameplay_test.go.txt" game/handler/tower_defense_test.go
gofmt -w db/def/room.go game/modules/towerdefense/*.go game/entities/room/*.go game/handler/tower_defense*.go
python3 "$repo_root/scripts/testdata/robot-common/install.py" "$repo_root" "$project_root" tower-defense
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestTowerDefense' ./game/modules/towerdefense ./game/handler | tee "$review_root/test.log"
# 11 条场景（含业务 Robot）必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestTowerDefense' "$review_root/test.log" || true)"
if [[ "$passed" != 11 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 11 passing TestTowerDefense scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
# 已装模块自带的场景照跑：每个模块通过数 == 本检出 modules/<名>/module.yaml 的 verify.pass、0 skip（结果 module-verify.json）。
bash "$modules_sh" verify "$review_root/roost" "$repo_root" "$review_root/module-verify.json" "${genre_modules[@]}" | tee "$review_root/module-verify.txt"
# 真实依赖用例缺省模式也复制进工程并 vet（只编译不运行）：生成器签名变了，发版门禁里就能看到这些夹具编译失败。
for path in "$repo_root"/scripts/testdata/tower-defense/persistence/*_test.go; do cp "$path" "game/handler/tower_defense_$(basename "$path")"; done
gofmt -w game/handler/tower_defense_*_test.go
go vet -mod=mod -p 2 ./game/handler
if [[ "${ROOST_TOWER_DEFENSE_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  export ROOST_TOWER_DEFENSE_WAL="$review_root/persistence"
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestTowerDefenseRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
  if [[ "$(grep -c '^--- PASS: TestTowerDefenseRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
