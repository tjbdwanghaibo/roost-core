#!/usr/bin/env bash
# 开房间 TS SDK（client/ts，@roost/relay-client）：编解码金样 relay-packets.json、换 host、Web 端房主辅助，以及对真实
# relay + lobby 的用例（对端是 go test -c ./client/relay 的测试二进制；没有 ROOST_REDIS_TEST_ADDR /
# ROOST_DATAENGINE_IT_REDIS_ADDR 时房间目录用内存）。
# 没有 node / npm 时明确打印 SKIP 并以 0 退出；设 ROOST_REQUIRE_NODE=1 时缺 node 以 1 退出（CI 用）。
# 依赖按 package-lock.json 锁定版本：node_modules 不在时执行 npm ci（需要网络）。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir/client/ts"

if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
  if [[ "${ROOST_REQUIRE_NODE:-}" == "1" ]]; then
    echo "test-client-ts: node / npm not found and ROOST_REQUIRE_NODE=1" >&2
    exit 1
  fi
  echo "test-client-ts: SKIP TS client SDK tests: node / npm not found; this is not a TS pass" >&2
  exit 0
fi

if [[ ! -d node_modules ]]; then
  npm ci --no-audit --no-fund
fi
echo "test-client-ts: build + tests ($(node --version))"
npm run build
npm test
