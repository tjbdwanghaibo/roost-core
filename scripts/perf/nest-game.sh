#!/usr/bin/env bash
# 正式生成 Entity/DAO/Component + Nest ticker/快池，测普通游戏业务及心跳。
# 普通业务的入口（夹具 load_test.go 的 gameConfig.Frontend）：
#   缺省               ticker 直接投递 Nest；
#   ROOST_NEST_GAME_GATE=1      TCP → 独立 Gate → NATS → Game Ingress → Nest（需私有 ROOST_DATAENGINE_IT_NATS_URL）；
#   ROOST_NEST_GAME_EMBEDDED=1  TCP → Game 进程内 TCPServer + TCPRuntime → Nest（不需要 NATS，见 embedded_load_test.go）。
# 叠加 Sync：ROOST_NEST_GAME_SYNC=1 场景 Sync（GATE 或 EMBEDDED）；ROOST_NEST_GAME_SELF_SYNC=1 自身视图（只 EMBEDDED，
# 与 SYNC=1 互斥）。嵌入模式可选 ROOST_NEST_GAME_TCP_RESIDENT_BYTES 覆盖出站驻留预算。正式参数见 docs/performance/README.md。
# 心跳的驱动：ROOST_NEST_GAME_HEARTBEAT_DRIVER=ticker（缺省，帧回调逐个投递）或 heartbeat（gameplay/heartbeat，
# 每个 Unit 一个登记，周期 1s / ROOST_NEST_GAME_HZ，必须是心跳档位：1、2、5、10、20Hz）。
set -euo pipefail
# 复用同一正式生成流程，仅验证手写空间组件，不启动负载或外部服务。
test_pattern='^TestNestGameLoad$'
label_prefix=game
case "${1:-}" in
  "") ;;
  --check-spatial) test_pattern='^TestSpatialComponentNestCommitAndRollback$'; label_prefix=spatial-check ;;
  # 心跳驱动接线短测（几百毫秒，D20 第 4 期）：生成的 Unit 由 gameplay/heartbeat 驱动，拍数与 DAO 一致、无跳拍。
  --check-heartbeat) test_pattern='^TestHeartbeatDriverTicksGeneratedUnits$'; label_prefix=heartbeat-check ;;
  *) echo "Usage: $0 [--check-spatial|--check-heartbeat]" >&2; exit 2 ;;
esac
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
label="${ROOST_PERF_LABEL:-$label_prefix-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
output="$repo/artifacts/perf/nest/$label"
[[ ! -e "$output" ]] || { echo "Output exists: $output" >&2; exit 2; }
repeats="${ROOST_PERF_COUNT:-3}"
if [[ "$label_prefix" == *-check ]]; then repeats=1; fi
[[ "$repeats" =~ ^[1-9][0-9]*$ ]] || exit 2
export GOWORK=off GOMAXPROCS="${ROOST_PERF_CPU:-4}"
work="$(mktemp -d "${TMPDIR:-/tmp}/roost-nest-game.XXXXXX")"
trap 'rm -rf "$work"' EXIT
mkdir -p "$output" "$work/def"
cp "$repo/codegen/internal/entity/testdata/nestgame/"*.go "$work/"
cp "$repo/codegen/internal/entity/testdata/nestgame/def/"*.go "$work/def/"
cat > "$work/go.mod" <<MOD
module nestgame

go 1.27.0

require github.com/tjbdwanghaibo/roost-core v0.0.0
replace github.com/tjbdwanghaibo/roost-core => $repo
MOD
cd "$repo"
{ go version; git rev-parse HEAD; git status --short; shasum -a 256 codegen/internal/entity/testdata/nestgame/*.go codegen/internal/entity/testdata/nestgame/def/*.go; env | sort | sed -n '/^ROOST_NEST_GAME_/p'; printf 'GOMAXPROCS=%s\n' "$GOMAXPROCS"; } > "$output/env.txt"
go run ./codegen/cmd/dao -def "$work/def" -out "$work" -pkg nestgame -force > "$output/generate.log" 2>&1
go run ./codegen/cmd/entity -dir "$work" -force >> "$output/generate.log" 2>&1
cd "$work"
race=()
if [[ "${ROOST_NEST_GAME_RACE:-0}" == 1 ]]; then race=(-race); fi
go test -mod=mod ${race[@]+"${race[@]}"} -c -o "$output/nest-game.test" . > "$output/build.log" 2>&1
status=0
for ((sample=1;sample<=repeats;sample++));do
 profile=()
 if [[ "${ROOST_NEST_GAME_PROFILE:-0}" == 1 ]]; then profile=("-test.cpuprofile=$output/sample-$sample.cpu.pprof" "-test.memprofile=$output/sample-$sample.heap.pprof" "-test.mutexprofile=$output/sample-$sample.mutex.pprof" "-test.mutexprofilefraction=10" "-test.blockprofile=$output/sample-$sample.block.pprof" "-test.blockprofilerate=1000000"); fi
 ROOST_NEST_GAME_OUTPUT="$output/sample-$sample.json" "$output/nest-game.test" -test.run "$test_pattern" -test.v -test.timeout=70m ${profile[@]+"${profile[@]}"} > "$output/sample-$sample.log" 2>&1 || status=1
 cat "$output/sample-$sample.log"
 # 长尾采集（ROOST_NEST_GAME_TAIL=metrics|trace，见夹具 tailtrace_test.go）：负载结束后离线分析，不占被测区间。
 if [[ -n "${ROOST_NEST_GAME_TAIL:-}" && -d "$output/sample-$sample.tail" ]]; then
  (cd "$repo/scripts/perf/tailtrace" && GOWORK=off go run . -dir "$output/sample-$sample.tail" -json "$output/sample-$sample.tail/analysis.json") > "$output/sample-$sample.tail/analysis.txt" 2>&1 || status=1
  sed -n '1,40p' "$output/sample-$sample.tail/analysis.txt"
 fi
done
printf 'Results: %s\n' "$output"
exit "$status"
