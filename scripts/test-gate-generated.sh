#!/usr/bin/env bash
# roost add gate 在生成工程上的离线验收：-template game 的全新工程（--skip-deps，replace 指回本仓）装 login、enter、portal，
# roost add gate 把嵌入 TCP 改成独立 Gate；之后 go build / vet、roost project doctor（含 gate:* 检查与编译后进程的
# --check-config）、roost generate --check 全部通过，再跑一次 roost add gate 与 roost project sync 都不改工程。
# 不连 Mongo / NATS / Redis / etcd。真实进程演练（account + 经 Game Ingress 的 game + Gate、经 Gate 登录一次）不在这里，
# 见 docs/framework/GATEWAY.md §2。成功后删除临时工程；失败或 ROOST_GATE_KEEP=1 时保留。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-gate-generated.XXXXXX")"
succeeded=0
cleanup() {
	if [[ "$succeeded" == 1 && "${ROOST_GATE_KEEP:-0}" != 1 ]]; then
		rm -rf "$work_dir"
	else
		echo "gate generated project kept: $work_dir" >&2
	fi
}
trap cleanup EXIT
export GOWORK=off
cd "$repo_dir"
go build -o "$work_dir/roost" ./codegen/cmd/roost
roost="$work_dir/roost"
project="$work_dir/gated"
"$roost" project new gated -module example.com/gated -out "$project" -template game --skip-deps >"$work_dir/generate.log" 2>&1
cd "$project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_dir"
"$roost" module add login enter portal >>"$work_dir/generate.log" 2>&1
"$roost" add gate | tee "$work_dir/add-gate.log"
grep -qE '^ +gate: gate$' roost.yaml || { echo "roost.yaml has no access.player.gate" >&2; exit 1; }
git init -q . && git add -A && git -c user.email=gate@example.com -c user.name=gate commit -qm gate
go build -mod=mod ./...
go vet -mod=mod ./...
"$roost" project doctor >"$work_dir/doctor.log" 2>&1 || { cat "$work_dir/doctor.log"; exit 1; }
for check in gate:ingress gate:player-tcp gate:namespace gate:etcd-singleton gate:client-address config-check:gate config-check:game; do
	grep -qE "^OK +$check( |$)" "$work_dir/doctor.log" || { cat "$work_dir/doctor.log"; echo "doctor did not pass $check" >&2; exit 1; }
done
"$roost" generate --check
"$roost" add gate | grep -q 'nothing changed' || { echo "a second roost add gate did not report nothing changed" >&2; exit 1; }
"$roost" project sync --skip-deps >/dev/null
[[ -z "$(git status --porcelain)" ]] || { git status --short >&2; echo "repeating roost add gate / project sync changed the project" >&2; exit 1; }
echo "gate generated project: add gate, build, vet, doctor (gate:*), generate --check and an idempotent repeat passed"
succeeded=1
