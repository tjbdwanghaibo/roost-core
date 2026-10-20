#!/usr/bin/env bash
# 联盟活动同一条链路继续到真实 Player 资产与永久回执；两个原服各自 WAL / 子进程。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?set isolated Redis address}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?set isolated NATS URL}"
: "${ROOST_DATAENGINE_IT_MONGO_URI:?set isolated Mongo replica-set URI}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-slg-reward.XXXXXX")"
project_root="$review_root/project"
echo "SLG Player delivery consumer and logs: $review_root"
succeeded=0
cleanup() {
 if [[ "$succeeded" == 1 && "${ROOST_SLG_REWARD_KEEP:-0}" != 1 ]]; then rm -rf "$review_root";
 else echo "SLG Player evidence retained: $review_root; database: ${ROOST_SLG_REWARD_DB:-unset}" >&2; fi
}
trap cleanup EXIT
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"
export ROOST_SLG_REWARD_DB="roost_slg_reward_it_${PPID}_$$"
export ROOST_SLG_REWARD_WAL="$review_root/persistence"
cd "$repo_root"
go build -o "$review_root/roost" ./codegen/cmd/roost
"$review_root/roost" project new slg-alliance-practice -module example.com/slg-alliance-practice -out "$project_root" -template game --skip-deps > "$review_root/generate.log" 2>&1
cd "$project_root"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
"$review_root/roost" add dao Player --entity Player >> "$review_root/generate.log" 2>&1
python3 - "$project_root/db/def/player.go" <<'PY'
from pathlib import Path
import os,sys
p=Path(sys.argv[1]);s=p.read_text().replace('type PlayerDao struct {','''type PlayerDao struct {
 HomeSID int32 `bson:"home_sid" dao:"persist"`
 Gold int64 `bson:"gold" dao:"persist"`
 Receipts string `bson:"receipts" dao:"persist"`''').replace('db=slg_alliance_practice','db='+os.environ['ROOST_SLG_REWARD_DB']);p.write_text(s)
PY
cp "$repo_root/scripts/testdata/slg-alliance/player/handlers.go.txt" game/handler/alliance_rewards.go
mkdir -p game/alliance
cp "$repo_root"/scripts/testdata/slg-alliance/*.go game/alliance/
for name in runtime delivery; do cp "$repo_root/scripts/testdata/slg-alliance/player/${name}_test.go" "game/alliance/player_${name}_test.go"; done
if [[ "${ROOST_SLG_REMOTE:-0}" == 1 ]]; then
 "$review_root/roost" add mod remote_entity -service game >> "$review_root/generate.log" 2>&1
 "$review_root/roost" add entity AllianceAuthority >> "$review_root/generate.log" 2>&1
 "$review_root/roost" add dao AllianceAuthority --entity AllianceAuthority >> "$review_root/generate.log" 2>&1
 python3 - "$project_root" <<'REMOTE'
from pathlib import Path
import os,sys
root=Path(sys.argv[1]);p=root/'game/entities/alliance_authority/entity.go'
s=p.read_text().replace('category=entity.EntityCategoryOther','category=entity.EntityCategoryRemote remote=managed').replace('*entity.EntityBase','*entity.RemoteEntityBase');p.write_text(s)
p=root/'db/def/alliance_authority.go';s=p.read_text().replace('type AllianceAuthorityDao struct {','type AllianceAuthorityDao struct {\n State string `bson:"state" dao:"persist"`').replace('db=slg_alliance_practice','db='+os.environ['ROOST_SLG_REWARD_DB']+' dbscope=global');p.write_text(s)
REMOTE
 cp "$repo_root/scripts/testdata/slg-alliance/remote/handlers.go.txt" game/handler/alliance_authority.go
 for name in runtime store remote; do cp "$repo_root/scripts/testdata/slg-alliance/remote/${name}_test.go" "game/alliance/remote_${name}_test.go"; done
fi
gofmt -w db/def/*.go game/handler/*.go game/alliance/*.go

"$review_root/roost" generate >> "$review_root/generate.log" 2>&1
if [[ "${ROOST_SLG_REMOTE:-0}" == 1 ]]; then
 go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestSLGAllianceRemoteDelivery$' ./game/alliance | tee "$review_root/delivery.log"
 expected=TestSLGAllianceRemoteDelivery
else
 go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestSLGAlliancePlayerDelivery$' ./game/alliance | tee "$review_root/delivery.log"
 expected=TestSLGAlliancePlayerDelivery
fi
if [[ "$(grep -c "^--- PASS: $expected " "$review_root/delivery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/delivery.log"; then exit 1; fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
"$review_root/roost" id check
"$review_root/roost" generate --check
succeeded=1
