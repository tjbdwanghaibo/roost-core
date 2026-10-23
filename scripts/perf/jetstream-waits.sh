#!/usr/bin/env bash
# macOS调用栈和Mongo journal计数诊断；不把带采样的时延当容量验收。
set -euo pipefail
# 测试期间阻止空闲睡眠；只持有临时断言，不改变系统电源配置。
if [[ "${ROOST_WAIT_AWAKE:-}" != "1" ]]; then
 exec caffeinate -i env ROOST_WAIT_AWAKE=1 bash "$0" "$@"
fi
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo"
: "${ROOST_DATAENGINE_IT_ROOT:?source private env.sh}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo required}"
label="${ROOST_PERF_LABEL:-jetstream-waits-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
out="$repo/artifacts/perf/saga/$label"
mkdir "$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock" || exit 2
probe_pid=""; status_pid=""; mongo_pid=""
cleanup() {
 for pid in "$probe_pid" "$status_pid" "$mongo_pid"; do
  if [[ -n "$pid" ]]; then wait "$pid" || true; fi
 done
 rmdir "$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock"
}
trap cleanup EXIT
mkdir "$out"
{ date -u; pmset -g batt; git rev-parse HEAD; shasum -a 256 framework/saga/jetstream_performance_integration_test.go; } > "$out/env.txt"
GOWORK=off go test -tags=integration -c -o "$out/probe.test" ./framework/saga
mongosh --nodb --quiet scripts/perf/mongo-log-sample.js > "$out/mongo-status.jsonl" 2> "$out/mongo-status.err" &
status_pid=$!
ROOST_JS_PROBE_OUTPUT="$out/probe.json" ROOST_JS_PROBE_RATE=120 ROOST_JS_PROBE_WORKERS=4 ROOST_JS_PROBE_SECONDS=110 GOMAXPROCS=4 "$out/probe.test" -test.run '^TestRealJetStreamProbe$' -test.v -test.timeout=3m > "$out/probe.log" 2>&1 &
probe_pid=$!
sample_nodes() {
 local phase="$1" node pid
 for node in nats-1 nats-2 nats-3 mongo-1 mongo-2 mongo-3; do
  if [[ "$node" == nats-* ]]; then pid=$(cat "$ROOST_DATAENGINE_IT_ROOT/$node/nats.pid"); else pid=$(cat "$ROOST_DATAENGINE_IT_ROOT/$node/mongod.pid"); fi
  date -u "+$phase $node %Y-%m-%dT%H:%M:%SZ" >> "$out/sampling-times.txt"
  sample "$pid" 1 5 -file "$out/$phase-$node.sample.txt" >> "$out/sampling.log" 2>&1
 done
}
sleep 3
sample_nodes baseline
sleep 3
date -u '+mongo_start %Y-%m-%dT%H:%M:%SZ' > "$out/phases.txt"
ROOST_MONGO_PROBE_OUTPUT="$out/mongo.json" GOMAXPROCS=4 "$out/probe.test" -test.run '^TestRealMongoCommitProbe$' -test.v -test.timeout=3m > "$out/mongo.log" 2>&1 &
mongo_pid=$!
sleep 10
sample_nodes mixed
wait "$mongo_pid"; mongo_pid=""
date -u '+mongo_end %Y-%m-%dT%H:%M:%SZ' >> "$out/phases.txt"
wait "$probe_pid"; probe_pid=""
wait "$status_pid"; status_pid=""
tail -n 5 "$out/probe.log"
cat "$out/mongo.json"

python3 scripts/perf/jetstream-wait-summary.py "$out" --output "$out/summary.json"
