#!/usr/bin/env bash

# The fixed test root. It is deliberately not configurable (see
# require_safe_root: the fault commands kill processes under it), but the one
# fixed path differs per platform — /tmp is /private/tmp on macOS and stays
# /tmp on Linux, where /private cannot even be created. Hardcoding the macOS
# form made the whole suite unrunnable on a Linux CI runner. `cd /tmp && pwd -P`
# resolves the symlink on macOS and is a no-op on Linux.
readonly ROOST_IT_CANONICAL_ROOT="$(cd /tmp && pwd -P)/roost-dataengine-it"
# Go build cache for the suite, beside the test root: same platform rule.
readonly ROOST_IT_GO_CACHE_DEFAULT="$(dirname "$ROOST_IT_CANONICAL_ROOT")/roost-go-cache"
readonly ROOST_IT_ROOT="${ROOST_DATAENGINE_IT_ROOT:-$ROOST_IT_CANONICAL_ROOT}"

roost_it_log() {
	printf '[roost-it] %s\n' "$*"
}

roost_it_error() {
	printf '[roost-it] %s\n' "$*" >&2
}

require_safe_root() {
	if [[ "$ROOST_IT_ROOT" != "$ROOST_IT_CANONICAL_ROOT" ]]; then
		roost_it_error "refuse unsafe root: $ROOST_IT_ROOT"
		return 2
	fi
}

require_commands() {
	local missing=0 command_name
	for command_name in "$@"; do
		if ! command -v "$command_name" >/dev/null 2>&1; then
			roost_it_error "required command is missing: $command_name"
			missing=1
		fi
	done
	return "$missing"
}

wait_until() {
	local timeout="$1" description="$2"
	shift 2
	local deadline=$((SECONDS + timeout))
	until "$@"; do
		if (( SECONDS >= deadline )); then
			roost_it_error "timeout waiting for $description"
			return 1
		fi
		sleep 0.2
	done
}

pid_is_running() {
	local pid="$1"
	[[ "$pid" =~ ^[0-9]+$ ]] && kill -0 "$pid" >/dev/null 2>&1
}

pid_command() {
	ps -p "$1" -o command= 2>/dev/null || true
}

read_owned_pid() {
	local pid_file="$1"
	[[ -f "$pid_file" ]] || return 1
	local pid command_line
	pid="$(tr -d '[:space:]' < "$pid_file")"
	pid_is_running "$pid" || return 1
	command_line="$(pid_command "$pid")"
	[[ "$command_line" == *"$ROOST_IT_ROOT"* ]] || {
		roost_it_error "refuse foreign pid $pid from $pid_file: $command_line"
		return 2
	}
	printf '%s\n' "$pid"
}

stop_owned_pid() {
	local pid_file="$1" label="$2"
	local pid status=0
	pid="$(read_owned_pid "$pid_file")" || status=$?
	if [[ "$status" -eq 1 ]]; then
		rm -f -- "$pid_file"
		return 0
	fi
	if [[ "$status" -ne 0 ]]; then
		return "$status"
	fi
	roost_it_log "stopping $label (pid $pid)"
	kill -TERM "$pid"
	local deadline=$((SECONDS + 25))
	while pid_is_running "$pid"; do
		if (( SECONDS >= deadline )); then
			roost_it_error "$label did not stop after 25s"
			return 1
		fi
		sleep 0.2
	done
	rm -f -- "$pid_file"
}

port_is_listening() {
	nc -z -w 1 127.0.0.1 "$1" >/dev/null 2>&1
}

require_port_available_or_owned() {
	local port="$1" pid_file="$2" label="$3"
	if ! port_is_listening "$port"; then
		return 0
	fi
	if read_owned_pid "$pid_file" >/dev/null 2>&1; then
		return 0
	fi
	roost_it_error "$label port $port is occupied by a process outside $ROOST_IT_ROOT"
	return 1
}

write_environment_file() {
	mkdir -p "$ROOST_IT_ROOT"
	local output="$ROOST_IT_ROOT/env.sh" temporary="$ROOST_IT_ROOT/env.sh.tmp"
	{
		printf 'export ROOST_DATAENGINE_IT=1\n'
		printf 'export ROOST_DATAENGINE_IT_ROOT=%q\n' "$ROOST_IT_ROOT"
		printf 'export ROOST_DATAENGINE_IT_MONGO_URI=%q\n' 'mongodb://127.0.0.1:27117,127.0.0.1:27118,127.0.0.1:27119/?replicaSet=roost-it'
		printf 'export ROOST_DATAENGINE_IT_NATS_URL=%q\n' 'nats://127.0.0.1:14222,nats://127.0.0.1:14223,nats://127.0.0.1:14224'
		printf 'export ROOST_DATAENGINE_IT_REDIS_ADDR=%q\n' "$(redis_addr)"
		if toxiproxy_running; then
			printf 'export ROOST_DATAENGINE_IT_TOXIPROXY_URL=%q\n' "$(toxiproxy_api)"
			printf 'export ROOST_DATAENGINE_IT_NATS_PROXIED_URL=%q\n' "$(toxiproxy_nats_url)"
			printf 'export ROOST_DATAENGINE_IT_REDIS_PROXIED_ADDR=%q\n' "$(toxiproxy_redis_addr)"
		fi
	} > "$temporary"
	mv -f -- "$temporary" "$output"
}
