#!/usr/bin/env sh
# 重新生成（或核对）C# 地形测试用的 TerrainChunk 与 Building DAO 夹具。全部离线，不连数据库 / NATS / Redis。
#
# 做法与 codegen/scripts/dao-golden-runtime.sh 相同：在 mktemp 临时 module 里用正式 DAO 生成器（codegen/cmd/dao）
# 从 game-demo 的 TerrainChunkDao 与 BuildingDao 定义（demo/db/def/terrain_chunk.go.tmpl、building.go.tmpl，单一来源，
# 不另抄一份）生成 Go DAO、dao_sync.proto 与 dao_sync_apply.cs，replace 到当前检出的 roost-core，再运行本目录的
# sync_fixture_test.go，写出 terrain-chunk-sync.json（Go 生成 DAO 写出的全量 / 增量字节 + gameplay/terrain View 的期望
# 查询）与 building-sync.json（建筑的全量 / 改参数 / 拆除的同步载荷，方案 §27）。
#
# 用法：sh client/dotnet/Roost.Client.Terrain.Tests/Fixtures/regen.sh           核对：生成物与提交的逐字节相同，否则以 1 退出
#       sh client/dotnet/Roost.Client.Terrain.Tests/Fixtures/regen.sh -update   覆盖提交的三个文件
# 何时跑：改 DAO 生成器（sync proto / C# Apply / Go 同步编码）、TerrainChunkDao / BuildingDao 定义或 gameplay/terrain 查询算法之后；
# scripts/test-client-dotnet.sh 的完整模式会以核对模式运行它。
set -eu

here=$(cd "$(dirname "$0")" && pwd)
core=$(cd "$here/../../../.." && pwd)
update=0
if [ "${1:-}" = "-update" ]; then
	update=1
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/terrain-fixture.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/def" "$work/gen"
cp "$core/demo/db/def/terrain_chunk.go.tmpl" "$work/def/terrain_chunk.go"
cp "$core/demo/db/def/building.go.tmpl" "$work/def/building.go"

(cd "$core" && GOWORK=off go run ./codegen/cmd/dao -def "$work/def" -out "$work/gen" -pkg terraindao >/dev/null)
sed '/^\/\/go:build terrainfixture$/d' "$here/sync_fixture_test.go" > "$work/gen/sync_fixture_test.go"

cd "$work/gen"
GOWORK=off go mod init terrainfixture >/dev/null 2>&1
GOWORK=off go mod edit -require "github.com/tjbdwanghaibo/roost-core@v0.0.0" -replace "github.com/tjbdwanghaibo/roost-core=$core"
GOWORK=off GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1
GOWORK=off go vet ./...
TERRAIN_SPEC="$core/client/spec" TERRAIN_FIXTURE_OUT="$work/terrain-chunk-sync.json" BUILDING_FIXTURE_OUT="$work/building-sync.json" \
	GOWORK=off go test -count=1 -run '^(TestWriteTerrainChunkSyncFixture|TestWriteBuildingSyncFixture)$' . >/dev/null

status=0
for f in dao_sync.proto dao_sync_apply.cs terrain-chunk-sync.json building-sync.json; do
	case "$f" in *.json) src="$work/$f" ;; *) src="$work/gen/$f" ;; esac
	if [ "$update" = 1 ]; then
		cp "$src" "$here/$f"
	elif ! cmp -s "$src" "$here/$f"; then
		echo "terrain fixture drift: $f differs from a fresh generation (run regen.sh -update and review the diff)" >&2
		status=1
	fi
done
if [ "$update" = 1 ]; then
	echo "terrain fixture: updated"
elif [ "$status" = 0 ]; then
	echo "terrain fixture: up to date"
fi
exit "$status"
