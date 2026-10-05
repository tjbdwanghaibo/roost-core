#!/usr/bin/env bash
# NC-201：在 source 过 env.sh 的 shell 里跑 redis-cluster-suites.sh，go test 是否还拿到单机 / 全环境准入变量。
set -uo pipefail
src="$1"
bin="$(mktemp -d "${TMPDIR:-/tmp}/n15-cluster.XXXXXX")"
trap 'rm -rf "$bin"' EXIT
cat > "$bin/go" <<'G'
#!/bin/sh
echo "stub go $*"
echo "ROOST_DATAENGINE_IT=${ROOST_DATAENGINE_IT:-<unset>}"
echo "REDIS_ADDR=${REDIS_ADDR:-<unset>}"
echo "ROOST_REVIEW_CLUSTER=${ROOST_REVIEW_CLUSTER:-<unset>}"
G
chmod +x "$bin/go"
# 模拟 source 过隔离环境 env.sh 的 shell（只放准入变量，不含任何真实地址）
PATH="$bin:$PATH" ROOST_DATAENGINE_IT=1 REDIS_ADDR=127.0.0.1:1 ROOST_REVIEW_CLUSTER=127.0.0.1:2 \
  bash "$src/kit/scripts/integration/redis-cluster-suites.sh"
echo "exit=$?"
