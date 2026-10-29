#!/usr/bin/env bash
# saga 正式实现（JetStream 协调器）的压测一轮：设计 docs/maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md §12。
# 取代已删除的 saga-scan.sh（Redis / Mongo 协调器）与原型目录 saga-jslog-proto/ 的 run-perf.sh。
#
# 用法：
#   ROOST_PERF_LABEL=<唯一名> scripts/perf/saga-js-scan.sh          整轮（缺省 15 档，每档 30s，整轮 ≤ 30 分钟）
#   ROOST_PERF_LABEL=<唯一名> scripts/perf/saga-js-scan.sh smoke    冒烟：一档 native-forward c2（pipelined + 异步完成）、3s，只证明脚本端到端能跑
#
# 每档跑一次 framework/saga 的 TestRealSagaJSScanLevel（-tags integration，测试二进制只编译一次）：私有三节点 NATS（R3、文件存储，
# scripts/nats-cluster.sh）、本机私有 mongod 单节点副本集（transactionLifetimeLimitSeconds=20，SAGA.md 的部署要求）、
# GOMAXPROCS=4、闭环并发、稳态窗口 [5s, 30s)。参与方是正式路径（Nest + nestwal WAL + DataEngine 投影 8 通道 + 实体内回执 +
# SubscribeNativeStep / SubscribeMongoStep），协调器是正式 Engine（P=16、G=4、R3）；冷副本关。每档前等 loadavg < 8（最多 60s）。
# 正确性是硬门槛：一档的非期望终态、余额不符、发布结果未知、积压 30s 内不归零任何一项不为 0，这一档的测试失败（结果仍写盘）。
#
# 原生参与方的提交方式（维护者 2026-10-29 决定，SAGA.md「原生步骤的提交方式」）：
#   - 不带后缀的原生档（native-forward、native-compensate、admission、snapshot）按生产推荐跑：步骤 handler durability=pipelined
#     + Nest pipelined 异步完成（测试参与方的缺省，脚本显式清掉 ROOST_SAGA_PT_STRICT），Nest short 池 worker 数 = 生产缺省
#     GOMAXPROCS = 4，不放大。native-pipelined 是 pipelined 不开异步完成的诊断档，不在缺省计划里。
#   - 后缀 @strict<N> 是对照档：步骤 handler durability=strict（ROOST_SAGA_PT_STRICT=1）、short 池 N 个 worker（缺省计划带
#     @strict4 与 @strict64：strict 在生产缺省 short 池上等 fsync 会占满 worker，64 是原型压测放大池的口径）。
#   - mongo-forward / replay 没有 Nest 参与方，后缀不适用。
# 缺省计划 15 档，每档 30s 加启动与排空，整轮 ≤ 30 分钟。summary.tsv 带 durability / 异步完成 / short 池 worker 数与忙占比、
# 每个作业平均等 worker 的时长，对照 strict 档能直接看出差别；还带余额核对分项（读失败 / 不符）、实际加压时长与加压结束时的积压。
# 每档另写 <档>.series.tsv：参与方 WAL / 投影器 / 积压 / 准入信号的逐秒累计值（JSON 的 series 字段）。
#
# 环境：
#   ROOST_PERF_LABEL           结果目录名（artifacts/perf/saga-js/<label>/，不随仓）；必填
#   ROOST_SCAN_PLAN            覆盖档位，空格分隔的 <场景>:<并发>[@strict<N>]（replay 是 replay:<日志条数>）
#   ROOST_SCAN_SECONDS         每档秒数（缺省 30；smoke 为 3）
#   ROOST_SCAN_NATS_PORT_BASE  私有 NATS 第一个客户端端口（缺省 29622；路由 +1000、监控 +3000，各连续三个）
#   ROOST_SCAN_MONGO_PORT      私有 mongod 端口（缺省 29817）
#   ROOST_SCAN_MONGO_URI       给了就不起私有 mongod，用它（例如云 Mongo 一轮，设计 §12 不计入 30 分钟）；不打印、不写盘
#   ROOST_SCAN_LOAD_MAX / ROOST_SCAN_LOAD_WAIT   负载门（缺省 8 / 60 秒）
#   ROOST_SCAN_DATA            私有 NATS / mongod 数据目录（缺省 ${TMPDIR:-/tmp}/roost-saga-js-scan-<label>，结束删除）
#   ROOST_SAGA_SCAN_PARTITIONS / ROOST_SAGA_SCAN_PROJECT_DELAY_US / ROOST_SAGA_SCAN_DRAIN_SECONDS
#                              透传给测试（P 缺省 16、准入档投影每条记录延迟缺省 10ms、停止加压后等积压归零的上限缺省 30s——
#                              云 Mongo 一轮投影慢时放宽，Mongo 余额才能核对），见 TestRealSagaJSScanLevel；
#                              short 池 worker 数与提交方式（ROOST_SAGA_SCAN_NEST_WORKERS / ROOST_SAGA_PT_STRICT）由每档的
#                              后缀决定（见上），不透传
#
# 不碰共享环境：端口与目录私有，开跑前检查端口空闲；结束（含失败）停掉私有实例、删数据目录；结果目录保留。
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LABEL="${ROOST_PERF_LABEL:?ROOST_PERF_LABEL is required (results go to artifacts/perf/saga-js/<label>/)}"
MODE="${1:-full}"
SECONDS_PER_LEVEL="${ROOST_SCAN_SECONDS:-30}"
WARMUP=5
PLAN="${ROOST_SCAN_PLAN:-}"
if [ "$MODE" = smoke ]; then
  SECONDS_PER_LEVEL="${ROOST_SCAN_SECONDS:-3}"
  WARMUP=1
  PLAN="${PLAN:-native-forward:2}"
