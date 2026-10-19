#!/usr/bin/env bash
# 卡牌 RPG 正式生成工程：配置快照、属性生成、Player / CardRun DAO、多实体 Sender、持久回合定时器。
# 缺省提交器为替身；ROOST_CARD_RPG_REAL=1 增加独立 Mongo / 文件 WAL 的 SIGKILL 恢复续战与领奖重试。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-card-rpg.XXXXXX")"
succeeded=0
cleanup() {
 if [[ "$succeeded" == 1 && "${ROOST_CARD_RPG_KEEP:-0}" != 1 ]]; then rm -rf "$review_root"; else
  echo "Card RPG consumer kept: $review_root" >&2
  if [[ "${ROOST_CARD_RPG_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then echo "Card RPG database namespace for diagnosis: ${ROOST_CARD_RPG_DB:-unset}" >&2; fi
 fi
}
trap cleanup EXIT
export GOWORK=off GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
echo "Card RPG generated consumer and logs: $review_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new card-rpg-practice -module example.com/card-rpg-practice -out "$review_root/project" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$review_root/project"
# 后续 add / generate 也始终使用本检出；消费者不修改 core 自己的 go.mod / go.sum。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_CARD_RPG_DB="roost_card_rpg_it_${PPID}_$$"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity CardRun >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao CardRun --entity CardRun >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity CardRun >> "$review_root/generate.log" 2>&1
"$review_root/roost" add attribute HeroStats >> "$review_root/generate.log" 2>&1
python3 - <<'DEFS'
from pathlib import Path
import os
for name,typename in [('player','PlayerDao'),('card_run','CardRunDao')]:
 p=Path('db/def')/(name+'.go');text=p.read_text();fields='\nState string `bson:"state" dao:"persist"`\n'
 if name=='card_run':fields+='TurnTimer int64 `bson:"turn_timer" dao:"persist"`\n'
 text=text.replace('type '+typename+' struct {','type '+typename+' struct {'+fields).replace('db=card_rpg_practice','db='+os.environ['ROOST_CARD_RPG_DB']);p.write_text(text)
p=Path('game/entities/card_run/entity.go');p.write_text(p.read_text().replace('category=entity.EntityCategoryOther','category=entity.EntityCategoryWorld'))
DEFS
mkdir -p game/cardrpg
for name in catalog profile battle; do cp "$repo_root/scripts/testdata/card-rpg/$name.go.txt" "game/cardrpg/$name.go"; done
cp "$repo_root/scripts/testdata/card-rpg/attributes.go.txt" game/gameplay/attribute/hero_stats.go
cp "$repo_root/scripts/testdata/card-rpg/timer_handlers.go.txt" game/entities/card_run/timer_handlers.go
cp "$repo_root/scripts/testdata/card-rpg/handlers.go.txt" game/handler/card_rpg.go
cp "$repo_root/scripts/testdata/card-rpg/gameplay_test.go.txt" game/handler/card_rpg_test.go
cp "$repo_root/scripts/testdata/card-rpg/catalog_test.go.txt" game/cardrpg/catalog_test.go
gofmt -w db/def/*.go game/cardrpg/*.go game/gameplay/attribute/hero_stats.go game/entities/card_run/timer_handlers.go game/handler/card_rpg*.go
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestCardRPG' ./game/cardrpg ./game/handler | tee "$review_root/test.log"
passed="$(grep -c '^--- PASS: TestCardRPG' "$review_root/test.log" || true)"
if [[ "$passed" != 16 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then echo "expected 16 passing Card RPG scenarios, got $passed" >&2; exit 1; fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
if [[ "${ROOST_CARD_RPG_REAL:-0}" == 1 ]]; then
 : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
 for path in "$repo_root"/scripts/testdata/card-rpg/persistence/*_test.go; do cp "$path" "game/handler/card_rpg_$(basename "$path")"; done
 export ROOST_CARD_RPG_WAL="$review_root/persistence"
 go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestCardRPGRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
 if [[ "$(grep -c '^--- PASS: TestCardRPGRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
 go vet -mod=mod -p 2 ./...
fi
succeeded=1
