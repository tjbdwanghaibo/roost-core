#!/usr/bin/env bash
# 三副本 JetStream 独立 / Saga 混合对照。编译完才开始计时；全程持有私有环境锁。
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
: "${ROOST_DATAENGINE_IT_ROOT:?source private environment env.sh}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo required}"
case "${ROOST_JS_PROBE_BACKGROUND:-saga}" in saga|mongo) ;; *) echo 'Background must be saga or mongo' >&2; exit 2 ;; esac
label="${ROOST_PERF_LABEL:-jetstream-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
output="$repo/artifacts/perf/saga/$label"
[[ ! -e "$output" ]] || exit 2
lock="$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock"
mkdir "$lock" || { echo 'Private environment already in use' >&2; exit 2; }
saga_pid=""
cleanup() {
 if [[ -n "$saga_pid" ]]; then wait "$saga_pid" || true; fi
 rmdir "$lock"
}
trap cleanup EXIT
mkdir -p "$output"
cd "$repo"
export GOWORK=off GOMAXPROCS=4
{ go version; git rev-parse HEAD; git status --short; shasum -a 256 framework/saga/*performance*test.go; } > "$output/env.txt"
go test -tags=integration -c -o "$output/probe.test" ./framework/saga
probe() {
 local name="$1" rate="$2" workers="$3" seconds="$4"
 ROOST_JS_PROBE_OUTPUT="$output/$name.json" ROOST_JS_PROBE_RATE="$rate" ROOST_JS_PROBE_WORKERS="$workers" ROOST_JS_PROBE_SECONDS="$seconds" \
 "$output/probe.test" -test.run '^TestRealJetStreamProbe$' -test.v -test.timeout=3m > "$output/$name.log" 2>&1
 tail -n 4 "$output/$name.log"
}
if [[ "${ROOST_JS_PROBE_BACKGROUND:-saga}" == "mongo" ]]; then
 ROOST_MONGO_PROBE_OUTPUT="$output/mongo.json" "$output/probe.test" -test.run '^TestRealMongoCommitProbe$' -test.v -test.timeout=3m > "$output/mongo.log" 2>&1 &
 saga_pid=$!
 sleep 10
 probe mongo-mixed 120 4 30
 wait "$saga_pid"
 saga_pid=""
 tail -n 4 "$output/mongo.log"
 probe alone-after 120 4 30
 printf 'Results: %s\n' "$output"
 exit 0
fi
probe alone-before 120 4 30
for workers in 4 32 128; do probe "closed-$workers" 0 "$workers" 15; done
ROOST_SAGA_PERF_OUTPUT="$output/saga-mixed.json" ROOST_SAGA_PERF_MODE=forward ROOST_SAGA_PERF_RATE=30 ROOST_SAGA_PERF_DURATION=1m ROOST_SAGA_PERF_WORKERS=2 ROOST_SAGA_PERF_HOPS=1 ROOST_SAGA_PERF_COORDINATOR_BATCH=3 \
 "$output/probe.test" -test.run '^TestRealSagaPerformance$' -test.v -test.timeout=3m > "$output/saga-mixed.log" 2>&1 &
saga_pid=$!
# 留出建表/消费者启动时间；实际时间戳与混合覆盖范围从日志核对。
sleep 10
probe mixed 120 4 30
wait "$saga_pid"
saga_pid=""
tail -n 5 "$output/saga-mixed.log"
probe alone-after 120 4 30
printf 'Results: %s\n' "$output"