elif [ "$MODE" != full ]; then
  echo "usage: $0 [full|smoke]" >&2
  exit 2
fi
# 设计 §12 的档位：原生正向主档、全补偿（都按生产推荐 pipelined + 异步完成、short 池生产缺省）、strict 对照（short 4 / 64）、
# Mongo 步骤、准入、快照、重放、原生正向第二样本。
PLAN="${PLAN:-native-forward:1 native-forward:16 native-forward:64 native-forward:128 native-compensate:16 native-compensate:128 native-forward:128@strict4 native-forward:128@strict64 mongo-forward:16 mongo-forward:128 admission:128 snapshot:64 replay:10000 replay:50000 native-forward:128}"
NATS_BASE="${ROOST_SCAN_NATS_PORT_BASE:-29622}"
MONGO_PORT="${ROOST_SCAN_MONGO_PORT:-29817}"
LOAD_MAX="${ROOST_SCAN_LOAD_MAX:-8}"
LOAD_WAIT="${ROOST_SCAN_LOAD_WAIT:-60}"
OUT="$REPO/artifacts/perf/saga-js/$LABEL"
DATA="${ROOST_SCAN_DATA:-${TMPDIR:-/tmp}/roost-saga-js-scan-$LABEL}"
DATA="${DATA%/}"
MARKER=".roost-saga-js-scan"

if [ -e "$OUT" ]; then
  echo "result directory $OUT already exists; pick another ROOST_PERF_LABEL" >&2
  exit 1
fi
port_busy() { lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; }
ports=()
for i in 0 1 2; do ports+=($((NATS_BASE + i)) $((NATS_BASE + 1000 + i)) $((NATS_BASE + 3000 + i))); done
[ -z "${ROOST_SCAN_MONGO_URI:-}" ] && ports+=("$MONGO_PORT")
for p in "${ports[@]}"; do
  if port_busy "$p"; then
    echo "port $p is in use; set ROOST_SCAN_NATS_PORT_BASE / ROOST_SCAN_MONGO_PORT to free ones" >&2
    exit 1
  fi
done

mkdir -p "$OUT" "$DATA"
touch "$DATA/$MARKER"
MONGO_PID=""
cleanup() {
  local status=$?
  if [ -n "$MONGO_PID" ]; then
    kill "$MONGO_PID" 2>/dev/null || true
    for _ in $(seq 1 100); do kill -0 "$MONGO_PID" 2>/dev/null || break; sleep 0.1; done
    kill -9 "$MONGO_PID" 2>/dev/null || true
  fi
  ROOST_NATS_CLUSTER_ROOT="$DATA/nats" ROOST_NATS_CLUSTER_PORT_BASE="$NATS_BASE" "$REPO/scripts/nats-cluster.sh" clean >/dev/null 2>&1 || true
  if [ -f "$DATA/$MARKER" ]; then rm -rf "$DATA"; fi
  echo "saga-js-scan: results in $OUT (exit $status)" >&2
}
trap cleanup EXIT

export GOWORK=off GOMAXPROCS=4

