#!/usr/bin/env bash
# NC-202：toxiproxy.pid 指向的进程已不是本环境的 toxiproxy（pid 复用 / 陈旧 pid 文件）时，down 不能杀它。
set -uo pipefail
lib="$1/kit/scripts/integration/lib"
home="$(mktemp -d "${TMPDIR:-/tmp}/n15-toxipid.XXXXXX")"; home="$(cd "$home" && pwd -P)"
sleep 300 & bystander=$!
trap 'kill "$bystander" 2>/dev/null; rm -rf "$home"' EXIT
mkdir -p "$home/roost-dataengine-it/toxiproxy"
printf '31000\n' > "$home/roost-dataengine-it/port-offset"
printf '%s\n' "$bystander" > "$home/roost-dataengine-it/toxiproxy/toxiproxy.pid"
env -u ROOST_DATAENGINE_IT_ROOT ROOST_IT_HOME="$home" ROOST_IT_PORT_OFFSET=31000 bash -c '
source "$0/common.sh"; source "$0/mongo.sh"; source "$0/nats.sh"; source "$0/redis.sh"; source "$0/toxiproxy.sh"
require_safe_root || exit 9
if toxiproxy_running; then echo "toxiproxy_running=true (pid $(cat "$(toxiproxy_pid_file)") is: $(ps -p "$(cat "$(toxiproxy_pid_file)")" -o command=))"; else echo "toxiproxy_running=false"; fi
toxiproxy_down; echo "toxiproxy_down exit=$?"
' "$lib" 2>&1
sleep 0.5
if kill -0 "$bystander" 2>/dev/null; then echo "RESULT: bystander pid $bystander alive"; else echo "RESULT: bystander pid $bystander KILLED"; fi
