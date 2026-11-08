#!/usr/bin/env sh
# 地形块 DAO 形状实验（方案稿 D8）复跑入口。全部离线，不连数据库 / NATS / Redis。
#
# 做法与 codegen/scripts/dao-golden-runtime.sh 相同：在 mktemp 临时 module 里用正式 DAO 生成器
# （codegen/cmd/dao）从 def/ 生成两种 TerrainChunk DAO，replace 到当前检出的 roost-core，
# 再把本目录的测试 / 基准复制进去运行。结果写回本目录：
#   single-edit.md、fallback-threshold.md、multi-edit.md、document-sizes.md  测量表（测试生成）
#   test.log  测试输出；bench.txt  基准原始输出；bench-benchstat.txt  中位数汇总；env.txt  机器与 Go 版本
#
# 用法：sh docs/maintenance/evidence/terrain-sync-20261108/run.sh
#   TERRAIN_BENCH_COUNT=5（缺省 5）控制基准重复次数；TERRAIN_SKIP_BENCH=1 只跑测量。
set -eu

here=$(cd "$(dirname "$0")" && pwd)
core=$(cd "$here/../../../.." && pwd)
count=${TERRAIN_BENCH_COUNT:-5}

work=$(mktemp -d "${TMPDIR:-/tmp}/terrain-sync.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/def" "$work/gen"
for f in "$here"/def/*.go; do
	sed '/^\/\/go:build terrainexp$/d' "$f" > "$work/def/$(basename "$f")"
done

# 正式生成器，不改生成器与框架代码。
(cd "$core" && GOWORK=off go run ./codegen/cmd/dao -def "$work/def" -out "$work/gen" -pkg terrain >/dev/null)

for f in "$here"/*_test.go; do
	sed '/^\/\/go:build terrainexp$/d' "$f" > "$work/gen/$(basename "$f")"
done

cd "$work/gen"
GOWORK=off go mod init terrainsync >/dev/null 2>&1
GOWORK=off go mod edit -require "github.com/tjbdwanghaibo/roost-core@v0.0.0" -replace "github.com/tjbdwanghaibo/roost-core=$core"
GOWORK=off GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1
GOWORK=off go vet ./...

{
	echo "date: $(date -u +%Y-%m-%dT%H:%MZ)"
	echo "go: $(go version)"
	echo "os: $(uname -srm)"
	if command -v sysctl >/dev/null 2>&1; then
		echo "cpu: $(sysctl -n machdep.cpu.brand_string 2>/dev/null || true)"
		echo "ncpu: $(sysctl -n hw.ncpu 2>/dev/null || true)"
	fi
	echo "core: $(cd "$core" && git rev-parse --short HEAD 2>/dev/null || echo unknown)"
} > "$here/env.txt"

TERRAIN_OUT="$here" GOWORK=off go test -count=1 -run '^TestTerrain' -v . > "$here/test.log" 2>&1 || {
	cat "$here/test.log"
	exit 1
}
echo "measurements: ok ($(grep -c '^--- PASS' "$here/test.log") passed)"

if [ -z "${TERRAIN_SKIP_BENCH:-}" ]; then
	GOWORK=off go test -run '^$' -bench '^BenchmarkTerrain' -benchmem -count="$count" -timeout 0 . > "$here/bench.txt"
	echo "bench: $here/bench.txt"
	# 有 benchstat 时另存中位数汇总（golang.org/x/perf/cmd/benchstat）。
	if command -v benchstat >/dev/null 2>&1; then
		benchstat "$here/bench.txt" > "$here/bench-benchstat.txt"
	fi
fi
