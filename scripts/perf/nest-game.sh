#!/usr/bin/env bash
# 正式生成 Entity/DAO/Component + Nest ticker/快池，测普通游戏业务及心跳。
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
label="${ROOST_PERF_LABEL:-game-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
output="$repo/artifacts/perf/nest/$label"
[[ ! -e "$output" ]] || { echo "Output exists: $output" >&2; exit 2; }
repeats="${ROOST_PERF_COUNT:-3}"
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
 if [[ "${ROOST_NEST_GAME_PROFILE:-0}" == 1 ]]; then profile=("-test.cpuprofile=$output/sample-$sample.cpu.pprof" "-test.memprofile=$output/sample-$sample.heap.pprof"); fi
 ROOST_NEST_GAME_OUTPUT="$output/sample-$sample.json" "$output/nest-game.test" -test.run '^TestNestGameLoad$' -test.v -test.timeout=70m ${profile[@]+"${profile[@]}"} > "$output/sample-$sample.log" 2>&1 || status=1
 cat "$output/sample-$sample.log"
done
printf 'Results: %s\n' "$output"
exit "$status"
