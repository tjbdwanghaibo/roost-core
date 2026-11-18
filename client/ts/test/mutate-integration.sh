#!/usr/bin/env bash
# 变异核对（证据用，不进 npm test）：把 dist/gate 里被测的一段逻辑改坏，跑 Go 驱动的真实玩家接入用例
# （TestTheTSClientWorksOverWebSocket*），要求变红；每条跑完 npm run build 恢复 dist。在 client/ts 下运行。
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
repo="$(cd ../.. && pwd)"
driver="$PWD/test/gate-driver.mjs"
npm run build >/dev/null

mutate() { # id 说明 文件 原文 替换
  local id=$1 what=$2 file=$3 from=$4 to=$5
  node -e '
    const fs = require("fs"); const [file, from, to] = process.argv.slice(1);
    const text = fs.readFileSync(file, "utf8");
    if (!text.includes(from)) { console.error("pattern not found in " + file); process.exit(3); }
    fs.writeFileSync(file, text.replace(from, to));' "$file" "$from" "$to" || return 1
  local out
  out=$(cd "$repo" && ROOST_TS_GATE_DRIVER="$driver" GOWORK=off go test -count=1 -run '^TestTheTSClientWorksOverWebSocket' ./infra/network/gateway 2>&1)
  local code=$?
  npm run build >/dev/null
  if [[ $code -eq 0 ]]; then echo "$id SURVIVED (still green): $what"; return 1; fi
  echo "$id RED: $what"
  echo "$out" | grep -E -- "--- FAIL|FAIL websocket|tcp_transport_test.go:[0-9]+: [a-z-]+:? .*(generations|want)" | sed 's/^/    /' | head -6
}

survivors=0
mutate G1 "连接代号不递增（每次都是同一个值）" dist/gate/auth.js \
  "const next = lastGeneration + 1n > wall ? lastGeneration + 1n : wall;" "const next = 1n; void wall;" || survivors=$((survivors + 1))
mutate G2 "不发心跳（间隔计时器从不安排）" dist/gate/connection.js \
  "if (this.opts.heartbeatIntervalMs === 0)" "if (true)" || survivors=$((survivors + 1))
mutate G3 "断线后不自动重连" dist/gate/connection.js \
  "if (!this.opts.autoReconnect) {" "if (true) {" || survivors=$((survivors + 1))
mutate G4 "请求不计超时" dist/gate/connection.js \
  "this.readySession().request(messageId, responseId, payload, complete, this.opts.requestTimeoutMs, signal);" \
  "this.readySession().request(messageId, responseId, payload, complete, undefined, signal);" || survivors=$((survivors + 1))
exit $survivors
