#!/usr/bin/env bash
# MOBA 业务消费者：正式工程 / DAO / Entity / Nest Sender、Lockstep 与确定性参考模拟。
set -euo pipefail
if [[ "${ROOST_MOBA_ROUTING:-0}" == 1 && "${ROOST_MOBA_REAL:-0}" != 1 ]]; then echo "ROOST_MOBA_ROUTING=1 requires ROOST_MOBA_REAL=1" >&2; exit 2; fi
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-moba.XXXXXX")"
succeeded=0
cleanup() { if [[ "$succeeded" == 1 && "${ROOST_MOBA_KEEP:-0}" != 1 ]]; then rm -rf "$review_root"; else echo "MOBA consumer kept: $review_root" >&2; if [[ "${ROOST_MOBA_REAL:-0}" == 1 && "$succeeded" != 1 ]]; then echo "MOBA database namespace for inspection (completed phases may have removed it): ${ROOST_MOBA_DB:-unset}" >&2; fi; fi; }
trap cleanup EXIT
export GOWORK=off GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
echo "MOBA generated consumer: $review_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new moba-practice -module example.com/moba-practice -out "$review_root/project" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$review_root/project"
export ROOST_MOBA_DB="roost_moba_it_${PPID}_$$"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity MatchResult >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao MatchResult --entity MatchResult >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity BattleRound >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao BattleRound --entity BattleRound >> "$review_root/generate.log" 2>&1
"$review_root/roost" add entity BattleDirectory >> "$review_root/generate.log" 2>&1
"$review_root/roost" add dao BattleDirectory --entity BattleDirectory >> "$review_root/generate.log" 2>&1
python3 - <<'DEFS'
from pathlib import Path
import os
for name, fields in {
 'player': [('Gold','int64'),('Wins','int64'),('Losses','int64'),('MobaRewards','string')],
 'battle_directory':[('Payload','string')],
 'battle_round':[('Payload','string')],
 'match_result':[('MatchKey','string'),('Digest','string'),('WinnerID','int64'),('LoserID','int64'),('Status','int64')],
}.items():
 p=Path('db/def')/(name+'.go');typename=''.join(part.title() for part in name.split('_'))+'Dao'
 s=p.read_text().replace('type '+typename+' struct {','type '+typename+' struct {\n'+''.join(f'{field} {typ} `bson:"{field.lower()}" dao:"persist"`\n' for field,typ in fields));s=s.replace("db=moba_practice", "db="+os.environ["ROOST_MOBA_DB"]);p.write_text(s)
for name in ('match_result','battle_round','battle_directory'):
 p=Path('game/entities')/name/'entity.go';p.write_text(p.read_text().replace('category=entity.EntityCategoryOther','category=entity.EntityCategoryWorld'))
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
passed="$(grep -c '^--- PASS: TestMOBA' "$review_root/test.log" || true)"
if [[ "$passed" != 25 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then echo "expected 25 passing MOBA scenarios, got $passed" >&2; exit 1; fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
if [[ "${ROOST_MOBA_REAL:-0}" == 1 ]]; then
  : "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
  cp "$repo_root/scripts/testdata/moba/persistence/persistence_test.go" game/handler/moba_persistence_test.go
  gofmt -w game/handler/moba_persistence_test.go
  for path in "$repo_root"/scripts/testdata/moba/active/*_test.go; do cp "$path" "game/handler/moba_$(basename "$path")"; done
  export ROOST_MOBA_WAL="$review_root/persistence"
  for phase in write recover; do
    ROOST_MOBA_PHASE="$phase" go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMOBARealSettlementAcrossProcesses$' ./game/handler | tee "$review_root/persistence-$phase.log"
    if [[ "$(grep -c '^--- PASS: TestMOBARealSettlementAcrossProcesses' "$review_root/persistence-$phase.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/persistence-$phase.log"; then exit 1; fi
  done
  go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMOBARealActiveRoundCrashRecovery$' ./game/handler | tee "$review_root/active-crash.log"
  if [[ "$(grep -c '^--- PASS: TestMOBARealActiveRoundCrashRecovery' "$review_root/active-crash.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/active-crash.log"; then exit 1; fi
  if [[ "${ROOST_MOBA_ROUTING:-0}" == 1 ]]; then
    : "${ROOST_DATAENGINE_IT_NATS_URL:?isolated NATS required}"
    go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMOBARealCrossSIDHostRouting$' ./game/handler | tee "$review_root/cross-sid.log"
    if [[ "$(grep -c '^--- PASS: TestMOBARealCrossSIDHostRouting' "$review_root/cross-sid.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/cross-sid.log"; then exit 1; fi
  fi
  go vet -mod=mod -p 2 ./...
fi
succeeded=1
