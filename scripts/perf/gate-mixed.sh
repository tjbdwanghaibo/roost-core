#!/usr/bin/env bash
# One-account hotspot diagnostic with real account, Gate, WAL/Mongo and heartbeat.
# This script does not start or stop services and does not claim MMO capacity.
# Dropped counts unsent client pacing slots. Independent rejection, timeout and
# heartbeat signals are retained separately; they must not be summed as packet loss.
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica set required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?isolated Redis required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?isolated NATS required}"
repo="$(cd "$(dirname "$0")/../.." && pwd)"
label="${ROOST_PERF_LABEL:-gate-mixed-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
output="$repo/artifacts/perf/gate/$label"
[[ ! -e "$output" ]] || { echo "Output exists: $output" >&2; exit 2; }
mkdir -p "$output"
cd "$repo"
export GOWORK=off GOMAXPROCS="${ROOST_PERF_CPU:-0}"
export ROOST_GATE_REAL=1 ROOST_GATE_LOAD_SECONDS="${ROOST_GATE_LOAD_SECONDS:-30}"
export ROOST_GATE_LOAD_RATE="${ROOST_GATE_LOAD_RATE:-100}"
export ROOST_GATE_LOAD_REPORT="$output/result.json"
# Do not accidentally run a managed/native functional client instead of this diagnostic.
unset ROOST_GATE_DOTNET ROOST_GATE_CPP
{ go version; git rev-parse HEAD; git status --short; printf 'GOMAXPROCS=%s duration=%ss target_rate=%s/s\n' "$GOMAXPROCS" "$ROOST_GATE_LOAD_SECONDS" "$ROOST_GATE_LOAD_RATE"; shasum -a 256 codegen/internal/roost/testdata/gate/*.go.txt robot/sync_report.go; } > "$output/environment.txt"
go test -count=1 -v -timeout 8m -run '^TestGateGeneratedBusinessConsumer$' ./codegen/internal/roost > "$output/test.log" 2>&1
if grep -q -- '--- SKIP:' "$output/test.log"; then echo "Unexpected skipped load" >&2; exit 1; fi
grep -q 'PASS Gate mixed load:' "$output/test.log"
python3 - "$output/result.json" <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
assert r['verified']
assert r['Dropped'] == r['drop_reasons']['client_pacing']
assert r['Scheduled'] == r['Completed'] - r['final_verification_requests'] + r['Dropped']
for event in r['Sync']['events']:
    assert event['received'] > 0 and not (event['pending'] or event['timed_out'] or event['canceled'])
    print('%s: %d applied, p99=%.3fms, max=%.3fms' % (event['kind'], event['received'], event['p99_ms'], event['max_ms']))
print('Scheduled=%d dropped=%d completed=%d (includes one final drain commit)' % (r['Scheduled'], r['Dropped'], r['Completed']))
print('Drop reasons: %s; independent signals: %s' % (r['drop_reasons'], r['signals']))
print('Functional consistency verified; capacity and latency acceptance must be evaluated separately.')
PY
printf 'Evidence: %s\n' "$output"
