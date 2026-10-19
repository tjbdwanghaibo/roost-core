#!/usr/bin/env bash
# 八人自走棋正式消费者；默认替身提交器，REAL=1 追加独立 Mongo / 文件 WAL SIGKILL 恢复。
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
