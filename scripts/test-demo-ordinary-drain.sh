#!/usr/bin/env bash
# Private real-process check: disconnected resident -> SIGTERM -> saved last point -> reconnect.
# Source the env.sh of a fresh mirror-local first. Never run on a shared deployment.
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo required}" "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}" "${ROOST_DATAENGINE_IT_REDIS_ADDR:?private Redis required}" "${DEMO_DRAIN_OUT:?new absolute evidence directory required}"
[[ "$DEMO_DRAIN_OUT" == /* && ! -e "$DEMO_DRAIN_OUT" && "${DRILL_KEEP:-0}" == 0 ]] || exit 2
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$DEMO_DRAIN_OUT"; mkdir "$out"; work_dir="$out/work"; mkdir "$work_dir"
project="$(printf 'drn%06x' $(((RANDOM << 9) ^ RANDOM)))"; etcd_pid=''
port_shift="${DEMO_DRAIN_PORT_SHIFT:-27113}"; etcd_port="${DEMO_DRAIN_ETCD_PORT:-34265}"
export GOWORK=off GOFLAGS='-p=2 -mod=mod'
source "$repo_dir/scripts/testdata/demo-drill/lib.sh"
trap drill_cleanup EXIT
drill_check_ports; drill_start_etcd; drill_generate; drill_configure
mkdir -p cmd/drainprobe
sed -e "s|DRILL_MODULE|example.com/$project|g" -e 's/^func main()/func mergeMain()/' "$repo_dir/scripts/testdata/demo-drill/merge.go.txt" >cmd/drainprobe/merge.go
sed "s|DRILL_MODULE|example.com/$project|g" "$repo_dir/scripts/testdata/demo-drill/rehome.go.txt" >cmd/drainprobe/main.go
go build -o bin/drainprobe ./cmd/drainprobe
make dev-run >"$out/start.log" 2>&1
probe() { bin/drainprobe "$1" -state "$out/state.json" -account-nats "${ROOST_DATAENGINE_IT_NATS_URL%%,*}" -prefix "$project" -endpoints "1000=127.0.0.1:$(drill_port 7000)"; }
# Setup uses the real TCP client to move to (620,540), then closes the socket.
position() { mongosh --quiet "$ROOST_DATAENGINE_IT_MONGO_URI" --eval "const p=db.getSiblingDB('${project}_game_1000').player.findOne({_id:Long('$id')});print(JSON.stringify(p ? [Number(p.saved_pos_x),Number(p.saved_pos_y)] : null));"; }
# Periodic saves have an entity-specific phase. If a periodic save won the race,
# that sample says nothing about shutdown. Keep it and select a different role.
for sample in 1 2 3; do
  probe setup >"$out/role-$sample.json"
  id="$(jq -er .entity_id "$out/role-$sample.json")"; [[ "$id" =~ ^[0-9]+$ ]] || exit 1
  position >"$out/before-$sample.json"
  if jq -e '. == [500,500]' "$out/before-$sample.json" >/dev/null; then
    cp "$out/before-$sample.json" "$out/before.json"; cp "$out/role-$sample.json" "$out/role.json"; break
  fi
done
[[ -f "$out/before.json" ]] || drill_fail 'inconclusive: no resident had an unsaved last point at stop'
sh deploy/dev/run.sh stop game >"$out/stop.log" 2>&1
! grep -Fq 'ordinary map drain incomplete' .dev/game.log || drill_fail 'drain incomplete'
grep -Fq 'server stopped' .dev/game.log || drill_fail 'normal stop not confirmed'
position >"$out/after.json"
jq -e '. == [620,540]' "$out/after.json" >/dev/null || drill_fail 'disconnected resident lost its last point'
# The same final point can come from a periodic save. Match the exact entity (int64)
# and the transaction outcome, not a substring of a log line or a pair of Mongo snapshots.
python3 - .dev/game.log "$id" "$out/drain-event.json" <<'PY'
import json, sys
matches = []
with open(sys.argv[1]) as log:
    for line in log:
        try:
            event = json.loads(line)
        except ValueError:
            continue  # dev-run also writes plain text
        if isinstance(event, dict) and event.get('msg') == 'ordinary map drain released resident' and event.get('entity_id') == int(sys.argv[2]):
            matches.append(event)
if len(matches) != 1:
    sys.exit('drain evidence missing or ambiguous for entity ' + sys.argv[2])
event = matches[0]
with open(sys.argv[3], 'w') as out:
    json.dump(event, out)
if event.get('position_changed') is not True:
    sys.exit('inconclusive: final position is correct, but the drain did not prove a coordinate write for this resident')
PY
sh deploy/dev/run.sh start game >"$out/restart.log" 2>&1
probe resume >"$out/resume.log"
drill_log 'PASS: normal stop persisted the last point; fresh ordinary map admitted the same player'
