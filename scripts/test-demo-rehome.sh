#!/usr/bin/env bash
# B-1：account + 两个 game 的真实 TCP / Redis / Mongo / NATS / 文件 WAL 换服演练。
# 六个精确文件栅栏：accepted、quiesced、leaving、handed-off、committed、dropped；脚本收到栅栏事实后
# SIGKILL 源服，保留原 WAL 重启，前四点经登录续做。每点一个新角色，核对 account 唯一绑定、旧服拒绝、背包与钱包去重。
# committed→删除源库的崩溃窗口仍按 Q4 保留：必须报告旧副本，不能把它当成“自动清理已完成”。
# 第七场景压住真实投影之后的 WAL Ack，验证 HandOffEntity 等 checkpoint；DEMO_REHOME_MUTATION=skip-checkpoint
# 只在临时 go overlay 中删除等待，脚本必须非零退出（CHECKPOINT_BYPASS）。不修改生产源文件。
#
# source <私有 mirror-local 环境>/roost-dataengine-it/env.sh
# DEMO_REHOME_OUT=/abs/evidence bash scripts/test-demo-rehome.sh
# DEMO_REHOME_PORT_SHIFT=29000，DEMO_REHOME_ETCD_PORT=39579；默认运行全部场景，DEMO_REHOME_STAGES 可选一个。
# 依赖私有环境由调用方管理；公共 lib 只清理本次生成工程。此脚本不进 pretag，不做性能或跨机结论。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?private Mongo replica-set URI required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?private NATS required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?private Redis required}"
: "${DEMO_REHOME_OUT:?absolute evidence directory required}"
for tool in etcd jq curl mongosh redis-cli nats python3; do
	command -v "$tool" >/dev/null || { echo "$tool not found" >&2; exit 2; }
