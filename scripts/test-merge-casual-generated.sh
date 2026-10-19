#!/usr/bin/env bash
# 合成 / 超休闲正式生成工程：Player DAO、棋盘 / 资产、离线命令、会话代际、持久挑战计时。
# 缺省提交器为替身；ROOST_MERGE_CASUAL_REAL=1 增加独立 Mongo / 文件 WAL 的 SIGKILL 恢复棋盘与广告 / 挑战奖励重放。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-merge-casual.XXXXXX")"
succeeded=0
cleanup() {
 if [[ "$succeeded" == 1 && "${ROOST_MERGE_CASUAL_KEEP:-0}" != 1 ]]; then rm -rf "$review_root"; else
  echo "Merge casual consumer kept: $review_root" >&2
  if [[ "${ROOST_MERGE_CASUAL_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then echo "Merge casual database namespace for diagnosis: ${ROOST_MERGE_CASUAL_DB:-unset}" >&2; fi
 fi
}
trap cleanup EXIT
export GOWORK=off GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
echo "Merge casual generated consumer and logs: $review_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new merge-casual-practice -module example.com/merge-casual-practice -out "$review_root/project" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$review_root/project"
# 后续 add / generate 也始终使用本检出；消费者不修改 core 自己的 go.mod / go.sum。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_MERGE_CASUAL_DB="roost_merge_casual_it_${PPID}_$$"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity Player >> "$review_root/generate.log" 2>&1
python3 - <<'DEFS'
from pathlib import Path
import os
p=Path('db/def/player.go');text=p.read_text().replace('type PlayerDao struct {','type PlayerDao struct {\nState string `bson:"state" dao:"persist"`\nWakeTimer int64 `bson:"wake_timer" dao:"persist"`\n').replace('db=merge_casual_practice','db='+os.environ['ROOST_MERGE_CASUAL_DB']);p.write_text(text)
DEFS
mkdir -p game/mergecasual
for name in rules state operations reward; do cp "$repo_root/scripts/testdata/merge-casual/$name.go.txt" "game/mergecasual/$name.go"; done
cp "$repo_root/scripts/testdata/merge-casual/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$repo_root/scripts/testdata/merge-casual/handlers.go.txt" game/handler/merge_casual.go
cp "$repo_root/scripts/testdata/merge-casual/gameplay_test.go.txt" game/handler/merge_casual_test.go
cp "$repo_root/scripts/testdata/merge-casual/rules_test.go.txt" game/mergecasual/rules_test.go
gofmt -w db/def/player.go game/mergecasual/*.go game/entities/player/timer_handlers.go game/handler/merge_casual*.go
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMergeCasual' ./game/mergecasual ./game/handler | tee "$review_root/test.log"
passed="$(grep -c '^--- PASS: TestMergeCasual' "$review_root/test.log" || true)"
if [[ "$passed" != 15 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then echo "expected 15 passing Merge casual scenarios, got $passed" >&2; exit 1; fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
if [[ "${ROOST_MERGE_CASUAL_REAL:-0}" == 1 ]]; then
 : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
 for path in "$repo_root"/scripts/testdata/merge-casual/persistence/*_test.go; do cp "$path" "game/handler/merge_casual_$(basename "$path")"; done
 export ROOST_MERGE_CASUAL_WAL="$review_root/persistence"
 go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMergeCasualRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
 if [[ "$(grep -c '^--- PASS: TestMergeCasualRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
 go vet -mod=mod -p 2 ./...
fi
succeeded=1
