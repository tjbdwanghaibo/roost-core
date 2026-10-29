#!/usr/bin/env sh
# 生成链路的 await / 业务时间守卫：awaitflow 消费工程必须与当前生成器一致，并在 race 下通过。
#
# codegen/internal/entity/testdata/awaitflow 是 entity + nest 两个正式生成器的消费工程：
# 生成的 Sync_ClaimReward 在真实 EntityManager / ManagerAccess 上 await 并在 long 池恢复
# （TestGeneratedPlayerAwaitsAndResumesOnLongPool，NEST-AWAIT），生成的分发包装在执行时填业务
# 时间（TestGeneratedDispatchFillsTheBusinessTimeAtExecution，impl/10-time），生成的 sender
# 没有客户端时拒绝调用（sender 包的 TestPlayerSenderRefusesCallsWithoutAClient）。
# testdata 不在 `go test ./...` 里，2026-10-29 测试审计前没有任何自动入口，只靠人照文档手跑。
#
# 与其它 *-runtime.sh 不同，awaitflow 在本模块里，编译的是当前检出的 roost-core，而不是
# core-pin.sh 的已发布最低版本：它守的是“生成器 + 当前框架”这一对，不是生成器的最低兼容承诺。
#
# 步骤：先把目录现状另存一份，用当前生成器原地重新生成（与 go generate 一样，漂移时留下新生成物
# 供提交），和另存的现状逐文件比对，不一致即失败；再跑 race 测试与 glsvet。
# 限制：只比对生成器会写的文件；生成器不再产出、但目录里还留着的旧文件不会被发现。
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$here/.." && pwd)
dir=codegen/internal/entity/testdata/awaitflow

work=$(mktemp -d "${TMPDIR:-/tmp}/awaitflow-runtime.XXXXXX")
trap 'rm -rf "$work"' EXIT

cd "$repo"
export GOWORK=off
cp -R "$dir" "$work/before"

go run ./codegen/cmd/entity -dir "./$dir" >/dev/null
go run ./codegen/cmd/nest -dir "./$dir" >/dev/null

if ! diff -r "$work/before" "$dir" >"$work/drift.diff"; then
	echo "awaitflow-runtime: $dir differs from what the current generators emit; the regenerated files are left in place, review and commit them" >&2
	cat "$work/drift.diff" >&2
	exit 1
fi

go test -race -count=1 "./$dir/..." "$@"
go run ./cmd/glsvet "./$dir/..."
