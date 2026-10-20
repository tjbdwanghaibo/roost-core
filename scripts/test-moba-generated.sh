#!/usr/bin/env bash
# MOBA 业务接入纵切：正式生成器、Player / MatchResult / BattleRound / BattleDirectory DAO、多实体 Sender
# 与 Nest，加框架 Lockstep（Match / Sequencer / FrameAssembler）、真实 loopback KCP 与 service/match 领域存储。
# 业务规则只在 scripts/testdata/moba：单路双英雄入门与 5v5 三路参考模拟、持久对局、宿主与原服奖励 handler，
# 不是框架默认玩法。
# 缺省不连接外部存储：提交器是替身。ROOST_MOBA_REAL=1（并提供隔离 Mongo 副本集
# ROOST_DATAENGINE_IT_MONGO_URI）时追加：两个独立进程正常停机后冷加载的结算重放，以及活动对局未投影时
# SIGKILL 子进程后的 WAL 恢复续赛，以及 SIGKILL 后冷加载、经已提交检查点快照恢复窗口外客户端的演练（两者都用
# RecoverRoomWithSnapshots 重建宿主）；再加 ROOST_MOBA_ROUTING=1（并提供 ROOST_DATAENGINE_IT_NATS_URL）时
# 追加五进程真实 NATS 宿主交接与原服奖励崩溃重投。各阶段用本次唯一库，完成后删除。
set -euo pipefail
if [[ "${ROOST_MOBA_ROUTING:-0}" == 1 && "${ROOST_MOBA_REAL:-0}" != 1 ]]; then
  echo "ROOST_MOBA_ROUTING=1 requires ROOST_MOBA_REAL=1" >&2
  exit 2
fi
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-moba.XXXXXX")"
project_root="$review_root/project"
echo "MOBA generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_MOBA_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ "$succeeded" == 1 && "${ROOST_MOBA_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "MOBA generated consumer kept: $review_root" >&2
    # 真实依赖模式失败时 DAO 库留作证据，查完手动删除。
    if [[ "${ROOST_MOBA_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then
      echo "MOBA real-persistence database left for inspection: ${ROOST_MOBA_DB:-unset}" >&2
    fi
  fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new moba-practice -module example.com/moba-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
export ROOST_MOBA_DB="roost_moba_it_${PPID}_$$"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity MatchResult >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao MatchResult --entity MatchResult >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity BattleRound >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao BattleRound --entity BattleRound >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity BattleDirectory >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao BattleDirectory --entity BattleDirectory >> "$review_root/generate.log" 2>&1
# DAO 定义与实体注解是业务文件；只修改它们，生成组件、访问器与 Sender 由 CLI 生成。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys, os
root=Path(sys.argv[1])
for name, fields in {
 'player': [('Gold','int64'),('Wins','int64'),('Losses','int64'),('MobaRewards','string')],
 'battle_directory': [('Payload','string')],
 'battle_round': [('Payload','string')],
 'match_result': [('MatchKey','string'),('Digest','string'),('WinnerID','int64'),('LoserID','int64'),('Status','int64')],
}.items():
 p=root/'db/def'/f'{name}.go'
 typename=''.join(part.title() for part in name.split('_'))+'Dao'
 marker='type '+typename+' struct {'
 text=p.read_text().replace(marker, marker+'\n'+''.join(f'{field} {typ} `bson:"{field.lower()}" dao:"persist"`\n' for field, typ in fields))
 assert 'db=main' in text, p  # 骨架写逻辑库 main；本次运行的唯一物理库由测试夹具的映射给出
 p.write_text(text)
# 结算记录、对局与比赛目录被多名玩家共同引用，是世界对象，锁序低于 Player。
for name in ('match_result', 'battle_round', 'battle_directory'):
 p=root/'game/entities'/name/'entity.go'
 p.write_text(p.read_text().replace('category=entity.EntityCategoryOther','category=entity.EntityCategoryWorld'))
DEFS
mkdir -p game/moba
cp "$repo_root/scripts/testdata/moba/simulation.go.txt" game/moba/simulation.go
cp "$repo_root/scripts/testdata/moba/simulation_test.go.txt" game/moba/simulation_test.go
cp "$repo_root/scripts/testdata/moba/network_test.go.txt" game/moba/network_test.go
cp "$repo_root/scripts/testdata/moba/matchmaking_test.go.txt" game/moba/matchmaking_test.go
for name in arena round room directory; do cp "$repo_root/scripts/testdata/moba/$name.go.txt" "game/moba/$name.go"; done
for name in arena_test arena_network_test; do cp "$repo_root/scripts/testdata/moba/$name.go.txt" "game/moba/$name.go"; done
cp "$repo_root/scripts/testdata/moba/round_handlers.go.txt" game/handler/moba_round.go
cp "$repo_root/scripts/testdata/moba/round_handlers_test.go.txt" game/handler/moba_round_test.go
cp "$repo_root/scripts/testdata/moba/settlement.go.txt" game/handler/moba_settlement.go
cp "$repo_root/scripts/testdata/moba/settlement_test.go.txt" game/handler/moba_settlement_test.go
gofmt -w db/def/*.go game/moba/*.go game/handler/moba_*.go
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMOBA' ./game/moba ./game/handler | tee "$review_root/test.log"
# 28 条场景必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestMOBA' "$review_root/test.log" || true)"
if [[ "$passed" != 28 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 28 passing TestMOBA scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
if [[ "${ROOST_MOBA_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  cp "$repo_root/scripts/testdata/moba/persistence/persistence_test.go" game/handler/moba_persistence_test.go
  gofmt -w game/handler/moba_persistence_test.go
  export ROOST_MOBA_WAL="$review_root/persistence"
  for phase in write recover; do
    ROOST_MOBA_PHASE="$phase" go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMOBARealSettlementAcrossProcesses$' ./game/handler | tee "$review_root/persistence-$phase.log"
    if [[ "$(grep -c '^--- PASS: TestMOBARealSettlementAcrossProcesses' "$review_root/persistence-$phase.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/persistence-$phase.log"; then exit 1; fi
  done
  for path in "$repo_root"/scripts/testdata/moba/active/*_test.go; do cp "$path" "game/handler/moba_$(basename "$path")"; done
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMOBARealActiveRoundCrashRecovery$' ./game/handler | tee "$review_root/active-crash.log"
  if [[ "$(grep -c '^--- PASS: TestMOBARealActiveRoundCrashRecovery' "$review_root/active-crash.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/active-crash.log"; then exit 1; fi
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL$' ./game/handler | tee "$review_root/active-snapshot.log"
  if [[ "$(grep -c '^--- PASS: TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL' "$review_root/active-snapshot.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/active-snapshot.log"; then exit 1; fi
  if [[ "${ROOST_MOBA_ROUTING:-0}" == 1 ]]; then
    : "${ROOST_DATAENGINE_IT_NATS_URL:?isolated NATS required}"
    go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMOBARealCrossSIDHostRouting$' ./game/handler | tee "$review_root/cross-sid.log"
    if [[ "$(grep -c '^--- PASS: TestMOBARealCrossSIDHostRouting' "$review_root/cross-sid.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/cross-sid.log"; then exit 1; fi
  fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
