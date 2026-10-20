#!/usr/bin/env bash
# 合作生存撤离业务接入纵切：正式生成器、Player / SurvivalRaid DAO、SurvivalRaid 实体定时器、多实体 Sender 与 Nest。
# 业务规则（付费入场、共享采集、制作预扣与容量、环境伤害与治疗、死亡掉落与拾取、撤离读条与封存清单、永久兑付）
# 只在 scripts/testdata/survival-extraction，不是框架默认玩法，生成器也不生成这些文件。
# 缺省不连接外部存储：提交器是替身。ROOST_SURVIVAL_EXTRACTION_REAL=1（并提供隔离 Mongo 副本集
# ROOST_DATAENGINE_IT_MONGO_URI）时追加：子进程在制作与撤离读条都在途时（strict 已确认、WAL 未投影）被父进程 SIGKILL，
# 新进程冷加载、由驱动投递循环一次补触发完成到期事件、续局兑付，再由第三个进程冷加载核对并重试。使用本次唯一库，成功后删除。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-survival-extraction.XXXXXX")"
project_root="$review_root/project"
echo "Survival extraction generated consumer and logs: $review_root"
# 成功后删除临时工程；失败或 ROOST_SURVIVAL_EXTRACTION_KEEP=1 时保留，便于查看生成日志与工程。
succeeded=0
cleanup() {
  if [[ "$succeeded" == 1 && "${ROOST_SURVIVAL_EXTRACTION_KEEP:-0}" != 1 ]]; then
    rm -rf "$review_root"
  else
    echo "Survival extraction generated consumer kept: $review_root" >&2
    # 真实依赖模式失败时 DAO 库与 WAL 留作证据，查完手动删除。
    if [[ "${ROOST_SURVIVAL_EXTRACTION_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then
      echo "Survival extraction real-persistence database left for inspection: ${ROOST_SURVIVAL_EXTRACTION_DB:-unset}" >&2
    fi
  fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new survival-extraction-practice -module example.com/survival-extraction-practice \
  -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
# 先指向本检出：后面 generate 里的 go mod tidy 不去解析可能尚未发布的最低 core 版本，生成物也按本检出编译。
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
export ROOST_SURVIVAL_EXTRACTION_DB="roost_survival_extraction_it_${PPID}_$$"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity SurvivalRaid >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao SurvivalRaid --entity SurvivalRaid >> "$review_root/generate.log" 2>&1
"$review_root/roost" add timer --entity SurvivalRaid >> "$review_root/generate.log" 2>&1
# DAO 定义与实体注解是业务文件；只修改它们，生成组件、访问器与 Sender 由 CLI 生成。
python3 - "$project_root" <<'DEFS'
from pathlib import Path
import sys, os
root = Path(sys.argv[1])
for name, fields in {
    'player': [('Gold', 'int64', 'gold'), ('Initialized', 'bool', 'initialized')],
    'survival_raid': [('State', 'string', 'state'), ('WakeTimer', 'int64', 'wake_timer')],
}.items():
    p = root / 'db/def' / f'{name}.go'
    typename = ''.join(part.title() for part in name.split('_')) + 'Dao'
    marker = 'type ' + typename + ' struct {'
    text = p.read_text().replace(marker, marker + '\n' + ''.join(f'{field} {typ} `bson:"{key}" dao:"persist"`\n' for field, typ, key in fields))
    assert 'db=main' in text, p  # 骨架写逻辑库 main；本次运行的唯一物理库由测试夹具的映射给出
    p.write_text(text)
# SurvivalRaid 与 Player 在同一事务里入场 / 兑付；把 SurvivalRaid 放在 World 锁档（低于 Player），多实体锁序固定。
p = root / 'game/entities/survival_raid/entity.go'
p.write_text(p.read_text().replace('category=entity.EntityCategoryOther', 'category=entity.EntityCategoryWorld'))
DEFS
mkdir -p game/survival
for name in rules state match; do cp "$repo_root/scripts/testdata/survival-extraction/$name.go.txt" "game/survival/$name.go"; done
cp "$repo_root/scripts/testdata/survival-extraction/rules_test.go.txt" game/survival/rules_test.go
cp "$repo_root/scripts/testdata/survival-extraction/timer_handlers.go.txt" game/entities/survival_raid/timer_handlers.go
cp "$repo_root/scripts/testdata/survival-extraction/handlers.go.txt" game/handler/survival_extraction.go
cp "$repo_root/scripts/testdata/survival-extraction/gameplay_test.go.txt" game/handler/survival_extraction_test.go
gofmt -w db/def/*.go game/survival/*.go game/entities/survival_raid/*.go game/handler/survival_extraction*.go
"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestSurvivalExtraction' ./game/survival ./game/handler | tee "$review_root/test.log"
# 12 条场景（4 条规则 + 8 条 Nest）必须都实际执行并通过：-run 写错或用例被跳过时 go test 仍会 ok。
passed="$(grep -c '^--- PASS: TestSurvivalExtraction' "$review_root/test.log" || true)"
if [[ "$passed" != 12 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then
  echo "expected 12 passing TestSurvivalExtraction scenarios, got $passed" >&2
  exit 1
fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
if [[ "${ROOST_SURVIVAL_EXTRACTION_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  for path in "$repo_root"/scripts/testdata/survival-extraction/persistence/*_test.go; do cp "$path" "game/handler/survival_extraction_$(basename "$path")"; done
  gofmt -w game/handler/survival_extraction_*_test.go
  export ROOST_SURVIVAL_EXTRACTION_WAL="$review_root/persistence"
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestSurvivalExtractionRealCrashRecovery$' ./game/handler | tee "$review_root/recovery.log"
  if [[ "$(grep -c '^--- PASS: TestSurvivalExtractionRealCrashRecovery' "$review_root/recovery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/recovery.log"; then exit 1; fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
