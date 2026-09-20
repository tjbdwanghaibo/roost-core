#!/usr/bin/env bash
# toxiproxy sits between the tests and the isolated NATS nodes so a test can
# add network-level faults — latency, resets, half-open connections — that a
# process kill (fault nats-all) cannot express. It is optional for the
# ordinary suite and required by the nightly fault matrix
# (ROOST_IT_TOXIPROXY=1). Mongo is not proxied: replica-set discovery hands the
# driver each member's own advertised address, so a proxy in front of the seed
# list is bypassed after the first hello.

toxiproxy_api_port() { printf '18474\n'; }
toxiproxy_proxy_port() { printf '%d\n' "$((24221 + $1))"; }
toxiproxy_redis_port() { printf '26379\n'; }
toxiproxy_dir() { printf '%s/toxiproxy\n' "$ROOST_IT_ROOT"; }
toxiproxy_pid_file() { printf '%s/toxiproxy.pid\n' "$(toxiproxy_dir)"; }
toxiproxy_api() { printf 'http://127.0.0.1:%d\n' "$(toxiproxy_api_port)"; }

toxiproxy_available() {
	command -v toxiproxy-server >/dev/null 2>&1
}

toxiproxy_required() {
	[[ "${ROOST_IT_TOXIPROXY:-0}" == "1" ]]
}

toxiproxy_running() {
	local pid_file
	pid_file="$(toxiproxy_pid_file)"
	[[ -f "$pid_file" ]] && pid_is_running "$(cat "$pid_file")"
}

toxiproxy_api_ready() {
	curl --silent --show-error --fail "$(toxiproxy_api)/version" >/dev/null 2>&1
}

toxiproxy_ensure_proxy() {
	local name="$1" listen="$2" upstream="$3"
	if curl --silent --fail "$(toxiproxy_api)/proxies/$name" >/dev/null 2>&1; then
		return 0
	fi
	curl --silent --show-error --fail -X POST "$(toxiproxy_api)/proxies" \
		-H 'Content-Type: application/json' \
		-d "{\"name\":\"$name\",\"listen\":\"$listen\",\"upstream\":\"$upstream\",\"enabled\":true}" >/dev/null
}

toxiproxy_up() {
	if ! toxiproxy_available; then
		if toxiproxy_required; then
			roost_it_error "ROOST_IT_TOXIPROXY=1 but toxiproxy-server is not installed"
			return 1
		fi
		roost_it_log "toxiproxy-server not installed; network fault tests will skip"
		return 0
	fi
	local dir
	dir="$(toxiproxy_dir)"
	mkdir -p "$dir"
	if ! toxiproxy_running; then
		nohup toxiproxy-server -host 127.0.0.1 -port "$(toxiproxy_api_port)" >"$dir/toxiproxy.log" 2>&1 &
		echo $! > "$(toxiproxy_pid_file)"
	fi
	wait_until 15 "toxiproxy API" toxiproxy_api_ready
	local index
	for index in 1 2 3; do
		toxiproxy_ensure_proxy "nats-$index" "127.0.0.1:$(toxiproxy_proxy_port "$index")" "127.0.0.1:$(nats_client_port "$index")"
	done
	toxiproxy_ensure_proxy "redis" "127.0.0.1:$(toxiproxy_redis_port)" "$(redis_addr)"
	toxiproxy_heal
	roost_it_log "toxiproxy ready: $(toxiproxy_api), nats proxies 127.0.0.1:$(toxiproxy_proxy_port 1)-$(toxiproxy_proxy_port 3)"
}

# toxiproxy_heal removes every toxic and re-enables every proxy; `heal` calls
# it so a test that failed mid-fault leaves nothing behind for the next one.
toxiproxy_heal() {
	toxiproxy_running || return 0
	curl --silent --show-error --fail -X POST "$(toxiproxy_api)/reset" >/dev/null 2>&1 || true
}

toxiproxy_down() {
	local pid_file pid
	pid_file="$(toxiproxy_pid_file)"
	[[ -f "$pid_file" ]] || return 0
	pid="$(cat "$pid_file")"
	if pid_is_running "$pid"; then
		kill -TERM "$pid" 2>/dev/null || true
		wait_until 10 "toxiproxy to exit" bash -c "! kill -0 $pid 2>/dev/null" || true
	fi
	rm -f -- "$pid_file"
}

toxiproxy_status() {
	if toxiproxy_running && toxiproxy_api_ready; then
		roost_it_log "toxiproxy=RUNNING api=$(toxiproxy_api) proxies=$(curl --silent "$(toxiproxy_api)/proxies" | jq -r 'keys | join(",")')"
	else
		roost_it_log "toxiproxy=absent"
	fi
}

toxiproxy_redis_addr() { printf '127.0.0.1:%d\n' "$(toxiproxy_redis_port)"; }

toxiproxy_nats_url() {
	printf 'nats://127.0.0.1:%d,nats://127.0.0.1:%d,nats://127.0.0.1:%d\n' "$(toxiproxy_proxy_port 1)" "$(toxiproxy_proxy_port 2)" "$(toxiproxy_proxy_port 3)"
}
