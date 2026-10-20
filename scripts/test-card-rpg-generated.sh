#!/usr/bin/env bash
# 卡牌 RPG 业务接入纵切：正式生成器、Player / CardRun DAO、CardRun 实体定时器、HeroStats 属性、多实体 Sender
# 与 Nest。业务规则（配置校验、抽卡 / 保底、养成 / 编队、体力、三人编队回合制 PvE、首通 / 扫荡 / 任务）只在
# scripts/testdata/card-rpg，不是框架默认玩法，生成器也不生成这些文件。
# 缺省不连接外部存储：提交器是替身。ROOST_CARD_RPG_REAL=1（并提供隔离 Mongo 副本集
# ROOST_DATAENGINE_IT_MONGO_URI）时追加：子进程 strict 提交后、WAL 记录未投影时被父进程 SIGKILL，新进程
# 冷加载、由驱动投递一次过期回合、续战领奖，再由第三个进程冷加载核对并重试。使用本次唯一库，成功后删除。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-card-rpg.XXXXXX")"
project_root="$review_root/project"
echo "Card RPG generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_CARD_RPG_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ "$succeeded" == 1 && "${ROOST_CARD_RPG_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "Card RPG generated consumer kept: $review_root" >&2
    # 真实依赖模式失败时 DAO 库与 WAL 留作证据，查完手动删除。
    if [[ "${ROOST_CARD_RPG_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then
      echo "Card RPG real-persistence database left for inspection: ${ROOST_CARD_RPG_DB:-unset}" >&2
    fi
  fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new card-rpg-practice -module example.com/card-rpg-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
# 先指向本检出：后面 generate 里的 go mod tidy 不去解析可能尚未发布的最低 core 版本，生成物也按本检出编译。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_CARD_RPG_DB="roost_card_rpg_it_${PPID}_$$"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity CardRun >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao CardRun --entity CardRun >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity CardRun >> "$review_root/generate.log" 2>&1
"$review_root/roost" add attribute HeroStats >> "$review_root/generate.log" 2>&1
# DAO 定义与实体注解是业务文件；只修改它们，生成组件、访问器与 Sender 由 CLI 生成。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys, os
root = Path(sys.argv[1])
for name, fields in {
    'player': [('State', 'string', 'state')],
    'card_run': [('State', 'string', 'state'), ('TurnTimer', 'int64', 'turn_timer')],
}.items():
    p = root / 'db/def' / f'{name}.go'
    typename = ''.join(part.title() for part in name.split('_')) + 'Dao'
    marker = 'type ' + typename + ' struct {'
    text = p.read_text().replace(marker, marker + '\n' + ''.join(f'{field} {typ} `bson:"{key}" dao:"persist"`\n' for field, typ, key in fields))
    assert 'db=main' in text, p  # 骨架写逻辑库 main；本次运行的唯一物理库由测试夹具的映射给出
    p.write_text(text)
# CardRun 与 Player 在同一事务里开局 / 领奖；把 Run 放在 World 锁档（低于 Player），多实体锁序固定。
p = root / 'game/entities/card_run/entity.go'
p.write_text(p.read_text().replace('category=entity.EntityCategoryOther', 'category=entity.EntityCategoryWorld'))
DEFS
mkdir -p game/cardrpg
for name in catalog profile battle; do cp "$repo_root/scripts/testdata/card-rpg/$name.go.txt" "game/cardrpg/$name.go"; done
cp "$repo_root/scripts/testdata/card-rpg/catalog_test.go.txt" game/cardrpg/catalog_test.go
cp "$repo_root/scripts/testdata/card-rpg/attributes.go.txt" game/gameplay/attribute/hero_stats.go
cp "$repo_root/scripts/testdata/card-rpg/timer_handlers.go.txt" game/entities/card_run/timer_handlers.go
cp "$repo_root/scripts/testdata/card-rpg/handlers.go.txt" game/handler/card_rpg.go
cp "$repo_root/scripts/testdata/card-rpg/gameplay_test.go.txt" game/handler/card_rpg_test.go
gofmt -w db/def/*.go game/cardrpg/*.go game/gameplay/attribute/hero_stats.go game/entities/card_run/timer_handlers.go game/handler/card_rpg*.go
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestCardRPG' ./game/cardrpg ./game/handler | tee "$review_root/test.log"
# 16 条场景必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestCardRPG' "$review_root/test.log" || true)"
if [[ "$passed" != 16 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 16 passing TestCardRPG scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
if [[ "${ROOST_CARD_RPG_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  for path in "$repo_root"/scripts/testdata/card-rpg/persistence/*_test.go; do cp "$path" "game/handler/card_rpg_$(basename "$path")"; done
  gofmt -w game/handler/card_rpg_*_test.go
  export ROOST_CARD_RPG_WAL="$review_root/persistence"
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestCardRPGRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
  if [[ "$(grep -c '^--- PASS: TestCardRPGRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
