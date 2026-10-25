#!/usr/bin/env bash
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo"
: "${ROOST_DATAENGINE_IT_ROOT:?source private environment env.sh}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo required}"
mkdir "$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock" || exit 2
monitor_pid=""; probe_pid=""
cleanup() {
 if [[ -n "$probe_pid" ]]; then wait "$probe_pid" || true; fi
 if [[ -n "$monitor_pid" ]]; then wait "$monitor_pid" || true; fi
 rmdir "$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock"
}
trap cleanup EXIT
label="${ROOST_PERF_LABEL:-jetstream-resources-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
out="$PWD/artifacts/perf/saga/$label"
mkdir "$out"
{ date -u; pmset -g batt; sysctl hw.memsize hw.logicalcpu vm.swapusage; shasum -a 256 framework/saga/jetstream_performance_integration_test.go; } > "$out/env.txt"
GOWORK=off go test -tags=integration -c -o "$out/probe.test" ./framework/saga
python3 scripts/perf/macos-resources.py --output "$out" --seconds 115 > "$out/monitor.log" 2>&1 &
monitor_pid=$!
ROOST_JS_PROBE_OUTPUT="$out/probe.json" ROOST_JS_PROBE_RATE=120 ROOST_JS_PROBE_WORKERS=4 ROOST_JS_PROBE_SECONDS=110 GOMAXPROCS=4 "$out/probe.test" -test.run '^TestRealJetStreamProbe$' -test.v -test.timeout=3m > "$out/probe.log" 2>&1 &
probe_pid=$!
sleep 20
date -u '+mongo_start %Y-%m-%dT%H:%M:%SZ' > "$out/phases.txt"
ROOST_MONGO_PROBE_OUTPUT="$out/mongo.json" GOMAXPROCS=4 "$out/probe.test" -test.run '^TestRealMongoCommitProbe$' -test.v -test.timeout=3m > "$out/mongo.log" 2>&1
 date -u '+mongo_end %Y-%m-%dT%H:%M:%SZ' >> "$out/phases.txt"
wait "$probe_pid"
probe_pid=""
wait "$monitor_pid"
monitor_pid=""
tail -n 5 "$out/probe.log"
cat "$out/mongo.json"

python3 scripts/perf/macos-resource-summary.py "$out" --output "$out/summary.json"