{
  echo "label: $LABEL"
  echo "mode: $MODE"
  echo "plan: $PLAN"
  echo "seconds_per_level: $SECONDS_PER_LEVEL warmup: $WARMUP"
  echo "commit: $(git -C "$REPO" rev-parse HEAD)"
  echo "dirty: $(git -C "$REPO" status --porcelain | wc -l | tr -d ' ') paths"
  echo "go: $(go version)"
  echo "nats-server: $(nats-server --version 2>&1)"
  if [ -z "${ROOST_SCAN_MONGO_URI:-}" ]; then echo "mongod: $(mongod --version | head -1) (private, transactionLifetimeLimitSeconds=20)"; else echo "mongod: ROOST_SCAN_MONGO_URI (not recorded)"; fi
  echo "host: $(uname -a)"
  echo "swap: $(sysctl -n vm.swapusage 2>/dev/null || echo n/a)"
  echo "loadavg_start: $(sysctl -n vm.loadavg 2>/dev/null || uptime)"
} > "$OUT/env.txt"

ROOST_NATS_CLUSTER_ROOT="$DATA/nats" ROOST_NATS_CLUSTER_PORT_BASE="$NATS_BASE" "$REPO/scripts/nats-cluster.sh" start
eval "$(ROOST_NATS_CLUSTER_ROOT="$DATA/nats" ROOST_NATS_CLUSTER_PORT_BASE="$NATS_BASE" "$REPO/scripts/nats-cluster.sh" env)"

if [ -z "${ROOST_SCAN_MONGO_URI:-}" ]; then
  mkdir -p "$DATA/mongo"
  mongod --replSet sjsscan --port "$MONGO_PORT" --bind_ip 127.0.0.1 --dbpath "$DATA/mongo" --logpath "$DATA/mongod.log" \
    --setParameter transactionLifetimeLimitSeconds=20 >/dev/null 2>&1 &
  MONGO_PID=$!
  for _ in $(seq 1 100); do
    mongosh --quiet --port "$MONGO_PORT" --eval 'db.runCommand({ping: 1}).ok' >/dev/null 2>&1 && break
    sleep 0.2
  done
  mongosh --quiet --port "$MONGO_PORT" --eval "rs.initiate({_id: 'sjsscan', members: [{_id: 0, host: '127.0.0.1:$MONGO_PORT'}]}).ok" >/dev/null
  for _ in $(seq 1 100); do
    [ "$(mongosh --quiet --port "$MONGO_PORT" --eval 'db.hello().isWritablePrimary' 2>/dev/null)" = true ] && break
    sleep 0.2
  done
  export ROOST_DATAENGINE_IT_MONGO_URI="mongodb://127.0.0.1:$MONGO_PORT/?replicaSet=sjsscan"
else
  export ROOST_DATAENGINE_IT_MONGO_URI="$ROOST_SCAN_MONGO_URI"
fi

(cd "$REPO" && go test -c -tags integration -o "$DATA/saga.test" ./framework/saga)

load1() {
  if [ -r /proc/loadavg ]; then
    cut -d' ' -f1 /proc/loadavg
  else
    sysctl -n vm.loadavg | awk '{print $2}' # "{ 1.23 1.10 0.98 }"
  fi
}
gate() {
  local waited=0 load
  while :; do
    load="$(load1)"
    awk -v l="$load" -v m="$LOAD_MAX" 'BEGIN { exit !(l < m) }' && break
    [ "$waited" -ge "$LOAD_WAIT" ] && break
    sleep 5
    waited=$((waited + 5))
  done
  echo "$load $waited"
}

