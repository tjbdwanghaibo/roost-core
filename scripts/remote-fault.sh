#!/usr/bin/env bash
# 只控制固定隔离环境中的进程；PID 所有权校验复用正式集成环境脚本。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$repo_dir/kit/scripts/integration/lib/common.sh"
source "$repo_dir/kit/scripts/integration/lib/mongo.sh"
source "$repo_dir/kit/scripts/integration/lib/nats.sh"
require_safe_root
[[ "${ROOST_DATAENGINE_IT:-}" == 1 ]] || { echo 'isolated test environment required' >&2; exit 2; }
case "${1:-}" in
  heal) mongo_heal; nats_heal ;;
  mongo-primary) mongo_fault_primary ;;
  mongo-majority) stop_owned_pid "$(mongo_node_pid_file 1)" mongo-1; stop_owned_pid "$(mongo_node_pid_file 2)" mongo-2 ;;
  nats-node) stop_owned_pid "$(nats_node_pid_file 1)" nats-1 ;;
  nats-all) nats_down ;;
  *) echo 'Expected heal, mongo-primary, mongo-majority, nats-node or nats-all' >&2; exit 2 ;;
esac
