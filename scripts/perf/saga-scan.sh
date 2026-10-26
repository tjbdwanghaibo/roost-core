#!/usr/bin/env bash
# Saga 闭环并发度扫描：同一组并发度依次跑原生步骤正向 / 全补偿与 Mongo 步骤正向，每档记录成功吞吐、延迟、
# 终态与逐笔阶段拆分。Mongo 可以是本机隔离副本集（local），也可以是维护者提供的云库（cloud / cloud-alt）。
#
#   ROOST_SCAN_TARGET=local|cloud|cloud-alt   ROOST_SCAN_LEVELS="1 4 16 32 64 128"   ROOST_SCAN_DURATION=60s
#   ROOST_SCAN_SCENARIOS="native-forward native-compensate mongo-forward"   ROOST_PERF_LABEL=<唯一名>
#   ROOST_SCAN_MONGO_WORKERS=2（Mongo 步骤每订阅并发）  ROOST_SCAN_PAIRS=256（原生步骤账户对数）
#   ROOST_SCAN_HOPS=1（逐笔 / 逐跳追踪）  ROOST_SCAN_MAX_LOAD=6（开跑前等 1 分钟 loadavg 低于它，最多 10 分钟）
#
# 云库连接串只从 ~/.roost-it/cloud-mongo.env 读（ROOST_SAGA_PERF_MONGO_URI / _ALT），在子 shell 里换成夹具读的
# ROOST_DATAENGINE_IT_MONGO_URI 后立即 unset 原变量；脚本不打印、不落盘任何连接串。NATS 始终用本机隔离环境，
# 每档运行期间持有 remote-acceptance.lock。库名 / 流名带本轮唯一前缀，结束后按前缀核对残留。
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
: "${ROOST_DATAENGINE_IT_ROOT:?source the isolated environment env.sh first}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?isolated NATS required}"
target="${ROOST_SCAN_TARGET:-local}"
levels="${ROOST_SCAN_LEVELS:-1 4 16 32 64 128}"
duration="${ROOST_SCAN_DURATION:-60s}"
scenarios="${ROOST_SCAN_SCENARIOS:-native-forward native-compensate mongo-forward}"
mongo_workers="${ROOST_SCAN_MONGO_WORKERS:-2}"
pairs="${ROOST_SCAN_PAIRS:-256}"
hops="${ROOST_SCAN_HOPS:-1}"
max_load="${ROOST_SCAN_MAX_LOAD:-6}"
label="${ROOST_PERF_LABEL:-sagascan-$target-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || { echo 'invalid label' >&2; exit 2; }
case "$target" in
 local) ;;
 cloud|cloud-alt)
  # 只 source，不回显；换成夹具变量后删掉原变量，避免后续 env 转储带出连接串。
  [[ -f "$HOME/.roost-it/cloud-mongo.env" ]] || { echo 'cloud-mongo.env missing' >&2; exit 2; }
  # shellcheck disable=SC1091
  source "$HOME/.roost-it/cloud-mongo.env"
  if [[ "$target" == cloud ]]; then uri="${ROOST_SAGA_PERF_MONGO_URI:-}"; else uri="${ROOST_SAGA_PERF_MONGO_URI_ALT:-}"; fi
  unset ROOST_SAGA_PERF_MONGO_URI ROOST_SAGA_PERF_MONGO_URI_ALT
  [[ -n "$uri" ]] || { echo "no URI for $target" >&2; exit 2; }
  export ROOST_DATAENGINE_IT_MONGO_URI="$uri"
  # 日志脱敏：驱动错误与 Mongo Mod 的连接日志会带主机 / 用户名。从连接串里取出主机与用户名，只在内存里拼成 sed
  # 表达式，测试输出经它过滤后才落盘；另外整体遮盖 mongodb:// 串与 IPv4。
  authority="${uri#*://}"; authority="${authority%%/*}"; authority="${authority%%\?*}"
  user=""; [[ "$authority" == *@* ]] && { user="${authority%@*}"; user="${user%%:*}"; authority="${authority##*@}"; }
  scrub_expr='s#mongodb(\+srv)?://[^ "]*#<mongo-uri>#g; s#uri="[^"]*"#uri="<redacted>"#g; s#[0-9]{1,3}(\.[0-9]{1,3}){3}(:[0-9]+)?#<host>#g'
  IFS=',' read -r -a host_list <<< "$authority"
  for h in "${host_list[@]}" "$user"; do
   h="${h%%:*}"; [[ ${#h} -ge 3 ]] || continue
   scrub_expr+="; s#$(printf '%s' "$h" | sed -E 's/[][\.^$*+?(){}|#]/\\&/g')#<host>#g"
  done
  unset uri authority user host_list h ;;
 *) echo 'ROOST_SCAN_TARGET must be local, cloud or cloud-alt' >&2; exit 2 ;;
esac
: "${ROOST_DATAENGINE_IT_MONGO_URI:?Mongo required}"
output="$repo/artifacts/perf/saga/$label"
[[ ! -e "$output" ]] || { echo "exists: $output" >&2; exit 2; }
mkdir -p "$output/bin"
rand="$(printf '%04x' $((RANDOM % 65536)))"
export ROOST_PERF_DB_PREFIX="roost_sagaperf_1026_${rand}"
export ROOST_PERF_STREAM_PREFIX="SAGAPERF_1026_$(printf '%s' "$rand" | tr 'a-z' 'A-Z')"
export GOWORK=off GOMAXPROCS="${ROOST_PERF_CPU:-4}" ROOST_DATAENGINE_IT=1
cd "$repo"
{ go version; git rev-parse HEAD; git status --short; echo "target=$target levels=$levels duration=$duration scenarios=$scenarios"
  echo "mongo_workers=$mongo_workers pairs=$pairs hops=$hops gomaxprocs=$GOMAXPROCS db_prefix=$ROOST_PERF_DB_PREFIX stream_prefix=$ROOST_PERF_STREAM_PREFIX"
  sysctl -n hw.ncpu hw.memsize; sysctl vm.swapusage; pmset -g batt | head -1; } > "$output/env.txt" 2>&1
go test -c -tags integration -o "$output/bin/saga.test" ./framework/saga
go test -c -tags integration -o "$output/bin/wsaga.test" ./wiring/saga
go build -o "$output/bin/mongo-ceiling" ./scripts/perf/mongo-ceiling
lock="$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock"
held=0
release() { if [[ "$held" == 1 ]]; then rmdir "$lock" 2>/dev/null || true; held=0; fi; }
trap release EXIT
acquire() {
 local waited=0
 until mkdir "$lock" 2>/dev/null; do
  (( waited++ < 120 )) || { echo 'lock busy for 20 minutes' >&2; exit 2; }
  sleep 10
 done
 held=1
}
# 机器门控：1 分钟 loadavg 低于阈值才开跑，最多等 10 分钟；记录实际值。
gate() {
 local waited=0 load
 while :; do
  load="$(sysctl -n vm.loadavg | awk '{print $2}')"
  awk -v l="$load" -v m="$max_load" 'BEGIN{exit !(l < m)}' && break
  (( waited++ < 60 )) || { echo "load gate timeout (load=$load)" >&2; break; }
  sleep 10
 done
 printf '%s' "$load"
}
snapshot() { "$output/bin/mongo-ceiling" -snapshot > "$1" 2>/dev/null || echo '{"Unavailable":true}' > "$1"; }
# 本机库不需要脱敏；云库时所有落盘的测试输出都先过 scrub。
scrub() { if [[ -n "${scrub_expr:-}" ]]; then sed -E "$scrub_expr"; else cat; fi; }
scrub_files() {
 [[ -n "${scrub_expr:-}" ]] || return 0
 local f
 for f in "$output/$1".*; do
  [[ -f "$f" && "$f" != *.gz ]] || continue
  sed -E "$scrub_expr" "$f" > "$f.scrubbed" && mv "$f.scrubbed" "$f"
 done
}
status=0
for scenario in $scenarios; do
 for level in $levels; do
  name="$scenario-c$level"
  load0="$(gate)"
  acquire
  snapshot "$output/$name.server-before.json"
  start="$(date +%s)"
  case "$scenario" in
   native-forward|native-compensate)
    mode="${scenario#native-}"
    ROOST_STEPS_PERF_OUTPUT="$output/$name.json" ROOST_STEPS_PERF_VARIANT=steps ROOST_STEPS_PERF_MODE="$mode" \
    ROOST_STEPS_PERF_CONCURRENCY="$level" ROOST_STEPS_PERF_DURATION="$duration" ROOST_STEPS_PERF_PAIRS="$pairs" \
    ROOST_STEPS_PERF_HOPS="$([[ "$hops" == 1 ]] && echo 1 || echo 0)" \
     "$output/bin/wsaga.test" -test.run '^TestRealNativeStepsPerformance$' -test.count=1 -test.timeout=20m -test.v 2>&1 | scrub > "$output/$name.log" || status=1 ;;
   mongo-forward|mongo-compensate)
    mode="${scenario#mongo-}"
    ROOST_SAGA_PERF_OUTPUT="$output/$name.json" ROOST_SAGA_PERF_MODE="$mode" ROOST_SAGA_PERF_CONCURRENCY="$level" \
    ROOST_SAGA_PERF_DURATION="$duration" ROOST_SAGA_PERF_WORKERS="$mongo_workers" \
    ROOST_SAGA_PERF_HOPS="$([[ "$hops" == 1 ]] && echo 1 || echo 0)" \
     "$output/bin/saga.test" -test.run '^TestRealSagaPerformance$' -test.count=1 -test.timeout=20m -test.v 2>&1 | scrub > "$output/$name.log" || status=1
    scrub_files "$name.json"
    if [[ "$hops" == 1 && "$mode" == forward && -f "$output/$name.json.timeline.json" ]]; then
     python3 scripts/perf/saga-timeline.py "$output/$name.json" --output "$output/$name.timeline-summary.json" > "$output/$name.timeline-summary.txt" 2>&1 || true
     gzip -f "$output/$name.json.timeline.json"
    fi ;;
   *) echo "unknown scenario $scenario" >&2; exit 2 ;;
  esac
  end="$(date +%s)"
  scrub_files "$name"
  snapshot "$output/$name.server-after.json"
  release
  load1="$(sysctl -n vm.loadavg | awk '{print $2}')"
  printf '%s\tload_before=%s\tload_after=%s\tseconds=%s\n' "$name" "$load0" "$load1" "$((end - start))" | tee -a "$output/runs.tsv"
  grep -E 'concurrency=|completed=|FAIL|^ok|panic' "$output/$name.log" | tail -n 4 || true
 done
done
"$output/bin/mongo-ceiling" -leftover -db-prefix "$ROOST_PERF_DB_PREFIX" -drop 2>&1 | scrub > "$output/leftover-mongo.txt" || true
curl -s "http://127.0.0.1:19222/jsz?accounts=true&streams=true" | python3 -c 'import json,sys,os; p=os.environ["ROOST_PERF_STREAM_PREFIX"]; d=json.load(sys.stdin); n=[s["name"] for a in d.get("account_details",[]) for s in a.get("stream_detail",[]) if s["name"].startswith(p)]; print("leftover_streams=%d" % len(n)); [print(x) for x in n]' > "$output/leftover-streams.txt" 2>&1 || true
cat "$output/leftover-mongo.txt" "$output/leftover-streams.txt"
printf 'Results: %s\n' "$output"
exit "$status"
