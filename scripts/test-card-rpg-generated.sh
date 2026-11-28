#!/usr/bin/env bash
# 卡牌 RPG 业务接入纵切：正式生成器 + 业务模块（R9 批 1）+ 只手写模块没覆盖的部分。
# 模块：enter（建档；初始卡组写在 event.EnterFirst 监听，Q6 只装 enter）、wallet（金币）、bag（钻石、尘与英雄道具，Q2 B）、
# stamina（体力与开局扣体力）、gacha（抽卡、保底、十连、请求回执；扣金币，Q1 A 语义跟 gacha）、task（首胜任务，奖励 10 钻石）。
# 手写（scripts/testdata/card-rpg）：配置校验、卡牌实例（gacha 道具在同一笔事务里转成卡牌，重复卡转碎片）、养成 / 编队、
# HeroStats 属性、CardRun 三人编队回合制 PvE 与回合定时器（借鉴 turn）、首通 / 扫荡（借鉴 dungeon 的有界领奖账本）。
# 这些不是框架默认玩法，生成器也不生成这些文件。
# 缺省不连接外部存储：提交器是替身。ROOST_CARD_RPG_REAL=1（并提供隔离 Mongo 副本集
# ROOST_DATAENGINE_IT_MONGO_URI）时追加：子进程提交（业务命令缺省 async）后、WAL 记录未投影时被父进程 SIGKILL，新进程
# 冷加载、由驱动投递一次过期回合、续战领奖，再由第三个进程冷加载核对并重试。使用本次唯一库，成功后删除。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="${ROOST_GENRE_OUTPUT:-$(mktemp -d "${TMPDIR:-/tmp}/roost-card-rpg.XXXXXX")}"
mkdir -p "$review_root"
project_root="$review_root/project"
echo "Card RPG generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_CARD_RPG_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ -z "${ROOST_GENRE_OUTPUT:-}" && "$succeeded" == 1 && "${ROOST_CARD_RPG_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "Card RPG generated consumer kept: $review_root" >&2
    # 真实依赖模式失败时 DAO 库与 WAL 留作证据，查完手动删除。
    if [[ "${ROOST_CARD_RPG_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then
      echo "Card RPG real-persistence database left for inspection: ${ROOST_CARD_RPG_DB:-unset}" >&2
    fi
  fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new card-rpg-practice -module example.com/card-rpg-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
# 先指向本检出：后面 generate 里的 go mod tidy 不去解析可能尚未发布的最低 core 版本，生成物也按本检出编译。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_CARD_RPG_DB="roost_card_rpg_it_${PPID}_$$"
# 模块清单写在脚本里（Q8：不建配方）；依赖由安装器连带装上（stamina 带上 rules），Player 主 DAO、玩家 TCP 接入由 plan 代办。
genre_modules=(enter wallet bag stamina gacha task)
modules_sh="$repo_root/scripts/testdata/genre-common/modules.sh"
bash "$modules_sh" add "$review_root/roost" "${genre_modules[@]}" >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity CardRun >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao CardRun --entity CardRun >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity CardRun >> "$review_root/generate.log" 2>&1
"$review_root/roost" add attribute HeroStats >> "$review_root/generate.log" 2>&1
# DAO 定义、实体注解与配置表是业务文件；只修改它们，生成组件、访问器与 Sender 由 CLI 生成。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys
root = Path(sys.argv[1])
for name, fields in {
    # 卡牌存档（卡牌实例、编队、养成 / 开局账本、首通与战绩）；资产在模块状态里，不在这里。
    'player': [('State', 'string', 'state')],
    'card_run': [('State', 'string', 'state'), ('TurnTimer', 'int64', 'turn_timer')],
}.items():
    p = root / 'db/def' / f'{name}.go'
    typename = ''.join(part.title() for part in name.split('_')) + 'Dao'
    marker = 'type ' + typename + ' struct {'
    text = p.read_text()
    assert marker in text and 'db=main' in text, p  # 骨架写逻辑库 main；本次运行的唯一物理库由测试夹具的映射给出
    end = text.index('\n}', text.index(marker))
    p.write_text(text[:end] + '\n' + ''.join(f'{field} {typ} `store:"{key}" dao:"nosync"`\n' for field, typ, key in fields) + text[end:])
# CardRun 与 Player 在同一事务里开局 / 领奖；把 Run 放在 World 锁档（低于 Player），多实体锁序固定。
p = root / 'game/entities/card_run/entity.go'
p.write_text(p.read_text().replace('category=entity.EntityCategoryOther', 'category=entity.EntityCategoryWorld'))
# 卡牌的道具、卡池与首胜任务追加在模块自带的表后面（要特定表的模块场景自己装测试表，工程行不影响它们）：
# 2001 钻石、2002 尘、2101～2106 六个英雄道具；卡池 2 单抽 2 金币、连续 4 抽没出 2106 第 5 抽必出；
# 任务 101 永久“首胜”（事件 card.won）奖 10 钻石（task 的 item 奖励由 bag 联动登记）。
# 钻石与尘在建档（enter 的 EnterFirst 监听）时就发，只靠这张表：模块场景的测试先按正式启动的方式装载工程的
# configs/data（安装器写的 roost_config_data_test.go，维护者 2026-11-17），enter 自带的场景照样认得这两个道具号。
rows = {
    'item.csv': ['2001,钻石,0', '2002,尘,0'] + [f'{2100 + i},英雄{i},0' for i in range(1, 7)],
    'gacha_pool.csv': ['2,2,5,2106,false'],
    'gacha_drop.csv': [f'{200 + i},2,{2100 + i},{w}' for i, w in zip(range(1, 7), (27, 27, 26, 7, 7, 6))],
    'task.csv': ['101,card.won,permanent,1,0,item,2001,10,false,,0,'],
}
for name, lines in rows.items():
    p = root / 'configs/table' / name
    text = p.read_text()
    assert text.endswith('\n'), p
    p.write_text(text + ''.join(line + '\n' for line in lines))
DEFS
fixture="$repo_root/scripts/testdata/card-rpg"
mkdir -p game/modules/cardrpg
for name in catalog profile battle; do cp "$fixture/$name.go.txt" "game/modules/cardrpg/$name.go"; done
cp "$fixture/catalog_test.go.txt" game/modules/cardrpg/catalog_test.go
cp "$fixture/attributes.go.txt" game/modules/gameplay/attribute/hero_stats.go
cp "$fixture/timer_handlers.go.txt" game/entities/card_run/timer_handlers.go
cp "$fixture/enter_first.go.txt" game/entities/player/card_rpg.go
cp "$fixture/handlers.go.txt" game/handler/card_rpg.go
cp "$fixture/gameplay_test.go.txt" game/handler/card_rpg_test.go
gofmt -w db/def/*.go game/modules/cardrpg/*.go game/modules/gameplay/attribute/hero_stats.go game/entities/card_run/timer_handlers.go game/entities/player/card_rpg.go game/handler/card_rpg*.go
python3 "$repo_root/scripts/testdata/robot-common/install.py" "$repo_root" "$project_root" card-rpg
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestCardRPG' ./game/modules/cardrpg ./game/handler | tee "$review_root/test.log"
# 17 条场景（含业务 Robot）必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestCardRPG' "$review_root/test.log" || true)"
if [[ "$passed" != 17 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 17 passing TestCardRPG scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
# 已装模块自带的场景照跑：每个模块通过数 == 本检出 modules/<名>/module.yaml 的 verify.pass、0 skip（结果 module-verify.json）。
bash "$modules_sh" verify "$review_root/roost" "$repo_root" "$review_root/module-verify.json" "${genre_modules[@]}" | tee "$review_root/module-verify.txt"
# 真实依赖用例缺省模式也复制进工程并 vet（只编译不运行）：生成器签名变了，发版门禁里就能看到这些夹具编译失败。
for path in "$repo_root"/scripts/testdata/card-rpg/persistence/*_test.go; do cp "$path" "game/handler/card_rpg_$(basename "$path")"; done
gofmt -w game/handler/card_rpg_*_test.go
go vet -mod=mod -p 2 ./game/handler
if [[ "${ROOST_CARD_RPG_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  export ROOST_CARD_RPG_WAL="$review_root/persistence"
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestCardRPGRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
  if [[ "$(grep -c '^--- PASS: TestCardRPGRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
