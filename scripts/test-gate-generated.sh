#!/usr/bin/env bash
# roost add gate 在生成工程上的离线验收：-template game 的全新工程（--skip-deps，replace 指回本仓）装 login、enter、portal，
# roost add gate 把嵌入 TCP 改成独立 Gate；之后 go build / vet、roost project doctor（含 gate:* 检查与编译后进程的
# --check-config）、roost generate --check 全部通过，再跑一次 roost add gate 与 roost project sync 都不改工程。
# 缺省 Gate 名 gate 与自定义名 edge（roost add gate edge）各做一遍：生成的两端接线必须写实际服务名
# （gatewiring.Options.GateService / GameService，RV-20261116-02），自定义名要能编译、过 doctor 与 generate --check。
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
run_gate() {
	local name="$1" project="$work_dir/gated-$1"
	"$roost" project new gated -module example.com/gated -out "$project" -template game --skip-deps >"$work_dir/generate-$name.log" 2>&1
	cd "$project"
	go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_dir"
	"$roost" module add login enter portal >>"$work_dir/generate-$name.log" 2>&1
	if [[ "$name" == gate ]]; then "$roost" add gate; else "$roost" add gate "$name"; fi | tee "$work_dir/add-gate-$name.log"
	grep -qE "^ +gate: $name\$" roost.yaml || { echo "roost.yaml has no access.player.gate: $name" >&2; exit 1; }
	# 两端生成的接线写同一对实际服务名（Gate 服务 / 玩家服务 game）。
	for rel in internal/bootstrap/generated.go internal/access/player/tcp/server_gen.go; do
		if ! grep -qF "options.GateService = \"$name\"" "$rel" || ! grep -qF 'options.GameService = "game"' "$rel"; then
			echo "$rel does not wire the actual service names $name/game" >&2
			exit 1
		fi
	done
	git init -q . && git add -A && git -c user.email=gate@example.com -c user.name=gate commit -qm gate
	go build -mod=mod ./...
	go vet -mod=mod ./...
	"$roost" project doctor >"$work_dir/doctor-$name.log" 2>&1 || { cat "$work_dir/doctor-$name.log"; exit 1; }
	for check in gate:ingress gate:player-tcp gate:namespace gate:etcd-singleton gate:client-address "config-check:$name" config-check:game; do
		grep -qE "^OK +$check( |\$)" "$work_dir/doctor-$name.log" || { cat "$work_dir/doctor-$name.log"; echo "doctor did not pass $check" >&2; exit 1; }
	done
	"$roost" generate --check
	if [[ "$name" == gate ]]; then "$roost" add gate; else "$roost" add gate "$name"; fi | grep -q 'nothing changed' || { echo "a second roost add gate $name did not report nothing changed" >&2; exit 1; }
	"$roost" project sync --skip-deps >/dev/null
	[[ -z "$(git status --porcelain)" ]] || { git status --short >&2; echo "repeating roost add gate $name / project sync changed the project" >&2; exit 1; }
	cd "$repo_dir"
}
run_gate gate
run_gate edge
echo "gate generated project (gate and edge): add gate, actual service names in the wiring, build, vet, doctor (gate:*), generate --check and an idempotent repeat passed"
succeeded=1