printf 'level\tscenario\tparam\tload1\tgate_wait_s\tseconds\texit\n' > "$OUT/runs.tsv"
failed=0
n=0
for entry in $PLAN; do
  n=$((n + 1))
  level="${entry%@*}"
  variant=""
  [ "$level" = "$entry" ] || variant="${entry##*@}"
  scenario="${level%%:*}"
  param="${level#*:}"
  name="$(printf '%02d' "$n")-$scenario-$param${variant:+-$variant}"
  env_args=(ROOST_SAGA_SCAN_OUTPUT="$OUT/$name.json" ROOST_SAGA_SCAN_SCENARIO="$scenario" ROOST_SAGA_SCAN_SECONDS="$SECONDS_PER_LEVEL" ROOST_SAGA_SCAN_WARMUP="$WARMUP")
  case "$scenario:$variant" in
    mongo-forward:|replay:)
      env_args+=(ROOST_SAGA_PT_STRICT=) ;;
    mongo-forward:*|replay:*)
      echo "level $entry: @$variant does not apply to $scenario (no Nest participant)" >&2; exit 2 ;;
    *:)
      # 生产推荐：pipelined + 异步完成（参与方缺省），short 池 = GOMAXPROCS（Nest 缺省）。
      env_args+=(ROOST_SAGA_PT_STRICT= ROOST_SAGA_SCAN_NEST_WORKERS="$GOMAXPROCS") ;;
    *:strict[0-9]*)
      workers="${variant#strict}"
      case "$workers" in *[!0-9]*) echo "level $entry: want @strict<N>" >&2; exit 2 ;; esac
      env_args+=(ROOST_SAGA_PT_STRICT=1 ROOST_SAGA_SCAN_NEST_WORKERS="$workers") ;;
    *)
      echo "level $entry: unknown variant @$variant (want @strict<N>)" >&2; exit 2 ;;
  esac
  read -r load waited < <(gate)
  began=$(date +%s)
  if [ "$scenario" = replay ]; then
    env_args+=(ROOST_SAGA_SCAN_REPLAY_MESSAGES="$param" ROOST_SAGA_SCAN_CONCURRENCY=64)
  else
    env_args+=(ROOST_SAGA_SCAN_CONCURRENCY="$param")
  fi
  status=0
  (cd "$REPO/framework/saga" && env "${env_args[@]}" "$DATA/saga.test" -test.run '^TestRealSagaJSScanLevel$' -test.v -test.count=1 -test.timeout 15m) \
    > "$OUT/$name.log" 2>&1 || status=$?
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$n" "$scenario" "$param" "$load" "$waited" "$(( $(date +%s) - began ))" "$status" >> "$OUT/runs.tsv"
  grep -E 'saga/s|--- (PASS|FAIL)' "$OUT/$name.log" | sed "s/^/[$name] /" || true
  [ "$status" = 0 ] || failed=$((failed + 1))
done

python3 -I - "$OUT" > "$OUT/summary.tsv" <<'PY'
import json, pathlib, sys
out = pathlib.Path(sys.argv[1])
print("level\tscenario\tconcurrency\tdurability\tasync\tshort_workers\tper_second\tp50_ms\tp99_ms\tshort_busy\tworker_wait_us\tsagas\tunexpected\tbalance_mismatch\tbalance_read_errors\tmongo_checked\tunobserved\tpublish_unknown\tbacklog_peak\tload_s\tbacklog_at_load_end\tmax_drop_in_load\tdrain_s\treplay_messages\treplay_start_s")
cols = ("t", "phase", "terminals", "wal_admitted", "wal_appended", "wal_durable_lsn", "wal_syncs", "wal_checkpoints", "proj_committed", "proj_projected", "backlog", "admission_open", "admission_backlog")
for path in sorted(out.glob("[0-9][0-9]-*.json")):
    r = json.loads(path.read_text())
    replay = r.get("replay") or {}
    print("\t".join(str(x) for x in (path.stem, r["scenario"], r["concurrency"], r.get("durability", "-"), "on" if r.get("pipelined_async") else "off",
        r.get("nest_workers", "-"), round(r["per_second"], 1), round(r["p50_ms"], 1), round(r["p99_ms"], 1),
        round(r.get("short_busy_fraction", 0), 2), round(r.get("short_worker_wait_mean_us", 0)), r["sagas"], r["unexpected_terminals"], r["balance_mismatches"],
        r.get("balance_read_errors", 0), r.get("mongo_checked", "-"), r.get("unobserved_sagas", 0), r["publish_unknown"], r["backlog_peak_records"],
        round(r.get("load_seconds", 0), 1), r.get("backlog_at_load_end", 0), r.get("backlog_max_drop_in_load", 0), round(r["drain_seconds"], 1),
        replay.get("messages", ""), round(replay.get("start_seconds", 0), 3) if replay else "")))
    if r.get("series"):
        with open(path.with_suffix(".series.tsv"), "w") as f:
            f.write("\t".join(cols) + "\n")
            for row in r["series"]:
                f.write("\t".join(str(round(row.get(c, 0), 2) if c == "t" else row.get(c, "")) for c in cols) + "\n")
PY
column -t -s $'\t' "$OUT/summary.tsv" || cat "$OUT/summary.tsv"
echo "loadavg_end: $(sysctl -n vm.loadavg 2>/dev/null || uptime)" >> "$OUT/env.txt"
if [ "$failed" != 0 ]; then
  echo "saga-js-scan: $failed level(s) failed; see $OUT/*.log" >&2
  exit 1
fi
