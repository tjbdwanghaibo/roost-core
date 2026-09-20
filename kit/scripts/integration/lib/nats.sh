#!/usr/bin/env bash

nats_client_port() {
	printf '%d\n' "$((14221 + $1))"
}

nats_route_port() {
	printf '%d\n' "$((16221 + $1))"
}

nats_monitor_port() {
	printf '%d\n' "$((18221 + $1))"
}

nats_node_dir() {
	printf '%s/nats-%d\n' "$ROOST_IT_ROOT" "$1"
}

nats_node_pid_file() {
	printf '%s/nats.pid\n' "$(nats_node_dir "$1")"
}

nats_write_config() {
	local index="$1" node config client route monitor other
	node="$(nats_node_dir "$index")"
	config="$node/nats.conf"
	client="$(nats_client_port "$index")"
	route="$(nats_route_port "$index")"
	monitor="$(nats_monitor_port "$index")"
	mkdir -p "$node/store"
	{
		printf 'server_name: "nats-%d"\n' "$index"
		printf 'listen: "127.0.0.1:%d"\n' "$client"
		printf 'http: "127.0.0.1:%d"\n' "$monitor"
		printf 'jetstream {\n  store_dir: "%s/store"\n  max_mem_store: 268435456\n  max_file_store: 4294967296\n}\n' "$node"
		printf 'cluster {\n  name: "roost-it"\n  listen: "127.0.0.1:%d"\n  routes: [\n' "$route"
		for other in 1 2 3; do
			if [[ "$other" -ne "$index" ]]; then
				printf '    "nats-route://127.0.0.1:%d"\n' "$(nats_route_port "$other")"
			fi
		done
		printf '  ]\n}\n'
	} > "$config"
}

nats_varz_ready() {
	curl --silent --show-error --fail "http://127.0.0.1:$1/varz" 2>/dev/null | jq -e '.jetstream.config.store_dir != null' >/dev/null
}

nats_routes_ready() {
	curl --silent --show-error --fail "http://127.0.0.1:$1/routez" 2>/dev/null | jq -e '.num_routes >= 2' >/dev/null
}

# JetStream is enabled and routed before the meta group has elected a leader,
# and stream creation is refused until it has. A fixture that starts a Data
# Engine in that window waits its whole startup_timeout on "ensure effect
# stream" and fails before any fault is injected. Readiness therefore also
# requires every node to report a meta leader.
nats_meta_leader_ready() {
	curl --silent --show-error --fail "http://127.0.0.1:$1/jsz" 2>/dev/null | jq -e '(.meta_cluster.leader // "") | length > 0' >/dev/null
}

nats_start_node() {
	local index="$1" node pid_file config client monitor
	node="$(nats_node_dir "$index")"
	pid_file="$(nats_node_pid_file "$index")"
	config="$node/nats.conf"
	client="$(nats_client_port "$index")"
	monitor="$(nats_monitor_port "$index")"
	if read_owned_pid "$pid_file" >/dev/null 2>&1; then
		return 0
	fi
	require_port_available_or_owned "$client" "$pid_file" "nats-$index client"
	require_port_available_or_owned "$monitor" "$pid_file" "nats-$index monitor"
	nats_write_config "$index"
	rm -f -- "$pid_file"
	roost_it_log "starting nats-$index on 127.0.0.1:$client"
	nohup nats-server -c "$config" -P "$pid_file" </dev/null > "$node/nats.log" 2>&1 &
	wait_until 30 "nats-$index JetStream" nats_varz_ready "$monitor"
}

nats_cluster_ready() {
	local index monitor
	for index in 1 2 3; do
		monitor="$(nats_monitor_port "$index")"
		nats_varz_ready "$monitor" || return 1
		nats_routes_ready "$monitor" || return 1
		nats_meta_leader_ready "$monitor" || return 1
	done
}

nats_up() {
	local index
	for index in 1 2 3; do
		nats_start_node "$index"
	done
	wait_until 45 "NATS JetStream cluster ready" nats_cluster_ready
}

nats_down() {
	local index
	for index in 3 2 1; do
		stop_owned_pid "$(nats_node_pid_file "$index")" "nats-$index"
	done
}

nats_heal() {
	nats_up
}

nats_status() {
	if ! nats_cluster_ready; then
		roost_it_error "NATS JetStream cluster is not ready"
		return 1
	fi
	local index monitor name routes leader
	for index in 1 2 3; do
		monitor="$(nats_monitor_port "$index")"
		name="$(curl --silent --fail "http://127.0.0.1:$monitor/varz" | jq -r '.server_name')"
		routes="$(curl --silent --fail "http://127.0.0.1:$monitor/routez" | jq -r '.num_routes')"
		leader="$(curl --silent --fail "http://127.0.0.1:$monitor/jsz" | jq -r '.meta_cluster.leader // "none"')"
		printf '%s=JETSTREAM routes=%s meta_leader=%s\n' "$name" "$routes" "$leader"
	done
}

nats_stream_leader_node() {
	local stream="$1" monitor leader
	for monitor in 18222 18223 18224; do
		leader="$(curl --silent --fail "http://127.0.0.1:$monitor/jsz?streams=true" 2>/dev/null | jq -r --arg stream "$stream" '
          [.account_details[]?.stream_detail[]? | select(.name == $stream) | .cluster.leader][0] // empty
        ' 2>/dev/null || true)"
		if [[ "$leader" == nats-* ]]; then
			printf '%s\n' "$leader"
			return 0
		fi
	done
	return 1
}

nats_fault_leader() {
	local stream="$1" node index
	node="$(nats_stream_leader_node "$stream")"
	index="${node#nats-}"
	stop_owned_pid "$(nats_node_pid_file "$index")" "$node"
	printf '%s\n' "$node"
}

nats_fault_all() {
	nats_down
}
