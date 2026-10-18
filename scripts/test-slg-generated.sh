#!/usr/bin/env bash
# SLG 业务接入纵切：正式生成器、Player DAO、组件、Sender、Nest 与按实体定时器。
# 不连接外部存储：提交器是替身，离线恢复用 DAO 序列化；真实存储走标准 integration 脚本。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-slg.XXXXXX")"
project_root="$review_root/project"
echo "SLG generated consumer and logs: $review_root"
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new slg-practice -module example.com/slg-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity Tile >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao Tile --entity Tile >> "$review_root/generate.log" 2>&1
# DAO 定义与回调骨架是业务文件；只修改它们，生成组件与访问器由 CLI 生成。
python3 - "$project_root/db/def/player.go" <<'PY'
from pathlib import Path
import sys
p=Path(sys.argv[1])
s=p.read_text().replace('type PlayerDao struct {', '''type PlayerDao struct {
    Gold int64 `bson:"gold" dao:"persist"`
    BuildingLevel int64 `bson:"building_level" dao:"persist"`
    ResearchLevel int64 `bson:"research_level" dao:"persist"`
    PendingResearch int64 `bson:"pending_research" dao:"persist"`
    PendingBuild int64 `bson:"pending_build" dao:"persist"`
    Marches int64 `bson:"marches" dao:"persist"`
    ArrivedX int64 `bson:"arrived_x" dao:"persist"`''')
p.write_text(s)
root=p.parents[2]
tile=root/'game/entities/tile/entity.go'
tile.write_text(tile.read_text().replace('category=entity.EntityCategoryOther','category=entity.EntityCategoryWorld'))
tile_dao=root/'db/def/tile.go'
tile_dao.write_text(tile_dao.read_text().replace('type TileDao struct {','type TileDao struct {\nOwnerID int64 `bson:"owner_id" dao:"persist"`'))
PY
cp "$repo_root/scripts/testdata/slg/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$repo_root/scripts/testdata/slg/tasks.go.txt" game/handler/slg_tasks.go
cp "$repo_root/scripts/testdata/slg/tasks_test.go.txt" game/handler/slg_tasks_test.go
gofmt -w db/def/player.go game/entities/player/timer_handlers.go game/handler/slg_tasks*.go
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestSLG' ./game/handler
"$review_root/roost" id check
"$review_root/roost" generate --check
