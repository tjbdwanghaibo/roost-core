#!/usr/bin/env bash
# SLG 业务接入纵切：正式生成器、Player / Tile / AllianceAuthority（Remote 托管）DAO、组件、Sender、Nest 与按实体定时器。
# 业务规则（建造 / 研究 / 行军、抢地、联盟成员与贡献意图、活动阶段屏障、原服奖励义务与 Player 到账）只在
# scripts/testdata/slg，不是框架默认玩法，生成器也不生成这些文件。
# 缺省不连接外部存储：提交器是替身，离线恢复用 DAO 序列化；联盟活动的业务文件与 Remote Sender 只参与编译与 vet。
# ROOST_SLG_REAL=1（并提供隔离 Mongo 副本集、Redis、NATS：ROOST_DATAENGINE_IT_MONGO_URI / _REDIS_ADDR / _NATS_URL）
# 时追加联盟活动的三条真实依赖用例：正式 Activity RPC 经真实 NATS 调用独立协调器子进程（重启两次）；两个原服各自的
# Player 在 strict 文件 WAL 上到账、在“Player 已到账、原服未标记完成”处被 SIGKILL 后冷加载补完；联盟权威换成生成的
# Remote 托管实体并在同进程内交接 owner。每条用例用本次唯一库、唯一 Redis 前缀与 NATS subject，成功后删除。
# 真实模式写共享隔离环境，运行前持有 $ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-slg.XXXXXX")"
project_root="$review_root/project"
echo "SLG generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_SLG_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ "$succeeded" == 1 && "${ROOST_SLG_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "SLG generated consumer kept: $review_root" >&2
    # 真实依赖模式失败时 DAO 库、WAL 与子进程日志留作证据，查完手动删除。
    if [[ "${ROOST_SLG_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then
      echo "SLG real-dependency databases left for inspection: ${slg_db:-unset}_player ${slg_db:-unset}_remote" >&2
    fi
  fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new slg-practice -module example.com/slg-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
# 先指向本检出：后面 generate 里的 go mod tidy 不去解析可能尚未发布的最低 core 版本，生成物也按本检出编译。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity Tile >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao Tile --entity Tile >> "$review_root/generate.log" 2>&1
"$review_root/roost" add mod remote_entity -service game >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity AllianceAuthority >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao AllianceAuthority --entity AllianceAuthority >> "$review_root/generate.log" 2>&1
# DAO 定义与实体注解是业务文件；只修改它们，生成组件、访问器与 Sender 由 CLI 生成。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys
root = Path(sys.argv[1])
for name, fields in {
    'player': [('Gold', 'int64', 'gold'), ('BuildingLevel', 'int64', 'building_level'), ('ResearchLevel', 'int64', 'research_level'),
               ('PendingResearch', 'int64', 'pending_research'), ('PendingBuild', 'int64', 'pending_build'), ('Marches', 'int64', 'marches'),
               ('ArrivedX', 'int64', 'arrived_x'), ('HomeSID', 'int32', 'home_sid'), ('Receipts', 'string', 'receipts')],
    'tile': [('OwnerID', 'int64', 'owner_id')],
    'alliance_authority': [('State', 'string', 'state')],
}.items():
    p = root / 'db/def' / f'{name}.go'
    typename = ''.join(part.title() for part in name.split('_')) + 'Dao'
    marker = 'type ' + typename + ' struct {'
    text = p.read_text().replace(marker, marker + '\n' + ''.join(f'{field} {typ} `bson:"{key}" dao:"persist"`\n' for field, typ, key in fields))
    assert 'db=main' in text, p  # 骨架写逻辑库 main；真实模式的唯一物理库由测试夹具的映射给出
    if name == 'alliance_authority':
        # Remote 托管实体由每个进程从同一个库提交 / 加载，DAO 必须是 dbscope=global。
        text = text.replace('db=main', 'db=main dbscope=global')
    p.write_text(text)
# 地块是被许多玩家争用的世界对象，锁序低于 Player；联盟权威是 Remote 托管实体。
p = root / 'game/entities/tile/entity.go'
p.write_text(p.read_text().replace('category=entity.EntityCategoryOther', 'category=entity.EntityCategoryWorld'))
p = root / 'game/entities/alliance_authority/entity.go'
text = p.read_text().replace('category=entity.EntityCategoryOther', 'category=entity.EntityCategoryRemote remote=managed')
p.write_text(text.replace('*entity.EntityBase', '*entity.RemoteEntityBase'))
DEFS
cp "$repo_root/scripts/testdata/slg/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$repo_root/scripts/testdata/slg/tasks.go.txt" game/handler/slg_tasks.go
cp "$repo_root/scripts/testdata/slg/tasks_test.go.txt" game/handler/slg_tasks_test.go
cp "$repo_root/scripts/testdata/slg/alliance_rewards.go.txt" game/handler/alliance_rewards.go
cp "$repo_root/scripts/testdata/slg/alliance_authority.go.txt" game/handler/alliance_authority.go
mkdir -p game/alliance
cp "$repo_root/scripts/testdata/slg/alliance/alliance.go" game/alliance/alliance.go
gofmt -w db/def/*.go game/entities/player/timer_handlers.go game/handler/*.go game/alliance/*.go
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestSLG' ./game/handler | tee "$review_root/test.log"
# 6 条场景必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestSLG' "$review_root/test.log" || true)"
if [[ "$passed" != 6 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 6 passing TestSLG scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
# 联盟测试缺省模式也复制并 vet；补报截止跟随协调器保留期的用例是纯内存的，缺省模式就跑。
cp "$repo_root"/scripts/testdata/slg/alliance/*_test.go game/alliance/
gofmt -w game/alliance/*_test.go
go vet -mod=mod -p 2 ./game/alliance
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestAllianceReportWindowFollowsTheCoordinatorReservationTTL$' ./game/alliance | tee "$review_root/alliance-unit.log"
if [[ "$(grep -c '^--- PASS: TestAllianceReportWindowFollowsTheCoordinatorReservationTTL ' "$review_root/alliance-unit.log" || true)" != 1 ]]; then
  echo "alliance report-window test did not pass" >&2
  exit 1
fi
if [[ "${ROOST_SLG_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  : "${ROOST_DATAENGINE_IT_REDIS_ADDR:?isolated Redis address required}"
  : "${ROOST_DATAENGINE_IT_NATS_URL:?isolated NATS URL required}"
  # 本次运行的物理库名前缀；写 Mongo 的两条用例各用 _player / _remote 一个库和一份 WAL 根，互不复用。
  slg_db="roost_slg_it_${PPID}_$$"
  # 基础活动链路（业务 Redis CAS 联盟、原服奖励义务回执与 ACK 重投），不写 Mongo。
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestSLGAllianceActivity$' ./game/alliance | tee "$review_root/activity.log"
  if [[ "$(grep -c '^--- PASS: TestSLGAllianceActivity ' "$review_root/activity.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/activity.log"; then exit 1; fi
  # 同一条链路继续到两个原服的 Player 到账（每个原服 write → SIGKILL → recover → verify 三个子进程）。
  ROOST_SLG_DB="${slg_db}_player" ROOST_SLG_WAL="$review_root/persistence/player" \
    go test -mod=mod -race -count=1 -timeout 4m -v -run '^TestSLGAlliancePlayerDelivery$' ./game/alliance | tee "$review_root/player.log"
  if [[ "$(grep -c '^--- PASS: TestSLGAlliancePlayerDelivery ' "$review_root/player.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/player.log"; then exit 1; fi
  # 联盟权威换成生成的 Remote 托管实体，冻结后由 sid 2000 交接到 2001，再继续 Player 到账。
  ROOST_SLG_DB="${slg_db}_remote" ROOST_SLG_WAL="$review_root/persistence/remote" \
    go test -mod=mod -race -count=1 -timeout 4m -v -run '^TestSLGAllianceRemoteDelivery$' ./game/alliance | tee "$review_root/remote.log"
  if [[ "$(grep -c '^--- PASS: TestSLGAllianceRemoteDelivery ' "$review_root/remote.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/remote.log"; then exit 1; fi
fi
succeeded=1
