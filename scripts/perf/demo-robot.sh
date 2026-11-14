#!/usr/bin/env bash
# 一次只跑一个完整 game-demo Robot 档；每次私有依赖和工程都全新。
# 默认仅 2 人冒烟；正式六档见 scripts/testdata/demo-robot/README.md。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
: "${DEMO_ROBOT_OUT:?new absolute evidence directory required}"
[[ "$DEMO_ROBOT_OUT" == /* && ! -e "$DEMO_ROBOT_OUT" ]] || { echo 'DEMO_ROBOT_OUT must be a new absolute directory' >&2; exit 2; }
robots="${DEMO_ROBOT_COUNT:-2}"
mode="${DEMO_ROBOT_MODE:-burst}"
port_shift="${DEMO_ROBOT_PORT_SHIFT:-31000}"
etcd_port="${DEMO_ROBOT_ETCD_PORT:-35479}"
offset="${DEMO_ROBOT_MIRROR_OFFSET:-37000}"
case "$robots" in 2|100|200);; *) echo 'DEMO_ROBOT_COUNT must be 2 (smoke), 100 or 200' >&2; exit 2;; esac
case "$mode" in burst) step="$robots"; interval=0;; ramp20) step=20; interval=1s;; ramp10) step=10; interval=1s;; *) echo 'mode must be burst, ramp20 or ramp10' >&2; exit 2;; esac
for value in "$port_shift" "$etcd_port" "$offset"; do
  [[ "$value" =~ ^[1-9][0-9]*$ ]] || { echo 'ports/offsets must be positive integers' >&2; exit 2; }
done
(( port_shift <= 56235 && etcd_port < 65535 && offset <= 38135 && offset != 1000 )) || { echo 'invalid port range / shared mirror offset' >&2; exit 2; }
[[ "${DRILL_KEEP:-0}" != 1 ]] || { echo 'DRILL_KEEP is incompatible with fresh single-run isolation' >&2; exit 2; }
for tool in go python3 etcd jq curl lsof mongosh mongod redis-cli redis-server nats nats-server toxiproxy-server; do
  command -v "$tool" >/dev/null || { echo "$tool not found" >&2; exit 2; }
done
mkdir -p "$DEMO_ROBOT_OUT"
out="$(cd "$DEMO_ROBOT_OUT" && pwd -P)"
work_dir="$out/work"
mkdir "$work_dir"
project="$(printf 'dmr%06x' $(((RANDOM << 9) ^ RANDOM)))"
etcd_pid=""
mirror_started=0
export GOWORK=off GOFLAGS='-p=2 -mod=mod' GOMAXPROCS=0
export ROOST_MIRROR_LOCAL_HOME="$out/mirror" ROOST_MIRROR_LOCAL_OFFSET="$offset"
export DEMO_ROBOT_COUNT="$robots" DEMO_ROBOT_STEP="$step" DEMO_ROBOT_INTERVAL="$interval" DEMO_ROBOT_OUT="$out"
export DEMO_ROBOT_PROJECT="$project" DEMO_ROBOT_ENDPOINT="127.0.0.1:$((7000 + port_shift))" DEMO_ROBOT_OPS="127.0.0.1:$((9100 + port_shift))"
# shellcheck source=../testdata/demo-drill/lib.sh
source "$repo_dir/scripts/testdata/demo-drill/lib.sh"
capture_host() {
  date -u '+%Y-%m-%dT%H:%M:%SZ'
  uptime
  if [[ "$(uname -s)" == Darwin ]]; then sysctl hw.logicalcpu hw.memsize vm.loadavg vm.swapusage; else getconf _NPROCESSORS_ONLN; cat /proc/loadavg /proc/meminfo; fi
}
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e +u
  capture_host >"$out/host-after.txt" 2>&1
  # 先保留服务日志和本轮依赖日志，再只停止自己创建的工程与私有环境。
  [[ -f "$work_dir/etcd/etcd.log" ]] && cp "$work_dir/etcd/etcd.log" "$out/etcd.log"
  drill_cleanup >"$out/cleanup.log" 2>&1
  if (( mirror_started )); then
    python3 "$repo_dir/scripts/testdata/demo-robot/report.py" copy-dependency-logs "$out"
    bash "$repo_dir/scripts/mirror-local.sh" clean >>"$out/cleanup.log" 2>&1 || rc=1
  fi
  python3 "$repo_dir/scripts/testdata/demo-robot/report.py" summarize "$out" "$rc" || rc=1
  echo "game-demo Robot evidence: $out (exit $rc)"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
drill_check_ports
# mirror-local 也逐端口检查所有者；这里在启动任何依赖前检查完整组合与内部重复。
python3 - "$port_shift" "$etcd_port" "$offset" <<'PY'
import socket, sys
shift, etcd, offset = map(int, sys.argv[1:])
ports = [p+shift for p in [7000,7001,7100,8081,8900,9300,9200,*range(9000,9011),*range(9100,9111)]] + [etcd,etcd+1]
ports += [p+offset for p in [27117,27118,27119,14222,14223,14224,16222,16223,16224,18222,18223,18224,16379,16380,18474,24222,24223,24224,26379,*range(17400,17406),*range(27400,27406)]]
assert len(ports) == len(set(ports)), 'port ranges overlap'
for port in ports:
    with socket.socket() as sock:
        try: sock.bind(('127.0.0.1', port))
        except OSError as err: raise SystemExit(f'port {port} unavailable: {err}')
PY
{
  git -C "$repo_dir" rev-parse HEAD
  git -C "$repo_dir" status --short
  go version
  printf 'project=%s count=%s mode=%s ramp_step=%s ramp_interval=%s GOMAXPROCS=%s GOFLAGS=%s mirror_offset=%s port_shift=%s etcd_port=%s\n' "$project" "$robots" "$mode" "$step" "$interval" "$GOMAXPROCS" "$GOFLAGS" "$offset" "$port_shift" "$etcd_port"
} >"$out/environment.txt"
mirror_started=1
bash "$repo_dir/scripts/mirror-local.sh" up >"$out/mirror-up.log" 2>&1
source "$out/mirror/roost-dataengine-it/env.sh"
drill_start_etcd
drill_generate
drill_configure
python3 - <<'PY'
from pathlib import Path
p = Path('configs/service/config.game.yaml')
s = p.read_text()
assert s.count('max_connections_per_ip: 128') == 1, 'expected default per-IP connection limit'
p.write_text(s.replace('max_connections_per_ip: 128', 'max_connections_per_ip: 512'))
PY
mkdir -p internal/demorobotcapacity
cp "$repo_dir/scripts/testdata/demo-robot/capacity_test.go.txt" internal/demorobotcapacity/capacity_test.go
go test -c -o "$out/capacity.test" ./internal/demorobotcapacity
go vet ./internal/demorobotcapacity
tar -czf "$out/project-source.tgz" --exclude=bin --exclude=.dev --exclude=.git .
drill_log 'make dev-run'
make dev-run >"$out/dev-run.log" 2>&1 || { tail -30 "$out/dev-run.log"; drill_fail 'make dev-run'; }
grep -q 'registering game server 1000' "$out/dev-run.log" || drill_fail 'game 1000 was not registered'
sh deploy/dev/run.sh status >"$out/services.txt"
[[ "$(grep -c ': running' "$out/services.txt")" == 11 ]] || drill_fail 'expected 11 services'
capture_host >"$out/host-before.txt" 2>&1
curl -fsS "http://$DEMO_ROBOT_OPS/metrics" >"$out/game-metrics-before.txt"
set +e
"$out/capacity.test" -test.run '^TestDemoRobotCapacity$' -test.v -test.timeout=6m | tee "$out/test.log"
rc=${PIPESTATUS[0]}
set -e
curl -fsS "http://$DEMO_ROBOT_OPS/metrics" >"$out/game-metrics-after.txt" || true
[[ "$(grep -c '^--- PASS: TestDemoRobotCapacity' "$out/test.log" || true)" == 1 ]] || rc=1
grep -q '^--- SKIP' "$out/test.log" && rc=1
exit "$rc"
