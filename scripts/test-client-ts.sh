#!/usr/bin/env bash
# TS SDK（client/ts，@roost/client）：
#   - npm test：开房间（relay 子路径）的编解码金样 relay-packets.json、换 host、Web 端房主辅助与对真实 relay + lobby 的
#     用例（对端是 go test -c ./client/relay 的测试二进制；没有 ROOST_REDIS_TEST_ADDR / ROOST_DATAENGINE_IT_REDIS_ADDR
#     时房间目录用内存），以及连玩家接入（gate 子路径）的 RS v2 金样、对假玩家接入的连接 / 心跳 / 超时 / 重连 / 请求 /
#     推送 / 校时用例、protobuf 子路径用例；
#   - Go 集成：TS 客户端经 WebSocket 连真实玩家接入，直连与经独立 Gate 两项（infra/network/gateway，
#     ROOST_TS_GATE_DRIVER 指向 test/gate-driver.mjs）；
#   - 完整模式 ROOST_TS_FULL=1 另跑真实工具链：用 protoc 31.1（ROOST_PROTOC）+ node_modules 里的 protoc-gen-es 重新生成
#     test/gen/dao_sync_pb.ts 并与提交的逐字节比较，再跑 roost generate 的 ts 导出 + tsc 编译用例。
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

echo "test-client-ts: TS client against the real player access (WebSocket, direct and through a standalone Gate)"
cd "$repo_dir"
ROOST_TS_GATE_DRIVER="$repo_dir/client/ts/test/gate-driver.mjs" GOWORK=off \
  go test -count=1 -run '^TestTheTSClientWorksOverWebSocket' ./infra/network/gateway

if [[ "${ROOST_TS_FULL:-}" == "1" ]]; then
  : "${ROOST_PROTOC:?ROOST_TS_FULL=1 needs ROOST_PROTOC pointing at protoc 31.1}"
  echo "test-client-ts: real toolchain (protoc $("$ROOST_PROTOC" --version), protoc-gen-es from node_modules)"
  gen_dir="$(mktemp -d)"
  trap 'rm -rf "$gen_dir"' EXIT
  "$ROOST_PROTOC" -I codegen/internal/dao/testdata/golden \
    --plugin=protoc-gen-es="$repo_dir/client/ts/node_modules/.bin/protoc-gen-es" \
    --es_out="$gen_dir" --es_opt=target=ts,import_extension=js dao_sync.proto
  cmp "$gen_dir/dao_sync_pb.ts" client/ts/test/gen/dao_sync_pb.ts ||
    { echo "test-client-ts: client/ts/test/gen/dao_sync_pb.ts is stale; regenerate it with the command above" >&2; exit 1; }
  ROOST_TS_FULL=1 GOWORK=off go test -count=1 -run '^TestClientExportTSWithTheRealToolchain$' ./codegen/internal/roost
fi
