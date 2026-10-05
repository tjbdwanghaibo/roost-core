#!/usr/bin/env bash
# NC-208 补修（kit/dataengine）：三条 TestToxicNATS* 只能清理自己建的代理 / 毒，不能 /reset 同一 toxiproxy 上别人的毒。
# toxiproxy 用本脚本调用方自起的那个（$api，随机端口），不碰共享隔离环境的 toxiproxy；
# Mongo / NATS 用共享隔离环境的直连地址（每个 fixture 自用唯一库名与流名）。
# 用法：toxreset_dataengine.sh <源码目录> <toxiproxy API>
set -uo pipefail
src="$1"; api="$2"
cd "$src"
test -e "$HOME/.roost-it/roost-dataengine-it/remote-acceptance.lock" && { echo "remote-acceptance.lock present, abort"; exit 2; }
# shellcheck disable=SC1091
source "$HOME/.roost-it/roost-dataengine-it/env.sh" >/dev/null 2>&1
mk() { curl -s -X POST "$api/proxies" -H 'Content-Type: application/json' -d "$1" | jq -r .listen; }
# 旧用例依赖环境脚本建的共享代理名 nats-1..3：在自起的 toxiproxy 上按同名建好，上游是共享 NATS 的三个直连节点
proxied=()
index=0
for upstream in ${ROOST_DATAENGINE_IT_NATS_URL//,/ }; do
  index=$((index + 1))
  listen="$(mk "{\"name\":\"nats-$index\",\"listen\":\"127.0.0.1:0\",\"upstream\":\"${upstream#nats://}\",\"enabled\":true}")"
  proxied+=("nats://$listen")
done
# 别人的代理和毒（另一会话正在注入的故障）
mk '{"name":"bystander-nc208b","listen":"127.0.0.1:0","upstream":"127.0.0.1:9","enabled":true}' >/dev/null
addbystander() { curl -s -o /dev/null -X POST "$api/proxies/bystander-nc208b/toxics" -H 'Content-Type: application/json' -d '{"name":"someone-elses-latency","type":"latency","stream":"downstream","attributes":{"latency":50}}'; }
addbystander
check() { printf '%s: bystander toxics=%s proxies=%s\n' "$1" "$(curl -s "$api/proxies/bystander-nc208b/toxics" | jq -c '[.[].name]')" "$(curl -s "$api/proxies" | jq -c 'keys')"; }
proxied_url="$(IFS=,; echo "${proxied[*]}")"
check before
for test in TestToxicNATSLatencyKeepsTheCommitOnTheDurablePath TestToxicNATSConnectionResetDeliversTheEffectExactlyOnce TestToxicNATSHalfOpenAckLossIsBoundedAndDeliversExactlyOnce; do
  env -u ROOST_IT_TOXIPROXY GOWORK=off \
    ROOST_DATAENGINE_IT_TOXIPROXY_URL="$api" ROOST_DATAENGINE_IT_NATS_PROXIED_URL="$proxied_url" \
    go test -tags=integration -count=1 -run "^${test}\$" ./kit/dataengine 2>&1 | tail -3
  check "after $test"
  addbystander
done
# 收尾：删掉本脚本建的代理
for n in nats-1 nats-2 nats-3 bystander-nc208b; do curl -s -o /dev/null -X DELETE "$api/proxies/$n"; done
check cleanup
