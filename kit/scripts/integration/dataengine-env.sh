#!/usr/bin/env bash
set -euo pipefail

script_path="${BASH_SOURCE[0]}"
repo_root="$(cd "$(dirname "$script_path")/../.." && pwd)"

# shellcheck source=lib/common.sh
source "$repo_root/scripts/integration/lib/common.sh"
# shellcheck source=lib/mongo.sh
source "$repo_root/scripts/integration/lib/mongo.sh"
# shellcheck source=lib/nats.sh
source "$repo_root/scripts/integration/lib/nats.sh"
# shellcheck source=lib/redis.sh
source "$repo_root/scripts/integration/lib/redis.sh"
# shellcheck source=lib/toxiproxy.sh
source "$repo_root/scripts/integration/lib/toxiproxy.sh"

usage() {
	cat <<'USAGE'
Usage: scripts/integration/dataengine-env.sh COMMAND [ARGS]

Commands:
  up                         Start and initialize all isolated nodes
  down                       Stop isolated nodes and preserve their data
  status                     Show Mongo, NATS, Redis and toxiproxy status
  reset                      Stop nodes and delete only the fixed test root
  test                       Start nodes and run integration-tagged tests
  fault mongo-primary        Stop the current isolated Mongo primary
  fault nats-leader STREAM   Stop the leader for an isolated JetStream stream
  fault nats-all             Stop all isolated NATS nodes
  heal                       Restart missing nodes, clear toxics, wait for full health

Network faults: when toxiproxy-server is installed, up also starts toxiproxy
with one proxy per NATS node and exports ROOST_DATAENGINE_IT_TOXIPROXY_URL and
ROOST_DATAENGINE_IT_NATS_PROXIED_URL; the Toxic* integration tests use them.
ROOST_IT_TOXIPROXY=1 makes toxiproxy mandatory (the nightly fault matrix).
USAGE
}

preflight() {
	require_safe_root
	require_commands mongod mongosh nats-server redis-server redis-cli curl jq nc ps
}

environment_up() {
	preflight
	mkdir -p "$ROOST_IT_ROOT"
	mongo_up
	nats_up
	redis_up
	toxiproxy_up
	write_environment_file
}

environment_status() {
	require_safe_root
	local status=0
	mongo_status || status=1
	nats_status || status=1
	redis_status || status=1
	toxiproxy_status
	return "$status"
}

environment_down() {
	require_safe_root
	toxiproxy_down
	redis_down
	nats_down
	mongo_down
}

environment_reset() {
	require_safe_root
	environment_down
	if [[ "$ROOST_IT_ROOT" != "$ROOST_IT_CANONICAL_ROOT" ]]; then
		roost_it_error "refuse unsafe root: $ROOST_IT_ROOT"
		return 2
	fi
	roost_it_log "removing $ROOST_IT_ROOT"
	rm -rf -- "$ROOST_IT_CANONICAL_ROOT"
}

environment_fault() {
	preflight
	case "${1:-}" in
		mongo-primary)
			mongo_fault_primary
			;;
		nats-leader)
			[[ -n "${2:-}" ]] || { roost_it_error "fault nats-leader requires a stream name"; return 2; }
			nats_fault_leader "$2"
			;;
		nats-all)
			nats_fault_all
			;;
		*)
			roost_it_error "unknown fault target: ${1:-}"
			return 2
			;;
	esac
}

environment_heal() {
	preflight
	mongo_heal
	nats_heal
	redis_up
	toxiproxy_heal
	write_environment_file
}

environment_test() {
	environment_up
	# shellcheck disable=SC1091
	source "$ROOST_IT_ROOT/env.sh"
	(
		cd "$repo_root"
		GOCACHE="${GOCACHE:-$ROOST_IT_GO_CACHE_DEFAULT}" \
			go test -tags=integration ./dataengine ./saga ./remoteentity ./nats -count=1
	)
	# The Redis fault suites moved to roost-core with the redis client
	# (consolidation P2-②); run them against the same environment when a
	# core checkout is at hand (sibling directory by default, ROOST_CORE_DIR
	# to override). Absent checkout = skipped loudly, never silently green.
	# etcd/driver and mongo/driver carry the real-service guard tests
	# (U-0153): mongo/driver reads ROOST_DATAENGINE_IT_MONGO_URI from env.sh
	# and spawns a standalone mongod itself; etcd/driver spawns etcd from PATH
	# and skips loudly when the binary is absent.
	local core_dir="${ROOST_CORE_DIR:-$repo_root/../roost-core}"
	if [[ -f "$core_dir/go.mod" ]]; then
		(
			cd "$core_dir"
			ROOST_REDIS_TEST_ADDR="$ROOST_DATAENGINE_IT_REDIS_ADDR" \
				GOCACHE="${GOCACHE:-$ROOST_IT_GO_CACHE_DEFAULT}" \
				go test -tags=integration ./redis/... ./etcd/driver ./mongo/driver -count=1
		)
	else
		echo "[roost-it] roost-core checkout not found at $core_dir; core redis fault suites NOT run" >&2
	fi
}

case "${1:-}" in
	up)
		environment_up
		environment_status
		;;
	down)
		environment_down
		;;
	status)
		environment_status
		;;
	reset)
		environment_reset
		;;
	test)
		environment_test
		;;
	fault)
		shift
		environment_fault "$@"
		;;
	heal)
		environment_heal
		environment_status
		;;
	-h|--help|help)
		usage
		;;
	*)
		usage >&2
		exit 2
		;;
esac
