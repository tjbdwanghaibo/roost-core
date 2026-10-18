#!/usr/bin/env bash
# MMORPG 业务接入纵切：正式生成器、Player / Scene / Trade / Monster DAO、Player 持久定时器、多实体 Sender 与 Nest。
# 缺省不连接外部存储：提交器是替身，验证内存事务组合。ROOST_MMORPG_REAL=1（并提供隔离 Mongo 副本集
# ROOST_DATAENGINE_IT_MONGO_URI）时追加两个独立进程的真实文件 WAL / Mongo 停机后冷加载恢复，用后删除本次唯一库。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-mmorpg.XXXXXX")"
project_root="$review_root/project"
echo "MMORPG generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_MMORPG_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ "$succeeded" == 1 && "${ROOST_MMORPG_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "MMORPG generated consumer kept: $review_root" >&2
    # 真实依赖模式失败时 DAO 库留作证据，查完手动删除。
    if [[ "${ROOST_MMORPG_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then
      echo "MMORPG real-persistence database left for inspection: ${ROOST_MMORPG_DB:-unset}" >&2
    fi
  fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new mmorpg-practice -module example.com/mmorpg-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
export ROOST_MMORPG_DB="roost_mmo_it_${PPID}_$$"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity Scene >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao Scene --entity Scene >> "$review_root/generate.log" 2>&1
for name in Trade Monster; do
  "$review_root/roost" add entity "$name" >> "$review_root/generate.log" 2>&1
  "$review_root/roost" add dao "$name" --entity "$name" >> "$review_root/generate.log" 2>&1
done
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys, os, re
root=Path(sys.argv[1])
for name, fields in {
 'player': ['Gold', 'PartyID', 'SceneID', 'SceneVersion', 'TradeSeq', 'TradeBuyer', 'TradeAmount', 'HP', 'Items', 'AttackSeq', 'LastAttackTarget', 'CooldownUntil', 'BuffValue', 'BuffTimer', 'ReviveTimer', 'QuestKills', 'QuestClaimed'],
 'scene': ['Occupants', 'InstanceComplete'],
 'trade': ['Status', 'SellerID', 'BuyerID', 'Price', 'Count', 'Deadline', 'SellerConfirmed', 'BuyerConfirmed'],
 'monster': ['HP', 'SceneID', 'LootOwner', 'LootTaken'],
}.items():
 p=root/'db/def'/f'{name}.go'
 text=p.read_text(); marker='type '+name.title()+'Dao struct {'
 text=text.replace(marker, marker+'\n'+''.join(f'{field} int64 `bson:"{field.lower()}" dao:"persist"`\n' for field in fields))
 text=re.sub(r"db=mmorpg_practice", "db="+os.environ["ROOST_MMORPG_DB"],text)
 p.write_text(text)
for name in ('scene', 'trade', 'monster'):
 p=root/f'game/entities/{name}/entity.go'
 p.write_text(p.read_text().replace('category=entity.EntityCategoryOther','category=entity.EntityCategoryWorld'))
DEFS
cp "$repo_root/scripts/testdata/mmorpg/tasks.go.txt" game/handler/mmo_tasks.go
cp "$repo_root/scripts/testdata/mmorpg/tasks_test.go.txt" game/handler/mmo_tasks_test.go
cp "$repo_root/scripts/testdata/mmorpg/gameplay.go.txt" game/handler/mmo_gameplay.go
cp "$repo_root/scripts/testdata/mmorpg/gameplay_test.go.txt" game/handler/mmo_gameplay_test.go
cp "$repo_root/scripts/testdata/mmorpg/timer_handlers.go.txt" game/entities/player/timer_handlers.go
gofmt -w db/def/*.go game/handler/mmo_*.go game/entities/player/timer_handlers.go
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMMORPG(Party|Trade|Combat|Buff|Transfer|Death)' ./game/handler | tee "$review_root/test.log"
# 13 条场景必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestMMORPG' "$review_root/test.log" || true)"
if [[ "$passed" != 13 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 13 passing TestMMORPG scenarios, got $passed" >&2
  exit 1
fi
"$review_root/roost" id check
"$review_root/roost" generate --check
if [[ "${ROOST_MMORPG_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  cp "$repo_root/scripts/testdata/mmorpg/persistence/persistence_test.go" game/handler/mmo_persistence_test.go
  gofmt -w game/handler/mmo_persistence_test.go
  export ROOST_MMORPG_WAL="$review_root/persistence"
  for phase in write recover; do
    ROOST_MMORPG_PHASE="$phase" go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMMORPGRealPersistenceAcrossProcesses$' ./game/handler | tee "$review_root/persistence-$phase.log"
    if [[ "$(grep -c '^--- PASS: TestMMORPGRealPersistenceAcrossProcesses' "$review_root/persistence-$phase.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/persistence-$phase.log"; then exit 1; fi
  done
fi
succeeded=1
