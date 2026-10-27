#!/usr/bin/env bash
# 原型压测矩阵：私有三节点集群（nats-cluster.sh）+ 同进程协调器 / 参与方 / 发起方，GOMAXPROCS=4（Saga 历史口径）。
# 每档前等 1 分钟 loadavg 低于 SAGAJS_LOAD_GATE（缺省 8，最多等 2 分钟，超时照跑并记下负载），记录 loadavg、swap 与供电。
# 用法：SAGAJS_ROOT=<集群目录> OUT=<结果目录> run-perf.sh [matrix|replay|one <label> <scenario> <parts> <durability> "<levels>"]
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
: "${SAGAJS_ROOT:?}"
: "${OUT:?}"
BASE="${SAGAJS_PORT_BASE:-25622}"
export SAGAJS_URL="nats://127.0.0.1:$BASE,nats://127.0.0.1:$((BASE+1)),nats://127.0.0.1:$((BASE+2))"
export GOWORK=off GOMAXPROCS=4
SECONDS_PER_LEVEL="${SECONDS_PER_LEVEL:-30}"
WARMUP="${WARMUP:-5}"
GATE="${SAGAJS_LOAD_GATE:-8}"
mkdir -p "$OUT"
BIN="$OUT/jslogproto.test"
(cd "$REPO" && go test -c -o "$BIN" ./scripts/perf/saga-jslog-proto/)

gate() {
  local waited=0
  while :; do
    local l1
    l1=$(sysctl -n vm.loadavg | awk '{print $2}')
    if awk -v l="$l1" -v g="$GATE" 'BEGIN{exit !(l<g)}'; then break; fi
    [ "$waited" -ge 120 ] && { echo "$(date '+%F %T') gate timeout, running anyway load={$(sysctl -n vm.loadavg)}" | tee -a "$OUT/runs.log"; break; }
    sleep 15; waited=$((waited + 15))
  done
}

natscpu() { ps -o pid=,time=,rss= -p "$(cat "$SAGAJS_ROOT"/n*.pid | paste -sd, -)" 2>/dev/null | tr '\n' ';'; }

one() {
  local label=$1 scenario=$2 parts=$3 durability=$4 levels=$5
  for lv in $levels; do
    gate
    echo "$(date '+%F %T') start $label c$lv load={$(sysctl -n vm.loadavg)} swap=$(sysctl -n vm.swapusage | awk '{print $6}') power='$(pmset -g batt | head -1 | sed "s/.*'\(.*\)'.*/\1/")' nats_cpu=$(natscpu)" | tee -a "$OUT/runs.log"
    SAGAJS_PERF=1 SAGAJS_PERF_LABEL="$label" SAGAJS_PERF_SCENARIO="$scenario" SAGAJS_PERF_PARTS="$parts" \
      SAGAJS_PERF_DURABILITY="$durability" SAGAJS_PERF_LEVELS="$lv" SAGAJS_PERF_SECONDS="$SECONDS_PER_LEVEL" \
      SAGAJS_PERF_WARMUP="$WARMUP" SAGAJS_PERF_OUT="$OUT" SAGAJS_PERF_WAL_ROOT="$OUT/wal" \
      "$BIN" -test.run '^TestPerfClosedLoop$' -test.v -test.count=1 -test.timeout 0 2>&1 | grep -v ' WARN ' | tee -a "$OUT/$label.log" | grep -E 'TPS|FAIL|panic' || true
    echo "$(date '+%F %T') end $label c$lv nats_cpu=$(natscpu)" | tee -a "$OUT/runs.log"
  done
}

case "${1:-matrix}" in
  matrix)
    # 压缩矩阵（维护者 2026-10-27 要求总时长 ≤ 30 分钟）：每档 30s、稳态 [5s, 30s)，单样本为主。
    L="1 16 64 128"
    one "forward-strict-p4-s1" forward 4 strict "$L"
    one "compensate-strict-p4-s1" compensate 4 strict "$L"
    one "forward-strict-p4-s2" forward 4 strict "$L"
    one "forward-strict-p1" forward 1 strict "16 128"
    one "forward-strict-p16" forward 16 strict "16 128"
    # 16 个分区分到 4 条流（4 个 Raft 组 / leader）
    SAGAJS_PERF_STREAMS=4 one "forward-strict-p16-g4" forward 16 strict "16 128"
    one "forward-pipelined-p4" forward 4 pipelined "16 128"
    # 快照 + 清理开着（每 2000 条事件一次，终态保留 2s）：量快照对吞吐的影响
    SAGAJS_PERF_SNAPSHOT_EVERY=2000 one "forward-strict-p4-snap" forward 4 strict "64"
    ;;
  replay)
    gate
    SAGAJS_PERF=1 SAGAJS_PERF_REPLAY_SIZES="${SIZES:-10000 50000}" "$BIN" -test.run '^TestPerfReplayVsLogSize$' -test.v -test.count=1 -test.timeout 0 2>&1 | tee -a "$OUT/replay.log"
    ;;
  one)
    one "$2" "$3" "$4" "$5" "$6"
    ;;
esac
echo "$(date '+%F %T') all done" | tee -a "$OUT/runs.log"
