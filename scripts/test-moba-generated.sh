#!/usr/bin/env bash
# MOBA 业务接入纵切：正式生成器 + 业务模块（R9）+ 只手写模块没覆盖的部分。
# 模块：wallet（玩家金币：结算与原服奖励的胜者 +10 走 Wallet().Add）。
# 手写：Player 的 Wins / Losses 与原服奖励账本 MobaRewards、MatchResult / BattleRound / BattleDirectory DAO、多实体 Sender
# 与 Nest，加框架 Lockstep（Match / Sequencer / FrameAssembler；D5：不迁 Stepper）、真实 loopback KCP 与 service/match
# 领域存储（CommitAcross 跨分区成局，matchmaking 模块只做本 sid）。业务规则只在 scripts/testdata/moba：单路双英雄入门与
# 5v5 三路参考模拟、持久对局、宿主与原服奖励 handler，不是框架默认玩法。
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
review_root="${ROOST_GENRE_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/roost-moba.XXXXXX")}"
mkdir -p "$review_root"
project_root="$review_root/project"
echo "MOBA generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_MOBA_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ -z "${ROOST_GENRE_OUTPUT:-}" && "$succeeded" == 1 && "${ROOST_MOBA_KEEP:-0}" != 1 ]]; then
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
# 先指向本检出：module add 与后面 generate 里的 go mod tidy 不去解析可能尚未发布的最低 core 版本，生成物也按本检出编译。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_MOBA_DB="roost_moba_it_${PPID}_$$"
# 模块清单写在脚本里（Q8：不建配方）；Player 实体与主 DAO 来自 game 模板，wallet 把余额与回执窗口装在 PlayerDao 上。
genre_modules=(wallet)
modules_sh="$repo_root/scripts/testdata/genre-common/modules.sh"
bash "$modules_sh" add "$review_root/roost" "${genre_modules[@]}" >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity MatchResult >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao MatchResult --entity MatchResult >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity BattleRound >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao BattleRound --entity BattleRound >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity BattleDirectory >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao BattleDirectory --entity BattleDirectory >> "$review_root/generate.log" 2>&1
# DAO 定义与实体注解是业务文件；只修改它们，生成组件、访问器与 Sender 由 CLI 生成。
# 字段追加在各 DAO 末尾（PlayerDao 在 wallet 的 Wallet / WalletLedger 之后）；金币在 wallet 里，不再手写 Gold。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys, os
root=Path(sys.argv[1])
for name, fields in {
 'player': [('Wins','int64'),('Losses','int64'),('MobaRewards','string')],
 'battle_directory': [('Payload','string')],
 'battle_round': [('Payload','string')],
 'match_result': [('MatchKey','string'),('Digest','string'),('WinnerID','int64'),('LoserID','int64'),('Status','int64')],
}.items():
 p=root/'db/def'/f'{name}.go'
 typename=''.join(part.title() for part in name.split('_'))+'Dao'
 marker='type '+typename+' struct {'
 text=p.read_text()
 end=text.index('\n}', text.index(marker))
 text=text[:end]+'\n'+''.join(f'\t{field} {typ} `store:"{field.lower()}" dao:"nosync"`\n' for field, typ in fields)+text[end:]
 assert 'db=main' in text, p  # 骨架写逻辑库 main；本次运行的唯一物理库由测试夹具的映射给出
 p.write_text(text)
# 结算记录、对局与比赛目录被多名玩家共同引用，是世界对象，锁序低于 Player。
for name in ('match_result', 'battle_round', 'battle_directory'):
 p=root/'game/entities'/name/'entity.go'
 p.write_text(p.read_text().replace('category=entity.EntityCategoryOther','category=entity.EntityCategoryWorld'))
