#!/usr/bin/env bash
# 所有共用集群的故障串行执行；每格保留独立日志和结果，不把缺失/跳过计为通过。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${ROOST_DATAENGINE_IT_ROOT:?source the isolated environment env.sh}"
lock="$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock"
mkdir "$lock" || { echo 'Another Remote acceptance run owns the isolated environment' >&2; exit 2; }
trap 'rmdir "$lock"' EXIT
label="${ROOST_REMOTE_MATRIX_LABEL:-matrix-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
output="$repo_dir/artifacts/perf/remote/$label"
[[ ! -e "$output" ]] || exit 2
mkdir -p "$output"
export GOWORK=off ROOST_IT_TOXIPROXY=1
cd "$repo_dir"
{ go version; git rev-parse HEAD; shasum -a 256 codegen/internal/entity/testdata/remoteflow/*.go remoteentity/*.go nest/*.go dataengine/engine/*.go redis/driver/cluster_recovery.go; } > "$output/env.txt"
bash kit/scripts/integration/dataengine-env.sh heal > "$output/preflight.log" 2>&1
status=0
run_case() {
 local name="$1";shift
 local code=0
 "$@" > "$output/$name.log" 2>&1 || code=$?
 case "$name" in
  business-*|broker-*)
   if ! bash kit/scripts/integration/dataengine-env.sh heal > "$output/$name-heal.log" 2>&1; then
    printf '%s\tFAIL(environment recovery)\n' "$name" | tee -a "$output/results.tsv"
    exit 1
   fi ;;
 esac
 if [[ "$code" == 0 ]] && ! grep -q -- '--- SKIP:' "$output/$name.log"; then
  printf '%s\tPASS\n' "$name" | tee -a "$output/results.tsv"
 else
  printf '%s\tFAIL(exit=%s)\n' "$name" "$code" | tee -a "$output/results.tsv";status=1
 fi
}
for fault in mongo-primary mongo-majority nats-node nats-all; do
 for policy in async strict pipelined; do
  run_case "business-$fault-$policy" env ROOST_REMOTE_FAULT="$fault" ROOST_REMOTE_POLICY="$policy" ROOST_REMOTE_TIMEOUT=240s bash scripts/test-remote-generated.sh
 done
done
run_case lease-process go test -race -tags=integration ./remoteentity -run '^TestRealRemote(ProcessKillRecoversLease|ProcessPartitionFencesOldOwner|ColdAdmissionDeadlineUnderLatency)$' -count=1 -timeout=90s -v
run_case redis-cluster env ROOST_REMOTE_CLUSTER_IT=1 go test -race -tags=integration ./remoteentity -run '^TestRealRemoteRedisClusterFailover$' -count=1 -timeout=150s -v
run_case redis-unreplicated-fence env ROOST_REMOTE_CLUSTER_IT=1 go test -race -tags=integration ./remoteentity -run '^TestRealRemoteRedisClusterUnreplicatedFence$' -count=1 -timeout=120s -v
run_case durable-process go test -race -tags=integration ./remoteentity -run '^TestRealRemoteDurableAuthorityProcessPartition$' -count=1 -timeout=90s -v
run_case ownership-counters go test -race -tags=integration ./remoteentity -run '^TestReal(VersionedLock|RemoteOwnershipClaim)' -count=1 -timeout=90s -v
run_case mongo-wal-recovery go test -race -tags=integration ./kit/dataengine -run '^TestRealDataEngineRemotePublicationAndWALRecovery$' -count=1 -timeout=120s -v
run_case broker-failover go test -race -tags=integration ./kit/dataengine -run '^TestReal(MongoPrimaryFailover|NATSOutage|JetStreamLeaderFailover)' -count=1 -timeout=240s -v
run_case broker-network go test -race -tags=integration ./kit/dataengine -run '^TestToxicNATS' -count=1 -timeout=180s -v
run_case final-health bash kit/scripts/integration/dataengine-env.sh status
printf 'Matrix results: %s\n' "$output"
exit "$status"
