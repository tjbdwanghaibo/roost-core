#!/usr/bin/env bash
# 第三类批次 2 第 2 项：TestAllianceE2ERPCConcurrency 128 / 256 并发交替，私有 mirror-local（偏移 21000）。
# 用法：item2.sh <输出目录> <轮数> <bg 值列表，如 "1" 或 "1 0">
set -uo pipefail
repo=/Users/whb/roost/roost-core/.claude/worktrees/agent-acb3c435f4be2cf12
out="$1"; rounds="$2"; bgs="$3"
mkdir -p "$out"
export ROOST_MIRROR_LOCAL_HOME="$out/mirror" ROOST_MIRROR_LOCAL_OFFSET=21000 GOWORK=off
host() { date -u '+%Y-%m-%dT%H:%M:%SZ'; uptime; sysctl vm.swapusage; df -h / | tail -1; }
cleanup() { bash "$repo/scripts/mirror-local.sh" clean >>"$out/mirror-clean.log" 2>&1; }
trap cleanup EXIT
host >"$out/host-start.txt"
bash "$repo/scripts/mirror-local.sh" up >"$out/mirror-up.log" 2>&1 || { echo "mirror up failed"; exit 1; }
source "$out/mirror/roost-dataengine-it/env.sh"
for r in $(seq 1 "$rounds"); do
  for bg in $bgs; do
    for c in 128 256; do
      label="r$r-c$c-bg$bg"
      mkdir -p "$out/$label/logs"
      host >"$out/$label/host-before.txt"
      (cd "$repo" && ALLIANCE_E2E_PERF=1 ALLIANCE_E2E_BACKGROUND_CHECKPOINT="$bg" ALLIANCE_E2E_STACKS=1 \
        ALLIANCE_E2E_RPC_CONCURRENCY="$c" ALLIANCE_E2E_RUN='^TestAllianceE2ERPCConcurrency$' \
        ALLIANCE_E2E_LOGS="$out/$label/logs" bash scripts/test-alliance-e2e.sh) >"$out/$label/console.log" 2>&1
      echo "$label exit=$?" | tee -a "$out/runs.txt"
      host >"$out/$label/host-after.txt"
    done
  done
done
host >"$out/host-end.txt"
echo ITEM2-DONE
