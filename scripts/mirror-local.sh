#!/usr/bin/env bash
# Mirror 第 6 步的本机替代（docs/feature/MIRROR-STEP-6-LOCAL-2026-10-06.md）：在本机私有依赖进程上做
# 只读方（RemoteMirrorMod）的故障与性能验证。默认不在 CI 跑；`test` / `bench` 才会起进程。
#
# 依赖全部由本脚本自起，复用 kit/scripts/integration/lib（根目录、端口平移、按命令行认领 pid）：
#   - NATS JetStream 3 节点、Mongo 3 节点副本集、Redis 单机、toxiproxy（与隔离环境脚本同一套 lib 函数，但根目录是私有的）；
#   - Redis 单机的 1 个副本（单机切主：副本提升后把 toxiproxy 的 redis 代理改指新主，客户端地址不变）；
#   - Redis 3 主 3 从 Cluster（node-timeout 1s）。
# 不碰共享隔离环境：根目录是 $ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it（缺省在 $TMPDIR 下），端口偏移
# 缺省 20000；拒绝 ~/.roost-it 与偏移 0 / 1000（共享环境与历史默认环境）。不读共享环境的 env.sh。
# 私有根目录只有本脚本与它调起的用例在用，不取 A5 的 remote-acceptance.lock（那把锁保护的是共享隔离环境）。
#
# 用法：
#   scripts/mirror-local.sh up | status | down | clean
#   scripts/mirror-local.sh fault <动作> [参数]   用例经 ROOST_MIRROR_LOCAL_SCRIPT 调用，动作见 fault_action
#   scripts/mirror-local.sh test                  up → 生成工程里的 TestGeneratedRemoteMirrorLocal（两进程故障场景）→ clean
#   scripts/mirror-local.sh test-core             up → remoteentity 的私有环境集成用例（缺省 ^TestMirrorLocal，
#                                                 ROOST_MIRROR_LOCAL_CORE_RUN 覆盖；O-M6-3 墓碑 WAIT 的切主红绿、
#                                                 O-M6-6 同 sid 重启接管旧代锁）→ clean
#   scripts/mirror-local.sh bench <输出目录>       up → remoteentity 的 BenchmarkMirrorLocal*（当前源码）→ clean
#     基线对照：ROOST_MIRROR_LOCAL_BASELINE=<基线源码目录>（例如 v1.20.2 的 detached worktree）时同一份基准文件
#     复制到基线上，与当前源码交替各跑 ROOST_MIRROR_LOCAL_COUNT（缺省 6）次，benchstat 出对照（见文档 §4）。
#   ROOST_MIRROR_LOCAL_KEEP=1 时 test / test-core / bench 结束不清理（调试用，之后手工 clean）。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
lib_dir="$repo_dir/kit/scripts/integration/lib"

