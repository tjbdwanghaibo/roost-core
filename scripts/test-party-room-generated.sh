#!/usr/bin/env bash
# 实时派对房间业务接入纵切：正式生成器、Player / PartyRoom DAO、PartyRoom 实体心跳（100ms 档）与持久定时器、
# 多实体 Sender 与 Nest，加 entitysync.Manager + policy.Group 房间内同步、service/match 成局与 service/session 进房。
# 业务规则（“抢星”小游戏、回合 / 加时 / 硬期限、房间码、人满、房主转移、断线宽限、输入去重、结算发奖与座位归还）
# 只在 scripts/testdata/party-room，不是框架默认玩法，生成器也不生成这些文件。
# 缺省不连接外部存储：提交器是替身，心跳驱动跑真实循环、业务时间用 clock.SetSource 拨。
# ROOST_PARTY_ROOM_REAL=1（并提供隔离 Mongo 副本集 ROOST_DATAENGINE_IT_MONGO_URI）时追加：子进程在局内（strict 输入
# 已确认、WAL 未投影）被父进程 SIGKILL，新进程冷加载、心跳按持久阶段重新启动并把局推进到结算、发奖、删档，
# 再由第三个进程核对奖励与删档并重试领奖。使用本次唯一库，成功后删除。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-party-room.XXXXXX")"
project_root="$review_root/project"
echo "Party room generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_PARTY_ROOM_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ "$succeeded" == 1 && "${ROOST_PARTY_ROOM_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "Party room generated consumer kept: $review_root" >&2
    # 真实依赖模式失败时 DAO 库与 WAL 留作证据，查完手动删除。
    if [[ "${ROOST_PARTY_ROOM_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then
      echo "Party room real-persistence database left for inspection: ${ROOST_PARTY_ROOM_DB:-unset}" >&2
    fi
  fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new party-room-practice -module example.com/party-room-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
# 先指向本检出：后面 generate 里的 go mod tidy 不去解析可能尚未发布的最低 core 版本，生成物也按本检出编译。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_PARTY_ROOM_DB="roost_party_room_it_${PPID}_$$"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity PartyRoom >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao PartyRoom --entity PartyRoom >> "$review_root/generate.log" 2>&1
"$review_root/roost" add heartbeat --entity PartyRoom >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity PartyRoom >> "$review_root/generate.log" 2>&1
# DAO 定义与实体注解是业务文件；只修改它们，生成组件、访问器与 Sender 由 CLI 生成。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys
root = Path(sys.argv[1])
for name, fields in {
    'player': [('Coins', 'int64', 'coins', 'persist'), ('Matches', 'int64', 'matches', 'persist'), ('Initialized', 'bool', 'initialized', 'persist')],
    # State 是房间的持久事实（含种子与回执，不同步）；Board 是公开记分板，不落库、经 Group 同步给全体成员。
    'party_room': [('State', 'string', 'state', 'persist'), ('WakeTimer', 'int64', 'wake_timer', 'persist'), ('Board', 'string', 'board', 'nopersist,sync')],
}.items():
    p = root / 'db/def' / f'{name}.go'
    typename = ''.join(part.title() for part in name.split('_')) + 'Dao'
    marker = 'type ' + typename + ' struct {'
    text = p.read_text().replace(marker, marker + '\n' + ''.join(f'{field} {typ} `bson:"{key}" dao:"{tag}"`\n' for field, typ, key, tag in fields))
    assert 'db=main' in text, p  # 骨架写逻辑库 main；本次运行的唯一物理库由测试夹具的映射给出
    p.write_text(text)
# PartyRoom 与 Player 在同一事务里领奖；放在 World 锁档（低于 Player），多实体锁序固定。
# 房间是 Entity Sync 的 subject：sync=true，用业务自己的 packer（只同步 Board 视图）。
p = root / 'game/entities/party_room/entity.go'
text = p.read_text()
assert 'category=entity.EntityCategoryOther' in text, p
p.write_text(text.replace('category=entity.EntityCategoryOther',
    'category=entity.EntityCategoryWorld sync=true syncNamespace=party_room subjectPacker=NewPartyRoomBoardPacker'))
DEFS
mkdir -p game/partyroom
for name in rules state; do cp "$repo_root/scripts/testdata/party-room/$name.go.txt" "game/partyroom/$name.go"; done
cp "$repo_root/scripts/testdata/party-room/rules_test.go.txt" game/partyroom/rules_test.go
# 心跳登记与定时器回调是 roost add 只在缺失时创建的业务文件，用夹具覆盖。
for name in room heartbeat_beats timer_handlers board_packer; do cp "$repo_root/scripts/testdata/party-room/$name.go.txt" "game/entities/party_room/$name.go"; done
cp "$repo_root/scripts/testdata/party-room/handlers.go.txt" game/handler/party_room.go
cp "$repo_root/scripts/testdata/party-room/sync.go.txt" game/handler/party_room_sync.go
cp "$repo_root/scripts/testdata/party-room/gameplay_test.go.txt" game/handler/party_room_test.go
gofmt -w db/def/*.go game/partyroom/*.go game/entities/party_room/*.go game/handler/party_room*.go
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestPartyGame' ./game/partyroom ./game/handler | tee "$review_root/test.log"
# 12 条场景（2 条规则 + 10 条 Nest / 心跳 / 同步）必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestPartyGame' "$review_root/test.log" || true)"
if [[ "$passed" != 12 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 12 passing TestPartyGame scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
# 真实依赖用例缺省模式也复制进工程并 vet（只编译不运行）：生成器签名变了，发版门禁里就能看到这些夹具编译失败。
for path in "$repo_root"/scripts/testdata/party-room/persistence/*_test.go; do cp "$path" "game/handler/party_room_$(basename "$path")"; done
gofmt -w game/handler/party_room_*_test.go
go vet -mod=mod -p 2 ./game/handler
if [[ "${ROOST_PARTY_ROOM_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  export ROOST_PARTY_ROOM_WAL="$review_root/persistence"
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestPartyGameRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
  if [[ "$(grep -c '^--- PASS: TestPartyGameRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
