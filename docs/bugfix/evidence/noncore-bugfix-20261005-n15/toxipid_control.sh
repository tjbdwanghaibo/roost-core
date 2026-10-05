#!/usr/bin/env bash
# NC-202 控制：pid 文件指向本环境（偏移 31000 → API 49474）自起的 toxiproxy 时，running 为真、down 照常停止它。
set -uo pipefail
lib="$1/kit/scripts/integration/lib"
home="$(mktemp -d "${TMPDIR:-/tmp}/n15-toxipid.XXXXXX")"; home="$(cd "$home" && pwd -P)"
mkdir -p "$home/roost-dataengine-it/toxiproxy"; printf '31000\n' > "$home/roost-dataengine-it/port-offset"
toxiproxy-server -host 127.0.0.1 -port 49474 >/dev/null 2>&1 & own=$!
trap 'kill "$own" 2>/dev/null; rm -rf "$home"' EXIT
printf '%s\n' "$own" > "$home/roost-dataengine-it/toxiproxy/toxiproxy.pid"
sleep 1
env -u ROOST_DATAENGINE_IT_ROOT ROOST_IT_HOME="$home" ROOST_IT_PORT_OFFSET=31000 bash -c '
source "$0/common.sh"; source "$0/mongo.sh"; source "$0/nats.sh"; source "$0/redis.sh"; source "$0/toxiproxy.sh"
toxiproxy_running && echo "toxiproxy_running=true" || echo "toxiproxy_running=false"
toxiproxy_down; echo "toxiproxy_down exit=$?"; [[ -f "$(toxiproxy_pid_file)" ]] && echo "pid file kept" || echo "pid file removed"
' "$lib" 2>&1
sleep 0.5; kill -0 "$own" 2>/dev/null && echo "RESULT: own toxiproxy still running" || echo "RESULT: own toxiproxy stopped"
