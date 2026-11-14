#!/usr/bin/env bash
# game-demo 正式模块组合：完整 dev-smoke（三场景），再经独立 Gate 跑双人回合。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo replica set required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?private Redis required}"
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-demo-turn.XXXXXX")"
project="$(printf 'dtr%06x' $(((RANDOM << 9) ^ RANDOM)))"
out="${ROOST_DEMO_TURN_OUT:-$repo_dir/artifacts/demo-turn-$project}"
port_shift="${ROOST_DEMO_TURN_PORT_SHIFT:-35000}"
etcd_port="${ROOST_DEMO_TURN_ETCD_PORT:-57779}"
etcd_pid=""
mkdir -p "$out"
export GOWORK=off GOFLAGS="${GOFLAGS:-} -mod=mod"
source "$repo_dir/scripts/testdata/demo-drill/lib.sh"
trap drill_cleanup EXIT
drill_check_ports
drill_start_etcd
drill_generate
drill_configure
make dev-run >"$out/dev-run.log" 2>&1 || { tail -30 "$out/dev-run.log"; drill_fail "dev-run"; }
# 必须调用实际 Makefile 目标，不能仅跑新场景就声称旧 battle / alliance 路径仍可用。
make dev-smoke LOADTEST_ENDPOINT="127.0.0.1:$(drill_port 7000)" \
  LOADTEST_ACCOUNT_NATS="${ROOST_DATAENGINE_IT_NATS_URL%%,*}" \
  LOADTEST_ARGS="-nats-prefix $project -game-ops 127.0.0.1:$(drill_port 9100)" >"$out/dev-smoke.log" 2>&1 || {
  tail -30 "$out/dev-smoke.log"; drill_fail "dev-smoke";
}
passed=$(grep -c 'robot runner: stop.*success=2 failure=0 canceled=0' "$out/dev-smoke.log" || true)
[[ "$passed" == 3 ]] || drill_fail "dev-smoke ran $passed passing scenarios, want 3"
drill_log "PASS make dev-smoke: demo, alliance_offline_join, turn; each 2/2 robots"
sh deploy/dev/run.sh stop game >/dev/null
drill_ingress_config configs/service/config.game.yaml configs/service/config.game-ingress.yaml
drill_gate_config
drill_start game-ingress bin/drillingress game 1000 configs/service/config.game-ingress.yaml
drill_start gate bin/drillgate gate 9500 configs/service/config.gate.yaml \
  DRILL_GATE_ADDR="127.0.0.1:$(drill_port 7100)" DRILL_GATE_ADVERTISE="127.0.0.1:$(drill_port 7100)"
drill_robots gate-turn -endpoint "127.0.0.1:$(drill_port 7100)" -scenario turn -count 2
drill_log "PASS demo Room/Turn: real matchmaking, TCP and Gate, full game, same-connection replay, reconnect epoch and final replay"
