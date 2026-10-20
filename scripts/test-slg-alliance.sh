#!/usr/bin/env bash
# SLG 联盟活动联合教学：正式生成 RPC、真实 Redis/NATS、协调器子进程重启。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?set isolated Redis address}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?set isolated NATS URL}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
log_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-slg-alliance.XXXXXX")"
echo "SLG alliance logs: $log_root"
cd "$repo_root"
export GOWORK=off
go test -race -count=1 -timeout 90s -v -run '^TestSLGAllianceActivity$' ./scripts/testdata/slg-alliance | tee "$log_root/test.log"
if [[ "$(grep -c '^--- PASS: TestSLGAllianceActivity ' "$log_root/test.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$log_root/test.log"; then
  echo 'expected one executed passing SLG alliance scenario' >&2
  exit 1
fi
go vet ./scripts/testdata/slg-alliance
