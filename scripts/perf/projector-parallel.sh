#!/usr/bin/env bash
# 投影器按实体分片并行投影的吞吐对照：同一组 WAL 形状在 ProjectionWorkers 各档位上排空，记录条/s 与核对结果。
# 夹具：framework/dataengine/engine TestRealMongoProjectorParallelThroughput（-tags integration，ROOST_PROJECTOR_PERF=1）。
#
#   ROOST_PROJPAR_TARGET=local|cloud   ROOST_PROJPAR_SHAPES="plain hot receipt special"   ROOST_PROJPAR_WORKERS=1,2,4,8,16
#   ROOST_PROJPAR_RECORDS=20000  ROOST_PROJPAR_ENTITIES=4096  ROOST_PROJPAR_ROUNDS=2（形状 × 档位整组重复次数，交替跑）
#   ROOST_PROJPAR_MAX_LOAD=8（每组前等 1 分钟 loadavg 低于它，最多 5 分钟，超时照跑并记下）  ROOST_PERF_LABEL=<唯一名>
#
# local 用 ~/.roost-it 隔离副本集（先 source env.sh），运行期间持有 remote-acceptance.lock；cloud 只从 ~/.roost-it/cloud-mongo.env
# 读 ROOST_SAGA_PERF_MONGO_URI，换成夹具变量后立即 unset 原变量，不打印、不落盘；全部输出先脱敏（连接串、主机、用户名、IPv4）再写文件。
# 库名前缀 roost_sagaperf_projpar_<label 摘要>，每档结束删库，最后用 mongo-ceiling -leftover 核对残留。
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
target="${ROOST_PROJPAR_TARGET:-local}"
shapes="${ROOST_PROJPAR_SHAPES:-plain hot receipt special}"
workers="${ROOST_PROJPAR_WORKERS:-1,2,4,8,16}"
rounds="${ROOST_PROJPAR_ROUNDS:-2}"
max_load="${ROOST_PROJPAR_MAX_LOAD:-8}"
label="${ROOST_PERF_LABEL:-projpar-$target-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || { echo 'invalid label' >&2; exit 2; }
scrub_expr='s#mongodb(\+srv)?://[^ "]*#<mongo-uri>#g; s#[0-9]{1,3}(\.[0-9]{1,3}){3}(:[0-9]+)?#<host>#g'
case "$target" in
 local)
  : "${ROOST_DATAENGINE_IT_ROOT:?source ~/.roost-it/roost-dataengine-it/env.sh first}" ;;
 cloud)
  [[ -f "$HOME/.roost-it/cloud-mongo.env" ]] || { echo 'cloud-mongo.env missing' >&2; exit 2; }
  # shellcheck disable=SC1091
  source "$HOME/.roost-it/cloud-mongo.env"
  uri="${ROOST_SAGA_PERF_MONGO_URI:-}"
  unset ROOST_SAGA_PERF_MONGO_URI ROOST_SAGA_PERF_MONGO_URI_ALT
  [[ -n "$uri" ]] || { echo 'no cloud URI' >&2; exit 2; }
  export ROOST_DATAENGINE_IT_MONGO_URI="$uri"
  authority="${uri#*://}"; authority="${authority%%/*}"; authority="${authority%%\?*}"
  user=""; [[ "$authority" == *@* ]] && { user="${authority%@*}"; user="${user%%:*}"; authority="${authority##*@}"; }
  IFS=',' read -r -a host_list <<< "$authority"
  for h in "${host_list[@]}" "$user"; do
   h="${h%%:*}"; [[ ${#h} -ge 3 ]] || continue
   scrub_expr+="; s#$(printf '%s' "$h" | sed -E 's/[][\.^$*+?(){}|#]/\\&/g')#<host>#g"
  done
  unset uri authority user host_list h ;;
 *) echo 'ROOST_PROJPAR_TARGET must be local or cloud' >&2; exit 2 ;;
esac
: "${ROOST_DATAENGINE_IT_MONGO_URI:?Mongo required}"
scrub() { sed -E "$scrub_expr"; }
output="$repo/artifacts/perf/projector/$label"
[[ ! -e "$output" ]] || { echo "exists: $output" >&2; exit 2; }
mkdir -p "$output"
prefix="roost_sagaperf_projpar_$(printf '%s' "$label" | shasum | cut -c1-10)"
cd "$repo"
export GOWORK=off GOMAXPROCS="${ROOST_PERF_CPU:-4}"
bin="$output/engine.test"
go test -c -tags integration -o "$bin" ./framework/dataengine/engine/
go build -o "$output/mongo-ceiling" ./scripts/perf/mongo-ceiling
held=0
lock="${ROOST_DATAENGINE_IT_ROOT:-/nonexistent}/remote-acceptance.lock"
release() { if [[ "$held" == 1 ]]; then rmdir "$lock" 2>/dev/null || true; held=0; fi; }
trap release EXIT
if [[ "$target" == local ]]; then
 waited=0
 until mkdir "$lock" 2>/dev/null; do (( waited++ < 120 )) || { echo 'lock busy for 20 minutes' >&2; exit 2; }; sleep 10; done
 held=1
fi
{
 go version; git rev-parse HEAD; git status --short | grep -v '^??' || true
 printf 'target=%s shapes=%s workers=%s rounds=%s records=%s entities=%s GOMAXPROCS=%s prefix=%s\n' "$target" "$shapes" "$workers" "$rounds" \
  "${ROOST_PROJPAR_RECORDS:-20000}" "${ROOST_PROJPAR_ENTITIES:-4096}" "$GOMAXPROCS" "$prefix"
 sysctl -n machdep.cpu.brand_string hw.ncpu vm.swapusage
} > "$output/env.txt"
# 网络基线：ping 往返与单文档天花板（只用本轮前缀的库）。
"$output/mongo-ceiling" -db-prefix "${prefix}_ceiling" -concurrency 1,16 -duration 5s -pings 100 -output "$output/ceiling.json" 2>&1 | scrub > "$output/ceiling.log" || true
gate() {
 local waited=0
 while :; do
  local l1; l1=$(sysctl -n vm.loadavg | awk '{print $2}')
  awk -v l="$l1" -v g="$max_load" 'BEGIN{exit !(l<g)}' && break
  (( waited >= 300 )) && { echo "$(date '+%F %T') gate timeout load={$(sysctl -n vm.loadavg)}" >> "$output/runs.log"; break; }
  sleep 15; waited=$((waited + 15))
 done
}
for ((round = 1; round <= rounds; round++)); do
 for shape in $shapes; do
  gate
  echo "$(date '+%F %T') start round=$round shape=$shape load={$(sysctl -n vm.loadavg)} swap=$(sysctl -n vm.swapusage | awk '{print $6}')" >> "$output/runs.log"
  ROOST_PROJECTOR_PERF=1 ROOST_PROJECTOR_PERF_SHAPE="$shape" ROOST_PROJECTOR_PERF_WORKERS="$workers" \
   ROOST_PROJECTOR_PERF_RECORDS="${ROOST_PROJPAR_RECORDS:-20000}" ROOST_PROJECTOR_PERF_ENTITIES="${ROOST_PROJPAR_ENTITIES:-4096}" \
   ROOST_PROJECTOR_PERF_DB_PREFIX="${prefix}_r$round" ROOST_PROJECTOR_PERF_OUT="$output/results-raw.jsonl" \
   "$bin" -test.run '^TestRealMongoProjectorParallelThroughput$' -test.v -test.count=1 -test.timeout 0 2>&1 | scrub >> "$output/test.log" || true
  echo "$(date '+%F %T') end round=$round shape=$shape load={$(sysctl -n vm.loadavg)}" >> "$output/runs.log"
 done
done
scrub < "$output/results-raw.jsonl" > "$output/results.jsonl" && rm -f "$output/results-raw.jsonl"
"$output/mongo-ceiling" -leftover -drop -db-prefix "$prefix" 2>&1 | scrub > "$output/leftover.log" || true
rm -f "$bin" "$output/mongo-ceiling"
release
cat "$output/results.jsonl"
tail -1 "$output/leftover.log"
printf 'Results: %s\n' "$output"
