#!/usr/bin/env bash
set -euo pipefail

script_path="${BASH_SOURCE[0]}"
repo_root="$(cd "$(dirname "$script_path")/../.." && pwd)"
# kit/ is a directory of the roost-core module now; go test runs from its root.
module_root="$(cd "$repo_root/.." && pwd)"

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
  reset                      Stop nodes and delete only the canonical test root
  test                       Start nodes and run integration-tagged tests
  fault mongo-primary        Stop the current isolated Mongo primary
  fault nats-leader STREAM   Stop the leader for an isolated JetStream stream
  fault nats-all             Stop all isolated NATS nodes
  heal                       Restart missing nodes, clear toxics, wait for full health

Every command except status holds ROOT/remote-acceptance.lock while it runs and
refuses (exit 2) while another run holds it (another command, scripts/test-remote-matrix.sh,
scripts/perf/remote.sh, a running `test`); the holder exports
ROOST_REMOTE_ACCEPTANCE_LOCK_HELD so its own children reuse the lock.

Network faults: when toxiproxy-server is installed, up also starts toxiproxy
with one proxy per NATS node and exports ROOST_DATAENGINE_IT_TOXIPROXY_URL and
ROOST_DATAENGINE_IT_NATS_PROXIED_URL; the Toxic* integration tests use them.
ROOST_IT_TOXIPROXY=1 makes toxiproxy mandatory (the nightly fault matrix).

Location and ports (defaults keep the historical layout):
  ROOST_IT_HOME=DIR          Existing absolute parent directory; the test root is
                             DIR/roost-dataengine-it (default DIR=/tmp). Prefer a
                             directory outside /tmp for a long-lived environment:
                             macOS purges /tmp files that are not accessed for days.
  ROOST_IT_PORT_OFFSET=N     Added to every port (default 0). A root remembers the
                             offset it was created with and refuses another one.
The generated env.sh exports both, so a shell that sourced it drives the same
environment. See scripts/integration/README.md.
USAGE
}

preflight() {
	require_safe_root
	acquire_acceptance_lock
	require_commands mongod mongosh nats-server redis-server redis-cli curl jq nc ps
}

environment_up() {
	# 先建根并记录偏移，再取锁（锁目录在根下面）；记录偏移只在第一次写，之后只校验。
	require_safe_root
	mkdir -p "$ROOST_IT_ROOT"
	record_port_offset
	preflight
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
	acquire_acceptance_lock
	toxiproxy_down
	redis_down
	nats_down
	mongo_down
}

environment_reset() {
	require_safe_root
	acquire_acceptance_lock
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
	# 整套 integration 含故障用例（停节点、给代理加毒），运行期间持有验收锁，与矩阵 / 长稳互斥。
	require_safe_root
	mkdir -p "$ROOST_IT_ROOT"
	record_port_offset
	acquire_acceptance_lock
	environment_up
	# shellcheck disable=SC1091
	source "$ROOST_IT_ROOT/env.sh"
	# One module since the consolidation (2026-09-21): the assembly-side
	# suites under kit/ and the driver / redis fault suites that moved to core
	# with their implementations are all in $module_root. The old version of
	# this function ran `./dataengine ./saga ./remoteentity ./nats` from kit/
	# — two of those directories had no test files left, so the matrix
	# reported two green cells for nothing — and then looked for the core
	# suites in a sibling `../roost-core` checkout that no longer exists,
	# printed "NOT run" and exited 0 (RR-20260922-02).
	#
	# The package list is pinned to the files on disk by
	# integration_coverage_promises_test.go: every directory with a
	# `//go:build integration` test file that keys on ROOST_DATAENGINE_IT,
	# and nothing else. etcd/driver spawns etcd from PATH and skips loudly
	# when the binary is absent; mongo/driver spawns its own standalone mongod
	# (U-0153).
	# Remote 也依赖同一隔离环境；故障套件按包串行，避免同时改动共享服务。
	(
		cd "$module_root"
		GOCACHE="${GOCACHE:-$ROOST_IT_GO_CACHE_DEFAULT}" \
			go test -tags=integration -p 1 ./dataengine/engine ./failurelog ./kit/dataengine ./kit/mongo ./kit/nats ./kit/syncbus ./remoteentity ./saga ./redis/... ./versionstore ./etcd/driver ./mongo/driver -count=1
	)
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
