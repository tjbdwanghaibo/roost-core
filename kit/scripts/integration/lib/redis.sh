#!/usr/bin/env bash
# One isolated Redis node for the lock / versionstore fault tests. Port 16379
# so a developer's own Redis on 6379 is never touched; toxiproxy fronts it on
# 26379 when installed (see toxiproxy.sh).

redis_port() { printf '16379\n'; }
redis_dir() { printf '%s/redis\n' "$ROOST_IT_ROOT"; }
redis_pid_file() { printf '%s/redis.pid\n' "$(redis_dir)"; }

redis_ready() {
	[[ "$(redis-cli -p "$(redis_port)" ping 2>/dev/null)" == "PONG" ]]
}

redis_up() {
	local dir pid_file
	dir="$(redis_dir)"
	pid_file="$(redis_pid_file)"
	mkdir -p "$dir"
	if read_owned_pid "$pid_file" >/dev/null 2>&1; then
		wait_until 15 "redis" redis_ready
		return 0
	fi
	require_port_available_or_owned "$(redis_port)" "$pid_file" "redis"
	rm -f -- "$pid_file"
	roost_it_log "starting redis on 127.0.0.1:$(redis_port)"
	# --set-proc-title no: redis-server rewrites its argv to "redis-server 127.0.0.1:16379"
	# by default, and read_owned_pid recognises our processes by the test root in
	# their command line; with the rewrite a restart refused its own node.
	nohup redis-server --port "$(redis_port)" --bind 127.0.0.1 --dir "$dir" --save "" --appendonly no --set-proc-title no --pidfile "$pid_file" </dev/null > "$dir/redis.log" 2>&1 &
	wait_until 15 "redis" redis_ready
}

redis_down() {
	stop_owned_pid "$(redis_pid_file)" "redis"
}

redis_status() {
	if redis_ready; then
		roost_it_log "redis=RUNNING 127.0.0.1:$(redis_port)"
	else
		roost_it_log "redis=DOWN"
		return 1
	fi
}

redis_addr() { printf '127.0.0.1:%d\n' "$(redis_port)"; }
