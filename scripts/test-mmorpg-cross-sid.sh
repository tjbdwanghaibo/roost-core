#!/usr/bin/env bash
# 对已启动的隔离 game-demo（sid 1000 / 1001）运行真实跨服赠礼；不启动、不停止服务，不改玩家 sid。
set -euo pipefail
: "${ROOST_MMORPG_DEMO_ROOT:?generated isolated game-demo root required}"
: "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?isolated NATS URL required}"
: "${ROOST_MMORPG_PREFIX:?bus prefix of the isolated demo required}"
: "${ROOST_MMORPG_DATABASE:?Player DAO database of the isolated demo required}"
: "${ROOST_MMORPG_ENDPOINTS:?1000=host:port,1001=host:port required}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-mmo-crosssid.XXXXXX")"
succeeded=0
cleanup() { if [[ "$succeeded" == 1 && "${ROOST_MMORPG_KEEP:-0}" != 1 ]]; then rm -rf "$review_root"; else echo "MMORPG cross-sid evidence kept: $review_root" >&2; fi; }
trap cleanup EXIT
export GOWORK=off GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$ROOST_MMORPG_DEMO_ROOT"
module="$(go list -mod=mod -m)"
sed "s|MMO_MODULE|$module|g" "$repo_root/scripts/testdata/mmorpg/crosssid/main.go.txt" > "$review_root/main.go"
go build -mod=mod -o "$review_root/giftdrill" "$review_root/main.go"
common=(-state "$review_root/state.json" -account-nats "$ROOST_DATAENGINE_IT_NATS_URL" -prefix "$ROOST_MMORPG_PREFIX" -endpoints "$ROOST_MMORPG_ENDPOINTS")
"$review_root/giftdrill" setup "${common[@]}" -items 10 > "$review_root/setup.log" 2>&1
"$review_root/giftdrill" owner "${common[@]}" > "$review_root/owner.log" 2>&1
echo "MMORPG: a real account session was refused by the wrong sid"
"$review_root/giftdrill" send "${common[@]}" -from p1 -to p2 -n 2 -out "$review_root/forward.json" > "$review_root/forward.log" 2>&1
"$review_root/giftdrill" send "${common[@]}" -from p3 -to p4 -n 2 -out "$review_root/back.json" > "$review_root/back.log" 2>&1
"$review_root/giftdrill" wait -mongo "$ROOST_DATAENGINE_IT_MONGO_URI" -saga-db "${ROOST_MMORPG_DATABASE}_saga" -gifts "$review_root/forward.json,$review_root/back.json" -timeout 1m | tee "$review_root/wait.log"
grep -q 'statuses=map\[completed:4\]' "$review_root/wait.log"
for who in p2 p4; do
  "$review_root/giftdrill" claim "${common[@]}" -who "$who" -out "$review_root/claimed-$who.json" > "$review_root/claim-$who.log" 2>&1
  grep -q "^$who claimed=2 " "$review_root/claim-$who.log"
  # 对已领的两封邮件逐封再发 ClaimMail（回复丢失后的重试），不能再增加背包；最后的 Mongo 背包核对兜底。
  "$review_root/giftdrill" claim "${common[@]}" -who "$who" -replay "$review_root/claimed-$who.json" > "$review_root/replay-$who.log" 2>&1
  grep -q "^$who replayed=2 " "$review_root/replay-$who.log"
done
"$review_root/giftdrill" send "${common[@]}" -from p1 -to p2 -to-id 1 -n 1 -out "$review_root/refund.json" > "$review_root/refund.log" 2>&1
"$review_root/giftdrill" wait -mongo "$ROOST_DATAENGINE_IT_MONGO_URI" -saga-db "${ROOST_MMORPG_DATABASE}_saga" -gifts "$review_root/refund.json" -timeout 1m | tee "$review_root/wait-refund.log"
grep -q 'statuses=map\[compensated:1\]' "$review_root/wait-refund.log"
"$review_root/giftdrill" bags -state "$review_root/state.json" -mongo "$ROOST_DATAENGINE_IT_MONGO_URI" -db "$ROOST_MMORPG_DATABASE" -expect-bags p1=8,p2=2,p3=8,p4=2 -timeout 10s | tee "$review_root/bags.log"
echo 'MMORPG cross-sid: four deliveries, replay-safe claims, one compensation and conserved bags passed'
succeeded=1
