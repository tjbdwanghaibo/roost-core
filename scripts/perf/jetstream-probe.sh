#!/usr/bin/env bash
# 三副本 JetStream 独立 / Mongo 事务混合对照。编译完才开始计时；全程持有私有环境锁。
# 原来缺省的“Saga 混合背景负载”分支已删除：它跑的 TestRealSagaPerformance 随旧协调器在
# saga 第 4 批（298e4396）删除，-test.run 匹配不到用例时测试二进制以 0 退出，那一段静默空跑。
# saga 正式实现的压测入口是 scripts/perf/saga-js-scan.sh。
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
: "${ROOST_DATAENGINE_IT_ROOT:?source private environment env.sh}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo required}"
case "${ROOST_JS_PROBE_BACKGROUND:-mongo}" in
 mongo) ;;
 saga) echo 'Saga background was removed with the old coordinator; use scripts/perf/saga-js-scan.sh for saga load' >&2; exit 2 ;;
 *) echo 'Background must be mongo' >&2; exit 2 ;;
esac
label="${ROOST_PERF_LABEL:-jetstream-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
output="$repo/artifacts/perf/saga/$label"
[[ ! -e "$output" ]] || exit 2
lock="$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock"
mkdir "$lock" || { echo 'Private environment already in use' >&2; exit 2; }
mongo_pid=""
cleanup() {
 if [[ -n "$mongo_pid" ]]; then wait "$mongo_pid" || true; fi
 rmdir "$lock"
}
trap cleanup EXIT
mkdir -p "$output"
cd "$repo"
export GOWORK=off GOMAXPROCS=4
{ go version; git rev-parse HEAD; git status --short; shasum -a 256 framework/saga/*performance*test.go; } > "$output/env.txt"
go test -tags=integration -c -o "$output/probe.test" ./framework/saga
# 测试二进制对匹配不到用例的 -test.run 也以 0 退出；每段都核对用例确实跑过，避免静默空跑。
ran() { grep -q -- "--- PASS: $1 " "$2" || { echo "$1 did not run; see $2" >&2; exit 1; }; }
probe() {
 local name="$1" rate="$2" workers="$3" seconds="$4"
 ROOST_JS_PROBE_OUTPUT="$output/$name.json" ROOST_JS_PROBE_RATE="$rate" ROOST_JS_PROBE_WORKERS="$workers" ROOST_JS_PROBE_SECONDS="$seconds" \
 "$output/probe.test" -test.run '^TestRealJetStreamProbe$' -test.v -test.timeout=3m > "$output/$name.log" 2>&1
 ran TestRealJetStreamProbe "$output/$name.log"
 tail -n 4 "$output/$name.log"
}
probe alone-before 120 4 30
for workers in 4 32 128; do probe "closed-$workers" 0 "$workers" 15; done
ROOST_MONGO_PROBE_OUTPUT="$output/mongo.json" "$output/probe.test" -test.run '^TestRealMongoCommitProbe$' -test.v -test.timeout=3m > "$output/mongo.log" 2>&1 &
mongo_pid=$!
# 留出 Mongo 探针建库与 worker 启动时间；实际时间戳与混合覆盖范围从日志核对。
sleep 10
probe mongo-mixed 120 4 30
wait "$mongo_pid"
mongo_pid=""
ran TestRealMongoCommitProbe "$output/mongo.log"
tail -n 4 "$output/mongo.log"
probe alone-after 120 4 30
printf 'Results: %s\n' "$output"
