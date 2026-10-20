#!/usr/bin/env bash
# 自走棋业务接入纵切：正式生成器、Player / ChessMatch DAO、ChessMatch 实体定时器、多实体 Sender 与 Nest。
# 业务规则（八席共享卡池与商店、经济与升级、合星、编队、准备 / 战斗阶段、确定性战斗与配对、淘汰与排名、
# 断线接管、按排名领奖）只在 scripts/testdata/auto-chess，不是框架默认玩法，生成器也不生成这些文件。
# 缺省不连接外部存储：提交器是替身。ROOST_AUTO_CHESS_REAL=1（并提供隔离 Mongo 副本集
# ROOST_DATAENGINE_IT_MONGO_URI）时追加：子进程 strict 提交后、WAL 记录未投影时被父进程 SIGKILL，新进程
# 冷加载、由驱动投递循环续完整局、八人领奖，再由第三个进程冷加载核对并重试。使用本次唯一库，成功后删除。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-auto-chess.XXXXXX")"
project_root="$review_root/project"
echo "Auto chess generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_AUTO_CHESS_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ "$succeeded" == 1 && "${ROOST_AUTO_CHESS_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "Auto chess generated consumer kept: $review_root" >&2
    # 真实依赖模式失败时 DAO 库与 WAL 留作证据，查完手动删除。
    if [[ "${ROOST_AUTO_CHESS_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then
      echo "Auto chess real-persistence database left for inspection: ${ROOST_AUTO_CHESS_DB:-unset}" >&2
    fi
  fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new auto-chess-practice -module example.com/auto-chess-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
# 先指向本检出：后面 generate 里的 go mod tidy 不去解析可能尚未发布的最低 core 版本，生成物也按本检出编译。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_AUTO_CHESS_DB="roost_auto_chess_it_${PPID}_$$"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity ChessMatch >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao ChessMatch --entity ChessMatch >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity ChessMatch >> "$review_root/generate.log" 2>&1
# DAO 定义与实体注解是业务文件；只修改它们，生成组件、访问器与 Sender 由 CLI 生成。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys, os
root = Path(sys.argv[1])
for name, fields in {
    'player': [('Gold', 'int64', 'gold'), ('Initialized', 'bool', 'initialized')],
    'chess_match': [('State', 'string', 'state'), ('WakeTimer', 'int64', 'wake_timer')],
}.items():
    p = root / 'db/def' / f'{name}.go'
    typename = ''.join(part.title() for part in name.split('_')) + 'Dao'
    marker = 'type ' + typename + ' struct {'
    text = p.read_text().replace(marker, marker + '\n' + ''.join(f'{field} {typ} `bson:"{key}" dao:"persist"`\n' for field, typ, key in fields))
    text = text.replace('db=auto_chess_practice', 'db=' + os.environ['ROOST_AUTO_CHESS_DB'])
    p.write_text(text)
# ChessMatch 与 Player 在同一事务里领奖；把 Match 放在 World 锁档（低于 Player），多实体锁序固定。
p = root / 'game/entities/chess_match/entity.go'
p.write_text(p.read_text().replace('category=entity.EntityCategoryOther', 'category=entity.EntityCategoryWorld'))
DEFS
mkdir -p game/autochess
for name in rules state match; do cp "$repo_root/scripts/testdata/auto-chess/$name.go.txt" "game/autochess/$name.go"; done
cp "$repo_root/scripts/testdata/auto-chess/rules_test.go.txt" game/autochess/rules_test.go
cp "$repo_root/scripts/testdata/auto-chess/timer_handlers.go.txt" game/entities/chess_match/timer_handlers.go
cp "$repo_root/scripts/testdata/auto-chess/handlers.go.txt" game/handler/auto_chess.go
cp "$repo_root/scripts/testdata/auto-chess/gameplay_test.go.txt" game/handler/auto_chess_test.go
gofmt -w db/def/*.go game/autochess/*.go game/entities/chess_match/*.go game/handler/auto_chess*.go
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestAutoChess' ./game/autochess ./game/handler | tee "$review_root/test.log"
# 12 条场景必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestAutoChess' "$review_root/test.log" || true)"
if [[ "$passed" != 12 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 12 passing TestAutoChess scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
if [[ "${ROOST_AUTO_CHESS_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  for path in "$repo_root"/scripts/testdata/auto-chess/persistence/*_test.go; do cp "$path" "game/handler/auto_chess_$(basename "$path")"; done
  gofmt -w game/handler/auto_chess_*_test.go
  export ROOST_AUTO_CHESS_WAL="$review_root/persistence"
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestAutoChessRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
  if [[ "$(grep -c '^--- PASS: TestAutoChessRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
