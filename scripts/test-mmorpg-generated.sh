#!/usr/bin/env bash
# MMORPG 业务接入纵切：正式生成器 + 业务模块（R9 批 2B）+ 只手写模块没覆盖的部分。
# 模块（Q5 A）：wallet（金币：交易、托管、任务奖励）、bag（交易与掉落的材料道具 3001）、task（击杀任务：击杀时 Report，
# 领奖 Claim 经 wallet 发 5 金币）。手写（scripts/testdata/mmorpg）：两人队伍、Scene（人数、切图版本）、Monster（血量、
# 掉落归属）、多人战斗与冷却、Buff / 复活定时器、InstanceComplete、Trade 托管（借鉴 friends 的两 Player 同事务写法）。
# 这些留到 CellWorld 进模块那一轮再定（team / scene / monster / combat / dungeon 模块与夹具撞名或语义不同，见 MMORPG-INTEGRATION §4.5）。
# 生成器部分：Player / Scene / Trade / Monster DAO、Player 持久定时器、多实体 Sender 与 Nest。
# 缺省不连接外部存储：提交器是替身，验证内存事务组合。ROOST_MMORPG_REAL=1（并提供隔离 Mongo 副本集
# ROOST_DATAENGINE_IT_MONGO_URI）时追加两个真实文件 WAL / Mongo 用例，定时器到期都由 Player 驱动的投递循环投递：
# 两个独立进程正常停机后冷加载恢复；子进程提交（业务命令缺省 async）后、WAL 记录未投影时被父进程 SIGKILL，新进程冷加载、
# 由循环补触发离线到期的 Buff / 复活并按时投递下一期限，再由第三个进程冷加载核对并重试。使用本次唯一库，成功后删除。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="${ROOST_GENRE_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/roost-mmorpg.XXXXXX")}"
mkdir -p "$review_root"
project_root="$review_root/project"
echo "MMORPG generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_MMORPG_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ -z "${ROOST_GENRE_OUTPUT:-}" && "$succeeded" == 1 && "${ROOST_MMORPG_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "MMORPG generated consumer kept: $review_root" >&2
    # 真实依赖模式失败时 DAO 库留作证据，查完手动删除。
    if [[ "${ROOST_MMORPG_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then
      echo "MMORPG real-persistence database left for inspection: ${ROOST_MMORPG_DB:-unset}" >&2
    fi
  fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new mmorpg-practice -module example.com/mmorpg-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
# 先指向本检出：后面 generate 里的 go mod tidy 不去解析可能尚未发布的最低 core 版本，生成物也按本检出编译。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_MMORPG_DB="roost_mmo_it_${PPID}_$$"
# 模块清单写在脚本里（Q8：不建配方）；Player 主 DAO 由 game 模板给出，模块状态是它的嵌套字段。
genre_modules=(wallet bag task)
modules_sh="$repo_root/scripts/testdata/genre-common/modules.sh"
bash "$modules_sh" add "$review_root/roost" "${genre_modules[@]}" >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity Player >> "$review_root/generate.log" 2>&1
for name in Scene Trade Monster; do
  "$review_root/roost" add entity "$name" >> "$review_root/generate.log" 2>&1
  "$review_root/roost" add dao "$name" --entity "$name" >> "$review_root/generate.log" 2>&1
done
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys
root=Path(sys.argv[1])
for name, fields in {
 # 金币、材料道具、击杀任务在模块状态里（PlayerDao.Wallet / Bag / Task），这里只留玩法自己的事实。
 'player': ['PartyID', 'SceneID', 'SceneVersion', 'TradeSeq', 'TradeBuyer', 'TradeAmount', 'HP', 'AttackSeq', 'LastAttackTarget', 'CooldownUntil', 'BuffValue', 'BuffTimer', 'ReviveTimer'],
 'scene': ['Occupants', 'InstanceComplete'],
 'trade': ['Status', 'SellerID', 'BuyerID', 'Price', 'Count', 'Deadline', 'SellerConfirmed', 'BuyerConfirmed'],
 'monster': ['HP', 'SceneID', 'LootOwner', 'LootTaken'],
}.items():
 p=root/'db/def'/f'{name}.go'
 text=p.read_text(); marker='type '+name.title()+'Dao struct {'
 assert marker in text and 'db=main' in text, p  # 骨架写逻辑库 main；本次运行的唯一物理库由测试夹具的映射给出
 end=text.index('\n}', text.index(marker))
 p.write_text(text[:end]+'\n'+''.join(f'{field} int64 `store:"{field.lower()}" dao:"nosync"`\n' for field in fields)+text[end:])
for name in ('scene', 'trade', 'monster'):
 p=root/f'game/entities/{name}/entity.go'
 p.write_text(p.read_text().replace('category=entity.EntityCategoryOther','category=entity.EntityCategoryWorld'))
# 追加在模块自带的表后面（模块场景用自己的表，工程行不影响它们）：3001 是交易与掉落的材料道具，单种上限 1000
# （与 handler 的 mmoItemLimit 一致）；201 号永久任务“击杀 1 只怪”（事件 mmo.kill）奖 5 金币。
for name, lines in {
 'item.csv': ['3001,MMO 材料,1000'],
 'task.csv': ['201,mmo.kill,permanent,1,0,coins,0,5,false'],
}.items():
 p=root/'configs/table'/name
 text=p.read_text()
 assert text.endswith('\n'), p
 p.write_text(text+''.join(line+'\n' for line in lines))
DEFS
cp "$repo_root/scripts/testdata/mmorpg/tasks.go.txt" game/handler/mmo_tasks.go
cp "$repo_root/scripts/testdata/mmorpg/tasks_test.go.txt" game/handler/mmo_tasks_test.go
cp "$repo_root/scripts/testdata/mmorpg/gameplay.go.txt" game/handler/mmo_gameplay.go
cp "$repo_root/scripts/testdata/mmorpg/gameplay_test.go.txt" game/handler/mmo_gameplay_test.go
cp "$repo_root/scripts/testdata/mmorpg/timer_handlers.go.txt" game/entities/player/timer_handlers.go
gofmt -w db/def/*.go game/handler/mmo_*.go game/entities/player/timer_handlers.go
python3 "$repo_root/scripts/testdata/robot-common/install.py" "$repo_root" "$project_root" mmorpg
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMMORPG(Party|Trade|Combat|Buff|Transfer|Death|Robot)' ./game/handler | tee "$review_root/test.log"
# 14 条场景必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestMMORPG' "$review_root/test.log" || true)"
if [[ "$passed" != 14 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 14 passing TestMMORPG scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
# 已装模块自带的场景照跑：每个模块通过数 == 本检出 modules/<名>/module.yaml 的 verify.pass、0 skip（结果 module-verify.json）。
bash "$modules_sh" verify "$review_root/roost" "$repo_root" "$review_root/module-verify.json" "${genre_modules[@]}" | tee "$review_root/module-verify.txt"
# 真实依赖用例缺省模式也复制进工程并 vet（只编译不运行）：生成器签名变了，发版门禁里就能看到这些夹具编译失败。
for path in "$repo_root"/scripts/testdata/mmorpg/persistence/*_test.go; do cp "$path" "game/handler/mmo_$(basename "$path")"; done
gofmt -w game/handler/mmo_persistence_test.go game/handler/mmo_recovery_test.go
go vet -mod=mod -p 2 ./game/handler
if [[ "${ROOST_MMORPG_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  # 两个用例各用自己的 WAL 根（共用本次唯一库；正常停机用例成功时先删库，崩溃用例再从空库开始）。
  for phase in write recover; do
    ROOST_MMORPG_WAL="$review_root/persistence" ROOST_MMORPG_PHASE="$phase" go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMMORPGRealPersistenceAcrossProcesses$' ./game/handler | tee "$review_root/persistence-$phase.log"
    if [[ "$(grep -c '^--- PASS: TestMMORPGRealPersistenceAcrossProcesses' "$review_root/persistence-$phase.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/persistence-$phase.log"; then exit 1; fi
  done
  ROOST_MMORPG_WAL="$review_root/crash" go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMMORPGRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
  if [[ "$(grep -c '^--- PASS: TestMMORPGRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
