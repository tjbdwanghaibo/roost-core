#!/usr/bin/env bash
# 合作塔防业务接入纵切：正式生成器、Player / TowerRoom DAO、TowerRoom 实体定时器、多实体 Sender 与 Nest。
# 业务规则（配置校验、5×5 地图与阻路 BFS、共享金币建造 / 升级 / 出售、准备与托管、波次战斗、基地胜负、团队奖励）
# 只在 scripts/testdata/tower-defense，不是框架默认玩法，生成器也不生成这些文件。
# 缺省不连接外部存储：提交器是替身。ROOST_TOWER_DEFENSE_REAL=1（并提供隔离 Mongo 副本集
# ROOST_DATAENGINE_IT_MONGO_URI）时追加：子进程在第一波打完第一步后（strict 已确认、WAL 未投影）被父进程 SIGKILL，
# 新进程冷加载、由驱动投递循环补触发一步并打完三波、领奖，再由第三个进程冷加载核对并重试。使用本次唯一库，成功后删除。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-tower-defense.XXXXXX")"
project_root="$review_root/project"
echo "Tower defense generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_TOWER_DEFENSE_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ "$succeeded" == 1 && "${ROOST_TOWER_DEFENSE_KEEP:-0}" != 1 ]]; then
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
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity TowerRoom >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao TowerRoom --entity TowerRoom >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity TowerRoom >> "$review_root/generate.log" 2>&1
# DAO 定义与实体注解是业务文件；只修改它们，生成组件、访问器与 Sender 由 CLI 生成。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys, os
root = Path(sys.argv[1])
for name, fields in {
    'player': [('Gold', 'int64', 'gold'), ('Initialized', 'bool', 'initialized')],
    'tower_room': [('State', 'string', 'state'), ('WakeTimer', 'int64', 'wake_timer')],
}.items():
    p = root / 'db/def' / f'{name}.go'
    typename = ''.join(part.title() for part in name.split('_')) + 'Dao'
    marker = 'type ' + typename + ' struct {'
    text = p.read_text().replace(marker, marker + '\n' + ''.join(f'{field} {typ} `bson:"{key}" dao:"persist"`\n' for field, typ, key in fields))
    assert 'db=main' in text, p  # 骨架写逻辑库 main；本次运行的唯一物理库由测试夹具的映射给出
    p.write_text(text)
# TowerRoom 与 Player 在同一事务里领奖；把 TowerRoom 放在 World 锁档（低于 Player），多实体锁序固定。
p = root / 'game/entities/tower_room/entity.go'
p.write_text(p.read_text().replace('category=entity.EntityCategoryOther', 'category=entity.EntityCategoryWorld'))
DEFS
mkdir -p game/towerdefense
for name in rules state match; do cp "$repo_root/scripts/testdata/tower-defense/$name.go.txt" "game/towerdefense/$name.go"; done
cp "$repo_root/scripts/testdata/tower-defense/rules_test.go.txt" game/towerdefense/rules_test.go
cp "$repo_root/scripts/testdata/tower-defense/timer_handlers.go.txt" game/entities/tower_room/timer_handlers.go
cp "$repo_root/scripts/testdata/tower-defense/handlers.go.txt" game/handler/tower_defense.go
cp "$repo_root/scripts/testdata/tower-defense/gameplay_test.go.txt" game/handler/tower_defense_test.go
gofmt -w db/def/*.go game/towerdefense/*.go game/entities/tower_room/*.go game/handler/tower_defense*.go
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestTowerDefense' ./game/towerdefense ./game/handler | tee "$review_root/test.log"
# 10 条场景（4 条规则 + 6 条 Nest）必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestTowerDefense' "$review_root/test.log" || true)"
if [[ "$passed" != 10 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 10 passing TestTowerDefense scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
if [[ "${ROOST_TOWER_DEFENSE_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  for path in "$repo_root"/scripts/testdata/tower-defense/persistence/*_test.go; do cp "$path" "game/handler/tower_defense_$(basename "$path")"; done
  gofmt -w game/handler/tower_defense_*_test.go
  export ROOST_TOWER_DEFENSE_WAL="$review_root/persistence"
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestTowerDefenseRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
  if [[ "$(grep -c '^--- PASS: TestTowerDefenseRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
