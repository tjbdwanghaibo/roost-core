#!/usr/bin/env bash
# 合成休闲业务接入纵切：正式生成器、Player DAO、Player 实体定时器与 Nest。业务规则（4×4 棋盘与仓库、合成 /
# 生产 / 出售撤销 / 订单、惰性体力恢复、离线命令批次与设备代际、限时挑战、模拟广告回调）只在
# scripts/testdata/merge-casual，不是框架默认玩法，生成器也不生成这些文件。
# 缺省不连接外部存储：提交器是替身。ROOST_MERGE_CASUAL_REAL=1（并提供隔离 Mongo 副本集
# ROOST_DATAENGINE_IT_MONGO_URI）时追加：子进程 strict 提交后、WAL 记录未投影时被父进程 SIGKILL，新进程
# 冷加载、判负过期挑战、续玩领奖，再由第三个进程冷加载核对并重试。使用本次唯一库，成功后删除。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-merge-casual.XXXXXX")"
project_root="$review_root/project"
echo "Merge casual generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_MERGE_CASUAL_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ "$succeeded" == 1 && "${ROOST_MERGE_CASUAL_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "Merge casual generated consumer kept: $review_root" >&2
    # 真实依赖模式失败时 DAO 库与 WAL 留作证据，查完手动删除。
    if [[ "${ROOST_MERGE_CASUAL_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then
      echo "Merge casual real-persistence database left for inspection: ${ROOST_MERGE_CASUAL_DB:-unset}" >&2
    fi
  fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new merge-casual-practice -module example.com/merge-casual-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
# 先指向本检出：后面 generate 里的 go mod tidy 不去解析可能尚未发布的最低 core 版本，生成物也按本检出编译。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_MERGE_CASUAL_DB="roost_merge_casual_it_${PPID}_$$"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity Player >> "$review_root/generate.log" 2>&1
# DAO 定义是业务文件；只修改它，生成组件、访问器与 Sender 由 CLI 生成。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys, os
root = Path(sys.argv[1])
p = root / 'db/def/player.go'
fields = [('State', 'string', 'state'), ('WakeTimer', 'int64', 'wake_timer')]
marker = 'type PlayerDao struct {'
text = p.read_text().replace(marker, marker + '\n' + ''.join(f'{field} {typ} `bson:"{key}" dao:"persist"`\n' for field, typ, key in fields))
text = text.replace('db=merge_casual_practice', 'db=' + os.environ['ROOST_MERGE_CASUAL_DB'])
p.write_text(text)
DEFS
mkdir -p game/mergecasual
for name in rules state operations reward; do cp "$repo_root/scripts/testdata/merge-casual/$name.go.txt" "game/mergecasual/$name.go"; done
cp "$repo_root/scripts/testdata/merge-casual/rules_test.go.txt" game/mergecasual/rules_test.go
cp "$repo_root/scripts/testdata/merge-casual/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$repo_root/scripts/testdata/merge-casual/handlers.go.txt" game/handler/merge_casual.go
cp "$repo_root/scripts/testdata/merge-casual/gameplay_test.go.txt" game/handler/merge_casual_test.go
gofmt -w db/def/player.go game/mergecasual/*.go game/entities/player/timer_handlers.go game/handler/merge_casual*.go
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMergeCasual' ./game/mergecasual ./game/handler | tee "$review_root/test.log"
# 15 条场景必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestMergeCasual' "$review_root/test.log" || true)"
if [[ "$passed" != 15 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 15 passing TestMergeCasual scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
if [[ "${ROOST_MERGE_CASUAL_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  for path in "$repo_root"/scripts/testdata/merge-casual/persistence/*_test.go; do cp "$path" "game/handler/merge_casual_$(basename "$path")"; done
  gofmt -w game/handler/merge_casual_*_test.go
  export ROOST_MERGE_CASUAL_WAL="$review_root/persistence"
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMergeCasualRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
  if [[ "$(grep -c '^--- PASS: TestMergeCasualRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
