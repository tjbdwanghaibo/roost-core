#!/usr/bin/env bash
# SLG 业务接入纵切：正式生成器 + 业务模块（R9）+ 只手写模块没覆盖的部分。
# 模块：wallet（玩家金币：建造 / 研究 / 抢地扣费、取消退款、离线收益、原服奖励到账都走它）。
# 手写（scripts/testdata/slg）：Player / Tile / AllianceAuthority（Remote 托管）DAO 字段、建造 / 研究 / 行军队列与 Player 定时器、
# 抢地（[Player, Tile] 多实体事务）、离线收益补发、联盟活动教学消费者（service/activity 协调器、Redis CAS 联盟、
# AllianceAuthority Remote 托管、原服奖励义务账本；Q4 保留）。这些不是框架默认玩法，生成器也不生成这些文件。
# 缺省不连接外部存储：提交器是替身，离线恢复用 DAO 序列化；联盟活动的业务文件与 Remote Sender 参与编译与 vet，
# 另跑两条纯内存用例（补报截止跟随协调器保留期、普通 Bus RPC 接收端预算）。
# ROOST_SLG_REAL=1（并提供隔离 Mongo 副本集、Redis、NATS：ROOST_DATAENGINE_IT_MONGO_URI / _REDIS_ADDR / _NATS_URL）
# 时追加联盟活动的四条真实依赖用例：正式 Activity RPC 经真实 NATS 调用独立协调器子进程（重启两次）；两个原服各自的
# Player 在 strict 文件 WAL 上到账、在“Player 已到账、原服未标记完成”处被 SIGKILL 后冷加载补完；联盟权威换成生成的
# Remote 托管实体并在同进程内交接 owner；普通 Bus RPC 在真实 NATS 上核对调用方 deadline 不传到接收端、接收端自己
# 限时后迟到成功不回复。每条用例用本次唯一库、唯一 Redis 前缀与 NATS subject，成功后删除。
# 真实模式写共享隔离环境，运行前持有 $ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="${ROOST_GENRE_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/roost-slg.XXXXXX")}"
mkdir -p "$review_root"
project_root="$review_root/project"
echo "SLG generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_SLG_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ -z "${ROOST_GENRE_OUTPUT:-}" && "$succeeded" == 1 && "${ROOST_SLG_KEEP:-0}" != 1 ]]; then
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
# 模块清单写在脚本里（Q8：不建配方）；Player 实体与主 DAO 来自 game 模板，wallet 把余额与回执窗口装在 PlayerDao 上。
genre_modules=(wallet)
modules_sh="$repo_root/scripts/testdata/genre-common/modules.sh"
bash "$modules_sh" add "$review_root/roost" "${genre_modules[@]}" >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity Tile >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao Tile --entity Tile >> "$review_root/generate.log" 2>&1
"$review_root/roost" add mod remote_entity -service game >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity AllianceAuthority >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao AllianceAuthority --entity AllianceAuthority >> "$review_root/generate.log" 2>&1
# DAO 定义与实体注解是业务文件；只修改它们，生成组件、访问器与 Sender 由 CLI 生成。
# SLG 的玩家字段追加在 PlayerDao 末尾（wallet 的 Wallet / WalletLedger 之后）；金币与原服奖励的到账回执在 wallet 里，不再手写。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys
root = Path(sys.argv[1])
for name, fields in {
    'player': [('BuildingLevel', 'int64', 'building_level'), ('ResearchLevel', 'int64', 'research_level'),
               ('PendingResearch', 'int64', 'pending_research'), ('PendingBuild', 'int64', 'pending_build'), ('Marches', 'int64', 'marches'),
               ('ArrivedX', 'int64', 'arrived_x'), ('HomeSID', 'int32', 'home_sid')],
    'tile': [('OwnerID', 'int64', 'owner_id')],
    'alliance_authority': [('State', 'string', 'state')],
}.items():
    p = root / 'db/def' / f'{name}.go'
    typename = ''.join(part.title() for part in name.split('_')) + 'Dao'
    marker = 'type ' + typename + ' struct {'
    text = p.read_text()
    end = text.index('\n}', text.index(marker))
    text = text[:end] + '\n' + ''.join(f'\t{field} {typ} `store:"{key}" dao:"nosync"`\n' for field, typ, key in fields) + text[end:]
    assert 'db=main' in text, p  # 骨架写逻辑库 main；真实模式的唯一物理库由测试夹具的映射给出
    if name == 'alliance_authority':
        # Remote 托管实体由每个进程从同一个库提交 / 加载，DAO 必须是 dbscope=global；
        # 骨架缺省写 dbscope=sid（v1.47.0 起），这里替换掉它而不是再追加一个。
        assert 'dbscope=sid' in text, p
        text = text.replace('dbscope=sid', 'dbscope=global')
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
mkdir -p game/modules/alliance
cp "$repo_root/scripts/testdata/slg/alliance/alliance.go" game/modules/alliance/alliance.go
cp "$repo_root/scripts/testdata/slg/alliance/rpc_budget.go" game/modules/alliance/rpc_budget.go
gofmt -w db/def/*.go game/entities/player/timer_handlers.go game/handler/*.go game/modules/alliance/*.go
python3 "$repo_root/scripts/testdata/robot-common/install.py" "$repo_root" "$project_root" slg
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestSLG' ./game/handler | tee "$review_root/test.log"
# 7 条场景（含业务 Robot）必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestSLG' "$review_root/test.log" || true)"
if [[ "$passed" != 7 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 7 passing TestSLG scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
# 已装模块自带的场景照跑：每个模块通过数 == 本检出 modules/<名>/module.yaml 的 verify.pass、0 skip（结果 module-verify.json）。
bash "$modules_sh" verify "$review_root/roost" "$repo_root" "$review_root/module-verify.json" "${genre_modules[@]}" | tee "$review_root/module-verify.txt"
# 联盟测试缺省模式也复制并 vet；补报截止跟随协调器保留期、普通 Bus RPC 接收端预算两条用例是纯内存的，缺省模式就跑。
cp "$repo_root"/scripts/testdata/slg/alliance/*_test.go game/modules/alliance/
gofmt -w game/modules/alliance/*_test.go
go vet -mod=mod -p 2 ./game/modules/alliance
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestAllianceReportWindowFollowsTheCoordinatorReservationTTL$' ./game/modules/alliance | tee "$review_root/alliance-unit.log"
if [[ "$(grep -c '^--- PASS: TestAllianceReportWindowFollowsTheCoordinatorReservationTTL ' "$review_root/alliance-unit.log" || true)" != 1 ]]; then
  echo "alliance report-window test did not pass" >&2
  exit 1
fi
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestAllianceReceiverBudgetRejectsLateSuccess$' ./game/modules/alliance | tee "$review_root/alliance-budget.log"
if [[ "$(grep -c '^--- PASS: TestAllianceReceiverBudgetRejectsLateSuccess ' "$review_root/alliance-budget.log" || true)" != 1 ]]; then
  echo "alliance receiver-budget test did not pass" >&2
  exit 1
fi
if [[ "${ROOST_SLG_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  : "${ROOST_DATAENGINE_IT_REDIS_ADDR:?isolated Redis address required}"
  : "${ROOST_DATAENGINE_IT_NATS_URL:?isolated NATS URL required}"
  # 本次运行的物理库名前缀；写 Mongo 的两条用例各用 _player / _remote 一个库和一份 WAL 根，互不复用。
  slg_db="roost_slg_it_${PPID}_$$"
  # 基础活动链路（业务 Redis CAS 联盟、原服奖励义务回执与 ACK 重投），不写 Mongo。
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestSLGAllianceActivity$' ./game/modules/alliance | tee "$review_root/activity.log"
  if [[ "$(grep -c '^--- PASS: TestSLGAllianceActivity ' "$review_root/activity.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/activity.log"; then exit 1; fi
  # 同一条链路继续到两个原服的 Player 到账（每个原服 write → SIGKILL → recover → verify 三个子进程）。
  ROOST_SLG_DB="${slg_db}_player" ROOST_SLG_WAL="$review_root/persistence/player" \
    go test -mod=mod -race -count=1 -timeout 4m -v -run '^TestSLGAlliancePlayerDelivery$' ./game/modules/alliance | tee "$review_root/player.log"
  if [[ "$(grep -c '^--- PASS: TestSLGAlliancePlayerDelivery ' "$review_root/player.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/player.log"; then exit 1; fi
  # 联盟权威换成生成的 Remote 托管实体，冻结后由 sid 2000 交接到 2001，再继续 Player 到账。
  ROOST_SLG_DB="${slg_db}_remote" ROOST_SLG_WAL="$review_root/persistence/remote" \
    go test -mod=mod -race -count=1 -timeout 4m -v -run '^TestSLGAllianceRemoteDelivery$' ./game/modules/alliance | tee "$review_root/remote.log"
  if [[ "$(grep -c '^--- PASS: TestSLGAllianceRemoteDelivery ' "$review_root/remote.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/remote.log"; then exit 1; fi
  # 普通 Bus RPC 的调用方 deadline 不传到接收端（框架现行契约）；接收端用自己的预算拒绝迟到成功。只用 NATS。
  go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestSLGAllianceRPCReceiverBudget$' ./game/modules/alliance | tee "$review_root/rpc-budget.log"
  if [[ "$(grep -c '^--- PASS: TestSLGAllianceRPCReceiverBudget ' "$review_root/rpc-budget.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/rpc-budget.log"; then exit 1; fi
fi
succeeded=1