done
[[ "$DEMO_REHOME_OUT" == /* ]] || { echo 'DEMO_REHOME_OUT must be absolute' >&2; exit 2; }
case "${DEMO_REHOME_MUTATION:-}" in ''|skip-checkpoint) ;; *) echo 'unknown DEMO_REHOME_MUTATION' >&2; exit 2 ;; esac
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-demo-rehome.XXXXXX")"
project="${DEMO_REHOME_PROJECT:-$(printf 'rhd%06x' $(((RANDOM << 9) ^ RANDOM)))}"
out="$DEMO_REHOME_OUT"
port_shift="${DEMO_REHOME_PORT_SHIFT:-29000}"
etcd_port="${DEMO_REHOME_ETCD_PORT:-39579}"
etcd_pid=""
mkdir -p "$out/control"
git -C "$repo_dir" rev-parse HEAD >"$out/baseline.txt"
go version >"$out/go-version.txt"
uname -sm >"$out/platform.txt"
shasum -a 256 "$repo_dir/scripts/test-demo-rehome.sh" "$repo_dir/scripts/testdata/demo-drill/"rehome* >"$out/inputs.sha256"
export GOWORK=off GOFLAGS="${GOFLAGS:--p=2} -mod=mod"
# shellcheck source=testdata/demo-drill/lib.sh
source "$repo_dir/scripts/testdata/demo-drill/lib.sh"
cleanup() {
	# Ack 故障必须先撤，正常停机才能排空；阶段栅栏上的源服由本脚本 kill 后回收。
	rm -f "$out/control/hold-checkpoint"
	if [[ -f "$out/control/reached.json" && -f "$work_dir/$project/.dev/rehome-source.pid" ]]; then
		local pid; pid=$(cat "$work_dir/$project/.dev/rehome-source.pid")
		if [[ "$(jq -r .pid "$out/control/reached.json")" == "$pid" ]]; then kill -9 "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
	fi
	drill_cleanup
}
trap cleanup EXIT
drill_log "project=$project work=$work_dir evidence=$out mutation=${DEMO_REHOME_MUTATION:-none}"
drill_check_ports
drill_start_etcd
drill_generate
drill_configure
mkdir -p cmd/rehomedrill
sed -e "s|DRILL_MODULE|example.com/$project|g" -e 's/^func main()/func mergeMain()/' "$repo_dir/scripts/testdata/demo-drill/merge.go.txt" >cmd/rehomedrill/merge.go
sed "s|DRILL_MODULE|example.com/$project|g" "$repo_dir/scripts/testdata/demo-drill/rehome.go.txt" >cmd/rehomedrill/main.go
python3 "$repo_dir/scripts/testdata/demo-drill/rehome-instrument.py" "$repo_dir" "$PWD" "$work_dir/overlay" "${DEMO_REHOME_MUTATION:-}"
gofmt -w cmd/rehomedrill game/rehome/rehome_drill_faults.go game/rehome/rehome_driver.go "$work_dir/overlay/"*.go
go build -overlay "$work_dir/overlay/overlay.json" -o bin/rehomeapp .
go build -o bin/rehomedrill ./cmd/rehomedrill
go build -o bin/sidmove ./cmd/sidmove
game_target=$(drill_second_game_config 1001)
game_source=configs/service/config.game.yaml
nats_url="${ROOST_DATAENGINE_IT_NATS_URL%%,*}"
account_prefix=$(awk '/^account:/{a=1;next} a&&/^  key_prefix:/{print $2;exit}' configs/service/config.account.yaml | tr -d '"')
accountctl() { bin/accountctl -redis "$ROOST_DATAENGINE_IT_REDIS_ADDR" -prefix "$account_prefix" "$@"; }
drill() {
	bin/rehomedrill "$1" -state "$out/$case_name/state.json" -account-nats "$nats_url" -prefix "$project" \
		-endpoints "1000=127.0.0.1:$(drill_port 7000),1001=127.0.0.1:$(drill_port 7001)" "${@:2}"
}
wait_file() {
	local file="$1" i
	for i in $(seq 1 300); do [[ -s "$file" ]] && return 0; sleep 0.1; done
	drill_fail "file barrier not reached: $file"
}
source_start() { drill_start rehome-source bin/rehomeapp game 1000 "$game_source" DRILL_REHOME_FAULT_DIR="$out/control"; }
source_kill() {
	local pid; pid=$(cat .dev/rehome-source.pid)
	[[ "$(jq -r .pid "$out/control/reached.json")" == "$pid" ]] || drill_fail "barrier PID is not this source"
	kill -9 "$pid"
	wait "$pid" 2>/dev/null || true
	rm .dev/rehome-source.pid
	cp .dev/rehome-source.log "$out/$case_name/source-killed.log"
	cp "$out/control/reached.json" "$out/$case_name/reached.json"
	bin/sidmove wal-drained -dir data/wal/dataengine/1000 >"$out/$case_name/killed-wal.json" 2>"$out/$case_name/killed-wal.err" || true
	rm "$out/control/reached.json" "$out/control/arm.json"
}
snapshot() {
	local label="$1"
	mongosh --quiet "$ROOST_DATAENGINE_IT_MONGO_URI" --eval \
		"const id=Long('$entity_id'); print(EJSON.stringify({source:db.getSiblingDB('${project}_1000').player.findOne({_id:id}),target:db.getSiblingDB('${project}_1001').player.findOne({_id:id})}));" >"$out/$case_name/$label-documents.json"
	drill binding >"$out/$case_name/$label-binding.json"
}

# run.sh 使用已编译的 bin/app；11 个服务提供 demo 正式协作者（含 alliance）。
sh deploy/dev/run.sh start >"$out/dev-run.log" 2>&1 || { tail -25 "$out/dev-run.log"; drill_fail "start demo services"; }
sh deploy/dev/run.sh stop game >/dev/null
source_start
drill_start rehome-target bin/app game 1001 "$game_target"
accountctl upsert-server -sid 1000 >/dev/null
accountctl upsert-server -sid 1001 >/dev/null
admin_token=$(awk '/^ops:/{o=1;next} o&&/^  admin_token:/{print $2;exit}' "$game_source" | tr -d '"')
[[ -n "$admin_token" ]] || drill_fail "source admin token missing"

for case_name in ${DEMO_REHOME_STAGES:-accepted quiesced leaving handed-off committed dropped checkpoint}; do
	mkdir -p "$out/$case_name"
	drill_log "case $case_name"
	rm -f "$out/control/checkpoint-waiting" "$out/control/checkpoint-held"
	drill setup >"$out/$case_name/player.json"
	player_id=$(jq -r .player_id "$out/$case_name/player.json")
	entity_id=$(jq -r .entity_id "$out/$case_name/player.json")
	curl -fsS "http://127.0.0.1:$(drill_port 9100)/admin/execute" -H "X-Admin-Token: $admin_token" -H 'Content-Type: application/json' \
		-d "{\"name\":\"gm.player.coins\",\"operator\":\"rehome-drill\",\"payload\":{\"player_id\":$player_id,\"delta\":100,\"note\":\"private crash drill\"}}" >"$out/$case_name/credit.json"
	jq -e '.ok == true' "$out/$case_name/credit.json" >/dev/null || { cat "$out/$case_name/credit.json"; drill_fail "credit"; }
	drill spend >"$out/$case_name/spend.txt"
	snapshot before
	stage="$case_name"; [[ "$stage" != checkpoint ]] || stage=handed-off
	jq -n --arg stage "$stage" --argjson id "$player_id" '{Stage:$stage,PlayerID:$id}' >"$out/control/arm.json"
	if [[ "$case_name" == checkpoint ]]; then printf '%s\n' "$entity_id" >"$out/control/hold-checkpoint"; fi
	drill apply >"$out/$case_name/apply.json" 2>"$out/$case_name/disconnected.txt" &
	apply_pid=$!
	wait_file "$out/$case_name/apply.json"
	if [[ "$case_name" == checkpoint ]]; then
		for i in $(seq 1 300); do
			[[ ! -f "$out/control/reached.json" ]] || drill_fail "CHECKPOINT_BYPASS: hand-off returned before checkpoint release"
			[[ ! -f "$out/control/checkpoint-waiting" ]] || break
			sleep 0.1
		done
		[[ -f "$out/control/checkpoint-waiting" && -f "$out/control/checkpoint-held" ]] || drill_fail "checkpoint premise missing"
		sleep 0.2
		[[ ! -f "$out/control/reached.json" ]] || drill_fail "CHECKPOINT_BYPASS: wait did not hold"
		cp "$out/control/checkpoint-"* "$out/$case_name/"
		rm "$out/control/hold-checkpoint"
	fi
	wait_file "$out/control/reached.json"
	jq -e --arg stage "$stage" --argjson id "$player_id" '.stage == $stage and .player_id == $id' "$out/control/reached.json" >/dev/null || drill_fail "wrong fault marker"
	snapshot at-barrier
	if [[ "$case_name" != committed && "$case_name" != dropped ]]; then
		jq -e '.serving_server_id == 1000 and .transfer != null' "$out/$case_name/at-barrier-binding.json" >/dev/null || drill_fail "in-flight binding missing"
		drill source-refused -gate "127.0.0.1:$(drill_port 7000)" >"$out/$case_name/inflight-source-refused.txt"
		drill source-refused -gate "127.0.0.1:$(drill_port 7001)" >"$out/$case_name/inflight-target-refused.txt"
	fi
	source_kill
	wait "$apply_pid" || drill_fail "accepted connection did not disconnect"
	source_start
	# 登录原服触发 Resume；预期第一次得到 rehome_moving，直到 account 的持久绑定指向目标服。
	for i in $(seq 1 40); do
		drill binding >"$out/$case_name/recover-binding.json"
		if jq -e '.serving_server_id == 1001 and .transfer == null' "$out/$case_name/recover-binding.json" >/dev/null; then break; fi
		drill resume >>"$out/$case_name/resume.log" 2>&1 || true
		sleep 0.25
	done
	jq -e '.serving_server_id == 1001 and .transfer == null' "$out/$case_name/recover-binding.json" >/dev/null || drill_fail "transfer not recovered"
	drill verify >"$out/$case_name/verify.txt"
	drill source-refused -gate "127.0.0.1:$(drill_port 7000)" >"$out/$case_name/source-refused.txt"
	# 等已受理的投影，最终数据与源库状态以 Mongo 为准。
	for i in $(seq 1 30); do
		snapshot after
		if jq -e '.target.wallet["1"] == 93 and .target.bag["1"]["1001"] == 11 and .target.rehome["7"] == 1 and .target.rehome["1"] == ""' "$out/$case_name/after-documents.json" >/dev/null; then
			if [[ "$case_name" == committed ]]; then break; fi
			jq -e '.source == null' "$out/$case_name/after-documents.json" >/dev/null && break
		fi
		sleep 0.1
	done
	jq -e '.target.wallet["1"] == 93 and .target.bag["1"]["1001"] == 11 and .target.rehome["7"] == 1 and .target.rehome["1"] == ""' "$out/$case_name/after-documents.json" >/dev/null || drill_fail "persisted assets or arrival marker differ"
	if [[ "$case_name" == committed ]]; then
		jq -e '.source != null and .target != null' "$out/$case_name/after-documents.json" >/dev/null || drill_fail "Q4 retained-copy premise missing"
		drill_log 'KNOWN_LIMIT Q4: committed before source deletion; source copy retained, owner is target only' | tee "$out/$case_name/known-limit.txt"
	else
		jq -e '.source == null and .target != null' "$out/$case_name/after-documents.json" >/dev/null || drill_fail "source documents not dropped"
	fi
	cp .dev/rehome-source.log "$out/$case_name/source-recovered.log"
	drill_log "PASS $case_name: target binding, bag=11, wallet=93, source refused"
done
drill_stop rehome-source
drill_stop rehome-target
bin/sidmove wal-drained -dir data/wal/dataengine/1000 >"$out/source-wal-drained.json"
bin/sidmove wal-drained -dir data/wal/dataengine/1001 >"$out/target-wal-drained.json"
drill_log "PASS demo rehome drill; Q4 committed-window copy remains explicitly documented"
