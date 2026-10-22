#!/usr/bin/env bash
# 生成工程的心跳 / 行为 / 时间域端到端（DESIGN-TODO D20～D22 生成器接入）：当前 checkout 的 roost 生成工程
# （--skip-deps，replace 指回本仓），roost add behavior --ai --state、roost add heartbeat、roost add timedomain，
# roost generate 之后 go build / vet / test（-race）全部通过，并跑一条端到端用例：生成的示例动作由心跳推进到结束、
# 示例 AI 策略经 AI 心跳反复发起动作、场景的示例心跳执行、时间域改倍率后发布。
# 不需要外部依赖（DataEngine 换成不落库的 committer）。夹具在 scripts/testdata/heartbeat（只给本脚本用的
# handler 与用例，不是生成物）。成功后删除临时工程；失败或 ROOST_HEARTBEAT_KEEP=1 时保留。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-heartbeat-generated.XXXXXX")"
succeeded=0
cleanup() {
	if [[ "$succeeded" == 1 && "${ROOST_HEARTBEAT_KEEP:-0}" != 1 ]]; then
		rm -rf "$work_dir"
	else
		echo "heartbeat generated project kept: $work_dir" >&2
	fi
}
trap cleanup EXIT
export GOWORK=off
cd "$repo_dir"
go build -o "$work_dir/roost" ./codegen/cmd/roost
roost="$work_dir/roost"
project="$work_dir/planet"
"$roost" project new planet -module example.com/planet -out "$project" --skip-deps > "$work_dir/generate.log" 2>&1
cd "$project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_dir"
{
	"$roost" add entity Monster
	"$roost" add dao Monster --entity Monster
	"$roost" add behavior --entity Monster --ai --state
	"$roost" add entity Scene
	"$roost" add dao Scene --entity Scene
	"$roost" add heartbeat --entity Scene
	"$roost" add timedomain --entity Scene
} >> "$work_dir/generate.log" 2>&1
cp "$repo_dir/scripts/testdata/heartbeat/handlers.go.txt" game/handler/heartbeat_e2e.go
cp "$repo_dir/scripts/testdata/heartbeat/heartbeat_e2e_test.go.txt" internal/bootstrap/heartbeat_e2e_test.go
"$roost" generate >> "$work_dir/generate.log" 2>&1
go build -mod=mod ./...
go vet -mod=mod ./...
go test -mod=mod -race -count=1 -timeout 10m ./...
# 两种投递方式各跑一遍（heartbeat.delivery：batch 是缺省的 Nest 批量心跳作业，维护者 2026-10-22 决定；dispatch 是单目标
# 可选项）。上面不设环境变量的 go test ./... 跑的是缺省（用例断言驱动是 batch）。
for delivery in dispatch batch; do
	ROOST_HEARTBEAT_DELIVERY="$delivery" go test -mod=mod -race -count=1 -timeout 5m -v -run '^TestGeneratedHeartbeatBehaviorEndToEnd$' ./internal/bootstrap | tee "$work_dir/e2e-$delivery.log"
	# 用例必须实际执行并通过：-run 写错或被跳过时 go test 仍会 ok。
	if [[ "$(grep -c '^--- PASS: TestGeneratedHeartbeatBehaviorEndToEnd' "$work_dir/e2e-$delivery.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$work_dir/e2e-$delivery.log"; then
		echo "TestGeneratedHeartbeatBehaviorEndToEnd ($delivery) did not pass" >&2
		exit 1
	fi
done
"$roost" id check
"$roost" generate --check >> "$work_dir/generate.log" 2>&1
succeeded=1