home="${ROOST_MIRROR_LOCAL_HOME:-${TMPDIR:-/tmp}/roost-mirror-local}"
home="${home%/}"
offset="${ROOST_MIRROR_LOCAL_OFFSET:-20000}"
[[ "$home" == /* ]] || { echo "ROOST_MIRROR_LOCAL_HOME must be absolute: $home" >&2; exit 2; }
mkdir -p "$home"
if [[ -d "$HOME/.roost-it" && "$(cd "$home" && pwd -P)" == "$(cd "$HOME/.roost-it" && pwd -P)" ]]; then
	echo "refuse: $home is the shared isolated environment (~/.roost-it)" >&2
	exit 2
fi
if [[ "$offset" == 0 || "$offset" == 1000 ]]; then
	echo "refuse: port offset $offset belongs to the shared / default environment" >&2
	exit 2
fi
# 只用本脚本算出的根目录；继承来的共享环境变量一律清掉。
unset ROOST_DATAENGINE_IT_ROOT ROOST_REMOTE_ACCEPTANCE_LOCK_HELD ROOST_DATAENGINE_IT ROOST_DATAENGINE_IT_MONGO_URI \
	ROOST_DATAENGINE_IT_NATS_URL ROOST_DATAENGINE_IT_REDIS_ADDR ROOST_DATAENGINE_IT_TOXIPROXY_URL \
	ROOST_DATAENGINE_IT_NATS_PROXIED_URL ROOST_DATAENGINE_IT_REDIS_PROXIED_ADDR
export ROOST_IT_HOME="$home" ROOST_IT_PORT_OFFSET="$offset" ROOST_IT_TOXIPROXY=1
export ROOST_IT_MONGO_CACHE_GB="${ROOST_IT_MONGO_CACHE_GB:-0.5}"

# shellcheck source=../kit/scripts/integration/lib/common.sh
source "$lib_dir/common.sh"
source "$lib_dir/mongo.sh"
source "$lib_dir/nats.sh"
source "$lib_dir/redis.sh"
source "$lib_dir/toxiproxy.sh"
require_safe_root

# ---- Redis 单机的副本 ----
replica_port() { roost_it_port 16380; }
replica_dir() { printf '%s/redis-replica\n' "$ROOST_IT_ROOT"; }
replica_pid_file() { printf '%s/redis.pid\n' "$(replica_dir)"; }

# start_redis_node <dir> <port> [额外参数...]：与 lib/redis.sh 同样的认领方式（--dir 带根目录、不改进程名）。
start_redis_node() {
	local dir="$1" port="$2"
	shift 2
	mkdir -p "$dir"
	local pid_file="$dir/redis.pid"
	if read_owned_pid "$pid_file" >/dev/null 2>&1; then
		return 0
	fi
	require_port_available_or_owned "$port" "$pid_file" "redis $port"
	rm -f -- "$pid_file"
	nohup redis-server --port "$port" --bind 127.0.0.1 --dir "$dir" --save "" --set-proc-title no --pidfile "$pid_file" "$@" </dev/null >>"$dir/redis.log" 2>&1 &
	wait_until 15 "redis $port" bash -c "[[ \"\$(redis-cli -p $port ping 2>/dev/null)\" == PONG ]]"
}

replica_up() {
	start_redis_node "$(replica_dir)" "$(replica_port)" --appendonly no --replicaof 127.0.0.1 "$(redis_port)"
}

# ---- Redis Cluster：3 主 3 从 ----
cluster_port() { roost_it_port "$((17399 + $1))"; }
cluster_dir() { printf '%s/redis-cluster-%d\n' "$ROOST_IT_ROOT" "$1"; }
cluster_addrs() {
	local i out=""
	for i in 1 2 3 4 5 6; do out+="${out:+,}127.0.0.1:$(cluster_port "$i")"; done
	printf '%s\n' "$out"
}
cluster_node_start() {
	local i="$1"
	require_port_available_or_owned "$(( $(cluster_port "$i") + 10000 ))" "$(cluster_dir "$i")/redis.pid" "redis cluster bus $i"
	start_redis_node "$(cluster_dir "$i")" "$(cluster_port "$i")" --cluster-enabled yes --cluster-config-file nodes.conf \
		--cluster-node-timeout 1000 --appendonly yes --appendfsync everysec
}
cluster_ok() {
	local i
	for i in 1 2 3 4 5 6; do
		redis-cli -p "$(cluster_port "$i")" cluster info 2>/dev/null | grep -q 'cluster_state:ok' || return 1
	done
}
# cluster_replicas_online：每个主节点都至少有一个 state=online 的副本。只看 cluster_state:ok 不够——刚建好的集群
# 副本还在全量同步，ROLE 不列出它们，墓碑 WAIT 按 no_replicas 跳过（2026-10-06 发版前验证：
# TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary 单独或第一个跑时因此失败）。
cluster_replicas_online() {
	local i info
	for i in 1 2 3 4 5 6; do
		info="$(redis-cli -p "$(cluster_port "$i")" info replication 2>/dev/null)" || return 1
		if grep -q '^role:master' <<<"$info"; then
			grep -q '^slave0:.*state=online' <<<"$info" || return 1
		fi
	done
}
cluster_up() {
	local i addrs=()
	for i in 1 2 3 4 5 6; do
		cluster_node_start "$i"
		addrs+=("127.0.0.1:$(cluster_port "$i")")
	done
	if ! redis-cli -p "$(cluster_port 1)" cluster info 2>/dev/null | grep -q 'cluster_known_nodes:6'; then
		redis-cli --cluster create "${addrs[@]}" --cluster-replicas 1 --cluster-yes >"$ROOST_IT_ROOT/redis-cluster-create.log" 2>&1
	fi
	wait_until 30 "redis cluster ok" cluster_ok
	wait_until 30 "redis cluster replicas online" cluster_replicas_online
}

# ---- 进程控制 ----
all_pid_files() {
	local i
	for i in 1 2 3; do nats_node_pid_file "$i"; mongo_node_pid_file "$i"; done
	redis_pid_file
	replica_pid_file
	for i in 1 2 3 4 5 6; do printf '%s/redis.pid\n' "$(cluster_dir "$i")"; done
}

# signal_owned <pid 文件> <信号>：只对本根目录认领到的进程发信号（SIGSTOP / SIGCONT / SIGKILL）。
signal_owned() {
	local pid
	pid="$(read_owned_pid "$1")" || { roost_it_error "no owned process for $1"; return 1; }
	kill "-$2" "$pid"
}

kill_owned() {
	local pid
	pid="$(read_owned_pid "$1")" || return 0
	kill -CONT "$pid" 2>/dev/null || true
	kill -KILL "$pid"
	while pid_is_running "$pid"; do sleep 0.05; done
}

environment_up() {
	mkdir -p "$ROOST_IT_ROOT"
	record_port_offset
	require_commands mongod mongosh nats-server redis-server redis-cli toxiproxy-server curl jq nc ps
	mongo_up
	nats_up
	redis_up
	replica_up
	toxiproxy_up
	cluster_up
	write_environment_file
	{
		printf 'export ROOST_MIRROR_LOCAL_SCRIPT=%q\n' "$repo_dir/scripts/mirror-local.sh"
		printf 'export ROOST_MIRROR_LOCAL_HOME=%q\n' "$home"
		printf 'export ROOST_MIRROR_LOCAL_OFFSET=%q\n' "$offset"
		printf 'export ROOST_MIRROR_LOCAL_REDIS_CLUSTER=%q\n' "$(cluster_addrs)"
		printf 'export ROOST_MIRROR_LOCAL_REDIS_REPLICA=%q\n' "127.0.0.1:$(replica_port)"
		printf 'export ROOST_MIRROR_LOCAL_NATS_MONITOR=%q\n' "http://127.0.0.1:$(nats_monitor_port 1)"
	} >>"$ROOST_IT_ROOT/env.sh"
}

environment_down() {
	local file
	# 先 SIGCONT：被 SIGSTOP 的进程收不到 TERM。
	while read -r file; do
		local pid
		pid="$(read_owned_pid "$file" 2>/dev/null)" && kill -CONT "$pid" 2>/dev/null || true
	done < <(all_pid_files)
	toxiproxy_down || true
	local i
	for i in 6 5 4 3 2 1; do stop_owned_pid "$(cluster_dir "$i")/redis.pid" "redis-cluster-$i" || true; done
	stop_owned_pid "$(replica_pid_file)" "redis-replica" || true
	redis_down || true
	nats_down || true
	mongo_down || true
}

# residual_check：根目录下的进程与本偏移的 toxiproxy 都不在了才返回 0。
residual_check() {
	local left
	left="$(ps -axo pid=,command= | grep -F " $ROOST_IT_ROOT/" | grep -v grep || true)"
	left+="$(ps -axo pid=,command= | grep -F "toxiproxy-server -host 127.0.0.1 -port $(toxiproxy_api_port)" | grep -v grep || true)"
	if [[ -n "$left" ]]; then
		roost_it_error "residual processes:"
		printf '%s\n' "$left" >&2
		return 1
	fi
	roost_it_log "no residual processes under $ROOST_IT_ROOT"
}

environment_clean() {
	environment_down
	residual_check
	[[ "$ROOST_IT_ROOT" == "$ROOST_IT_CANONICAL_ROOT" ]] || { roost_it_error "refuse unsafe root $ROOST_IT_ROOT"; return 2; }
	rm -rf -- "$ROOST_IT_CANONICAL_ROOT"
	rmdir "$home" 2>/dev/null || true
	roost_it_log "removed $ROOST_IT_CANONICAL_ROOT"
}

environment_status() {
	mongo_status || true
	nats_status || true
	redis_status || true
	redis-cli -p "$(replica_port)" info replication 2>/dev/null | grep -E '^role|master_link_status' || true
	toxiproxy_status
	cluster_ok && roost_it_log "redis cluster=ok $(cluster_addrs)" || roost_it_log "redis cluster=not ok"
}

# cluster_master_of <键>：打印负责该键槽位的主节点序号（1..6）。
cluster_master_of() {
	local slot i
	slot="$(redis-cli -p "$(cluster_port 1)" cluster keyslot "$1")"
	for i in 1 2 3 4 5 6; do
		# 问每个活着的节点自己：是主、且槽位区间含该槽。
		if redis-cli -p "$(cluster_port "$i")" cluster nodes 2>/dev/null | awk -v slot="$slot" '
			$3 ~ /myself/ && $3 ~ /master/ {
				for (f = 9; f <= NF; f++) { n = split($f, r, "-"); if (n == 1) r[2] = r[1]; if (slot + 0 >= r[1] + 0 && slot + 0 <= r[2] + 0) found = 1 }
			}
			END { exit found ? 0 : 1 }'; then
			printf '%d\n' "$i"
			return 0
		fi
	done
	return 1
}

# cluster_replica_of <主节点序号>：打印它的副本序号。
cluster_replica_of() {
	local id i
	id="$(redis-cli -p "$(cluster_port "$1")" cluster myid)"
	for i in 1 2 3 4 5 6; do
		if redis-cli -p "$(cluster_port "$i")" cluster nodes | awk -v id="$id" '$3 ~ /myself/ && $3 ~ /slave/ && $4 == id {found=1} END {exit found?0:1}'; then
			printf '%d\n' "$i"
			return 0
		fi
	done
	return 1
}

standalone_upstream_port() {
	curl --silent --fail "$(toxiproxy_api)/proxies/redis" | jq -r '.upstream' | sed 's/.*://'
}

# standalone_failover <lossy|graceful>：把 toxiproxy redis 代理后面的主切到另一个节点。
#   graceful：等副本追平偏移量再提升（不丢写）；旧主降为新主的副本。
#   lossy：旧主 SIGKILL，副本（此前可能已被 SIGSTOP，先 SIGCONT）不等追平直接提升；旧主之后以副本身份重启。
standalone_failover() {
	local mode="$1" old new old_dir new_dir
	old="$(standalone_upstream_port)"
	if [[ "$old" == "$(redis_port)" ]]; then new="$(replica_port)"; old_dir="$(redis_dir)"; new_dir="$(replica_dir)"; else new="$(redis_port)"; old_dir="$(replica_dir)"; new_dir="$(redis_dir)"; fi
	if [[ "$mode" == graceful ]]; then
		wait_until 10 "replica caught up" bash -c "
			m=\$(redis-cli -p $old info replication | awk -F: '/^master_repl_offset/ {print \$2}' | tr -d '\r');
			r=\$(redis-cli -p $new info replication | awk -F: '/^master_repl_offset/ {print \$2}' | tr -d '\r');
			[[ -n \"\$m\" && \"\$m\" == \"\$r\" ]]"
	else
		kill_owned "$old_dir/redis.pid"
		signal_owned "$new_dir/redis.pid" CONT 2>/dev/null || true
	fi
	redis-cli -p "$new" replicaof no one >/dev/null
	curl --silent --show-error --fail -X POST "$(toxiproxy_api)/proxies/redis" -H 'Content-Type: application/json' \
		-d "{\"upstream\":\"127.0.0.1:$new\"}" >/dev/null
	if [[ "$mode" == graceful ]]; then
		redis-cli -p "$old" replicaof 127.0.0.1 "$new" >/dev/null
	else
		start_redis_node "$old_dir" "$old" --appendonly no --replicaof 127.0.0.1 "$new"
	fi
	printf 'standalone primary %s -> %s (%s)\n' "$old" "$new" "$mode"
}

fault_action() {
	local action="${1:-}"
	shift || true
	case "$action" in
		nats-kill) kill_owned "$(nats_node_pid_file "$1")" ;;
		nats-stop) signal_owned "$(nats_node_pid_file "$1")" STOP ;;
		nats-cont) signal_owned "$(nats_node_pid_file "$1")" CONT ;;
		nats-start) nats_start_node "$1"; wait_until 45 "NATS JetStream cluster ready" nats_cluster_ready ;;
		mongo-stepdown)
			# 主节点让位 10 秒；mongosh 的连接会被主动断开，返回码不作数，之后等新主出现。
			mongosh "$ROOST_IT_MONGO_URI" --quiet --eval 'try { db.adminCommand({replSetStepDown: 10, secondaryCatchUpPeriodSecs: 5}) } catch (e) {}' >/dev/null 2>&1 || true
			wait_until 30 "new Mongo primary" mongo_replica_ready
			mongo_primary_node
			;;
		mongo-settle)
			# mongo-1 优先级最高，让位期（10s）过后会再选回来：等到它重新成为主、副本集全部健康，下一个场景才开始。
			local deadline=$((SECONDS + 60))
			until [[ "$(mongo_primary_node 2>/dev/null)" == mongo-1 ]] && mongo_replica_ready; do
				(( SECONDS < deadline )) || { roost_it_error "Mongo did not settle on mongo-1"; return 1; }
				sleep 0.5
			done
			;;
		redis-standalone-pause-replica)
			local port
			port="$(standalone_upstream_port)"
			# 暂停副本并断开复制连接：之后的写只留在主上（SIGSTOP 本身不够，内核缓冲里的复制流在 SIGCONT 后照样被读到）。
			if [[ "$port" == "$(redis_port)" ]]; then signal_owned "$(replica_pid_file)" STOP; else signal_owned "$(redis_pid_file)" STOP; fi
			redis-cli -p "$port" client kill type replica >/dev/null
			;;
		redis-standalone-failover) standalone_failover "${1:-graceful}" ;;
		redis-cluster-pause-replica)
			local master replica
			master="$(cluster_master_of "$1")"
			replica="$(cluster_replica_of "$master")"
			signal_owned "$(cluster_dir "$replica")/redis.pid" STOP
			redis-cli -p "$(cluster_port "$master")" client kill type replica >/dev/null
			printf 'paused replica %d of master %d\n' "$replica" "$master"
			;;
		redis-cluster-kill-master)
			# 杀掉负责该键的主节点（SIGKILL），把可能被暂停的副本 SIGCONT，等副本接管、集群恢复 ok，再把旧主以副本身份拉起。
			local master replica i
			master="$(cluster_master_of "$1")"
			replica="$(cluster_replica_of "$master")"
			# 副本必须在线（刚重启的旧主还在全量同步时杀新主，副本不会被提升）；被 SIGSTOP 的副本不等（未复制即切主）。
			if [[ "$(ps -o stat= -p "$(read_owned_pid "$(cluster_dir "$replica")/redis.pid")" 2>/dev/null)" != T* ]]; then
				wait_until 30 "cluster replica $replica in sync" bash -c "redis-cli -p $(cluster_port "$replica") info replication | grep -q 'master_link_status:up'"
			fi
			kill_owned "$(cluster_dir "$master")/redis.pid"
			for i in 1 2 3 4 5 6; do
				local pid
				pid="$(read_owned_pid "$(cluster_dir "$i")/redis.pid" 2>/dev/null)" && kill -CONT "$pid" 2>/dev/null || true
			done
			# 等另一个节点以 myself,master 认领该槽（副本接管），旧主此时还没拉起，不会被认成主。
			local deadline=$((SECONDS + 30)) now
			until now="$(cluster_master_of "$1" 2>/dev/null)" && [[ "$now" != "$master" ]]; do
				if (( SECONDS >= deadline )); then
					roost_it_error "redis cluster failover did not happen; restarting node $master"
					cluster_node_start "$master"
					return 1
				fi
				sleep 0.1
			done
			cluster_node_start "$master"
			wait_until 30 "redis cluster ok" cluster_ok
			wait_until 30 "restarted node $master in sync" bash -c "redis-cli -p $(cluster_port "$master") info replication | grep -q 'master_link_status:up'"
			printf 'killed cluster master %d (port %d); new master %s\n' "$master" "$(cluster_port "$master")" "$(cluster_master_of "$1")"
			;;
		redis-cluster-stop-replica)
			# 只 SIGSTOP 该键主节点的副本、不断开复制连接：主节点仍认为副本在线，WAIT 等到超时（O-M6-3 的 short）。
			# 之后用 redis-cluster-cont 恢复；暂停超过 node-timeout（1s）集群会把它标为失败，恢复后自动回来。
			local master replica
			master="$(cluster_master_of "$1")"
			replica="$(cluster_replica_of "$master")"
			signal_owned "$(cluster_dir "$replica")/redis.pid" STOP
			printf 'stopped replica %d of master %d\n' "$replica" "$master"
			;;
		redis-cluster-cont)
			local i
			for i in 1 2 3 4 5 6; do
				local pid
				pid="$(read_owned_pid "$(cluster_dir "$i")/redis.pid" 2>/dev/null)" && kill -CONT "$pid" 2>/dev/null || true
			done
			wait_until 30 "redis cluster ok" cluster_ok
			;;
		redis-cluster-heal)
			local i
			for i in 1 2 3 4 5 6; do cluster_node_start "$i"; done
			wait_until 30 "redis cluster ok" cluster_ok
			;;
		*) roost_it_error "unknown fault action: $action"; return 2 ;;
	esac
}

run_generated_tests() {
	# shellcheck disable=SC1091
	source "$ROOST_IT_ROOT/env.sh"
	export ROOST_MIRROR_LOCAL=1
	ROOST_REMOTE_RUN="${ROOST_REMOTE_RUN:-^TestGeneratedRemoteMirrorLocal$}" ROOST_REMOTE_TIMEOUT="${ROOST_REMOTE_TIMEOUT:-20m}" \
		bash "$repo_dir/scripts/test-remote-generated.sh"
}

# run_core_tests：remoteentity 里依赖私有环境的集成用例（integration tag，ROOST_MIRROR_LOCAL=1 才运行）。
run_core_tests() {
	(
		# shellcheck disable=SC1091
		source "$ROOST_IT_ROOT/env.sh"
		export ROOST_MIRROR_LOCAL=1
		cd "$repo_dir"
		GOWORK=off go test -tags integration -count=1 -run "${ROOST_MIRROR_LOCAL_CORE_RUN:-^TestMirrorLocal}" -v -timeout 10m ./remoteentity
	)
}

# run_bench <输出文件> <源码目录>：在 <源码目录> 跑一次 BenchmarkMirrorLocal*（-count 1），追加到输出文件。
run_bench() {
	local out="$1" source_dir="$2"
	(
		# shellcheck disable=SC1091
		source "$ROOST_IT_ROOT/env.sh"
		export ROOST_MIRROR_LOCAL=1
		cd "$source_dir"
		GOWORK=off go test -tags integration -run '^$' -bench "${ROOST_MIRROR_LOCAL_BENCH:-^BenchmarkMirrorLocal}" \
			-benchtime 1x -count 1 -timeout 60m ./remoteentity
	) | tee -a "$out"
}

# bench：当前源码与（可选）基线交替各跑 ROOST_MIRROR_LOCAL_COUNT 次（缺省 6），减少同机漂移；有 benchstat 时出对照表。
bench_all() {
	local dir="$1" count="${ROOST_MIRROR_LOCAL_COUNT:-6}" i
	mkdir -p "$dir"
	: >"$dir/current.txt"
	if [[ -n "${ROOST_MIRROR_LOCAL_BASELINE:-}" ]]; then
		cp "$repo_dir/remoteentity/mirror_local_bench_integration_test.go" "$ROOST_MIRROR_LOCAL_BASELINE/remoteentity/"
		: >"$dir/baseline.txt"
	fi
	for ((i = 1; i <= count; i++)); do
		run_bench "$dir/current.txt" "$repo_dir"
		if [[ -n "${ROOST_MIRROR_LOCAL_BASELINE:-}" ]]; then
			run_bench "$dir/baseline.txt" "$ROOST_MIRROR_LOCAL_BASELINE"
		fi
	done
	local benchstat_bin
	benchstat_bin="$(command -v benchstat || true)"
	[[ -n "$benchstat_bin" || ! -x "$HOME/go/bin/benchstat" ]] || benchstat_bin="$HOME/go/bin/benchstat"
	if [[ -n "${ROOST_MIRROR_LOCAL_BASELINE:-}" && -n "$benchstat_bin" ]]; then
		"$benchstat_bin" "$dir/baseline.txt" "$dir/current.txt" | tee "$dir/benchstat.txt"
	fi
}

finish() {
	local status=$?
	if [[ "${ROOST_MIRROR_LOCAL_KEEP:-0}" != 1 ]]; then
		environment_clean || status=1
	fi
	exit "$status"
}

case "${1:-}" in
	up) environment_up; environment_status ;;
	status) environment_status ;;
	down) environment_down ;;
	clean) environment_clean ;;
	fault) shift; fault_action "$@" ;;
	test)
		trap finish EXIT
		environment_up
		run_generated_tests
		;;
	test-core)
		trap finish EXIT
		environment_up
		run_core_tests
		;;
	bench)
		[[ -n "${2:-}" ]] || { echo "bench needs an output directory" >&2; exit 2; }
		trap finish EXIT
		environment_up
		bench_all "$2"
		;;
	*) sed -n '2,23p' "${BASH_SOURCE[0]}" >&2; exit 2 ;;
esac