DEFS
mkdir -p game/modules/moba loadtest protocol/def
cp "$repo_root/scripts/testdata/moba/result_protocol.go.txt" protocol/def/result.go
cp "$repo_root/scripts/testdata/moba/robot.go.txt" loadtest/battle.go
cp "$repo_root/scripts/testdata/moba/simulation.go.txt" game/modules/moba/simulation.go
cp "$repo_root/scripts/testdata/moba/simulation_test.go.txt" game/modules/moba/simulation_test.go
cp "$repo_root/scripts/testdata/moba/network_test.go.txt" game/handler/moba_network_test.go
cp "$repo_root/scripts/testdata/moba/matchmaking_test.go.txt" game/modules/moba/matchmaking_test.go
for name in arena round room directory; do cp "$repo_root/scripts/testdata/moba/$name.go.txt" "game/modules/moba/$name.go"; done
for name in arena_test arena_network_test; do cp "$repo_root/scripts/testdata/moba/$name.go.txt" "game/modules/moba/$name.go"; done
cp "$repo_root/scripts/testdata/moba/round_handlers.go.txt" game/handler/moba_round.go
cp "$repo_root/scripts/testdata/moba/round_handlers_test.go.txt" game/handler/moba_round_test.go
cp "$repo_root/scripts/testdata/moba/settlement.go.txt" game/handler/moba_settlement.go
cp "$repo_root/scripts/testdata/moba/settlement_test.go.txt" game/handler/moba_settlement_test.go
gofmt -w loadtest/*.go db/def/*.go game/modules/moba/*.go game/handler/moba_*.go
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMOBA' ./game/modules/moba ./game/handler | tee "$review_root/test.log"
# 29 条场景（含原服奖励到账回复在记录持久之后）必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestMOBA' "$review_root/test.log" || true)"
if [[ "$passed" != 29 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 29 passing TestMOBA scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
# 已装模块自带的场景照跑：每个模块通过数 == 本检出 modules/<名>/module.yaml 的 verify.pass、0 skip（结果 module-verify.json）。
bash "$modules_sh" verify "$review_root/roost" "$repo_root" "$review_root/module-verify.json" "${genre_modules[@]}" | tee "$review_root/module-verify.txt"
# 真实依赖用例缺省模式也复制进工程并 vet（只编译不运行）：生成器签名变了，发版门禁里就能看到这些夹具编译失败。
cp "$repo_root/scripts/testdata/moba/persistence/persistence_test.go" game/handler/moba_persistence_test.go
gofmt -w game/handler/moba_persistence_test.go
for path in "$repo_root"/scripts/testdata/moba/active/*_test.go; do cp "$path" "game/handler/moba_$(basename "$path")"; done
go vet -mod=mod -p 2 ./game/handler
if [[ "${ROOST_MOBA_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  export ROOST_MOBA_WAL="$review_root/persistence"
  for phase in write recover; do
    ROOST_MOBA_PHASE="$phase" go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMOBARealSettlementAcrossProcesses$' ./game/handler | tee "$review_root/persistence-$phase.log"
    if [[ "$(grep -c '^--- PASS: TestMOBARealSettlementAcrossProcesses' "$review_root/persistence-$phase.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/persistence-$phase.log"; then exit 1; fi
  done
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMOBARealActiveRoundCrashRecovery$' ./game/handler | tee "$review_root/active-crash.log"
  if [[ "$(grep -c '^--- PASS: TestMOBARealActiveRoundCrashRecovery' "$review_root/active-crash.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/active-crash.log"; then exit 1; fi
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL$' ./game/handler | tee "$review_root/active-snapshot.log"
  if [[ "$(grep -c '^--- PASS: TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL' "$review_root/active-snapshot.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/active-snapshot.log"; then exit 1; fi
  if [[ "${ROOST_MOBA_ROUTING:-0}" == 1 ]]; then
    : "${ROOST_DATAENGINE_IT_NATS_URL:?isolated NATS required}"
    go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMOBARealSnapshotDuringHostHandoff$' ./game/handler | tee "$review_root/process-snapshot.log"
    if [[ "$(grep -c '^--- PASS: TestMOBARealSnapshotDuringHostHandoff' "$review_root/process-snapshot.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/process-snapshot.log"; then exit 1; fi
    go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMOBARealCrossSIDHostRouting$' ./game/handler | tee "$review_root/cross-sid.log"
    if [[ "$(grep -c '^--- PASS: TestMOBARealCrossSIDHostRouting' "$review_root/cross-sid.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/cross-sid.log"; then exit 1; fi
  fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
