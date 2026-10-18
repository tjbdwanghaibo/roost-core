#!/usr/bin/env bash
# MMORPG 业务接入纵切：正式生成器、Player / Scene DAO、多实体 Sender 与 Nest。
# 不连接外部存储：提交器是替身，验证内存事务组合；真实存储走标准 integration 脚本。
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
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity Scene >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao Scene --entity Scene >> "$review_root/generate.log" 2>&1
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys
root=Path(sys.argv[1])
for name, fields in {
 'player': ['Gold', 'PartyID', 'SceneID', 'SceneVersion', 'TradeSeq', 'TradeBuyer', 'TradeAmount'],
 'scene': ['Occupants'],
}.items():
 p=root/'db/def'/f'{name}.go'
 text=p.read_text(); marker='type '+name.title()+'Dao struct {'
 text=text.replace(marker, marker+'\n'+''.join(f'{field} int64 `bson:"{field.lower()}" dao:"persist"`\n' for field in fields))
 p.write_text(text)
p=root/'game/entities/scene/entity.go'
p.write_text(p.read_text().replace('category=entity.EntityCategoryOther','category=entity.EntityCategoryWorld'))
DEFS
cp "$repo_root/scripts/testdata/mmorpg/tasks.go.txt" game/handler/mmo_tasks.go
cp "$repo_root/scripts/testdata/mmorpg/tasks_test.go.txt" game/handler/mmo_tasks_test.go
gofmt -w db/def/*.go game/handler/mmo_tasks*.go
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMMORPG' ./game/handler | tee "$review_root/test.log"
# 4 条场景必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestMMORPG' "$review_root/test.log" || true)"
if [[ "$passed" != 4 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 4 passing TestMMORPG scenarios, got $passed" >&2
  exit 1
fi
"$review_root/roost" id check
"$review_root/roost" generate --check
succeeded=1
