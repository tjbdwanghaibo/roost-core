#!/usr/bin/env bash
# NC-208：toxic 用例只能清理自己建的代理 / 毒，不能 /reset 同一 toxiproxy 上别人的毒。
# 全部指向本 agent 自起的 toxiproxy(39474) / redis(39379) / nats(39222)，不碰共享隔离环境。
set -uo pipefail
src="$1"; shift
api=http://127.0.0.1:39474
cd "$src"
mk() { curl -s -o /dev/null -X POST "$api/proxies" -H 'Content-Type: application/json' -d "$1"; }
# 旧用例依赖环境脚本建的共享代理名：redis、nats-1..3
mk '{"name":"redis","listen":"127.0.0.1:39600","upstream":"127.0.0.1:39379","enabled":true}'
for i in 1 2 3; do mk "{\"name\":\"nats-$i\",\"listen\":\"127.0.0.1:3961$i\",\"upstream\":\"127.0.0.1:39222\",\"enabled\":true}"; done
# 别人的代理和毒（另一会话正在注入的故障）
mk '{"name":"bystander-n15","listen":"127.0.0.1:39601","upstream":"127.0.0.1:39379","enabled":true}'
curl -s -o /dev/null -X POST "$api/proxies/bystander-n15/toxics" -H 'Content-Type: application/json' -d '{"name":"someone-elses-latency","type":"latency","stream":"downstream","attributes":{"latency":50}}'
common=(env -u ROOST_IT_TOXIPROXY GOWORK=off ROOST_DATAENGINE_IT=1
  ROOST_DATAENGINE_IT_TOXIPROXY_URL=$api
  ROOST_DATAENGINE_IT_REDIS_ADDR=127.0.0.1:39379 ROOST_DATAENGINE_IT_REDIS_PROXIED_ADDR=127.0.0.1:39600
  ROOST_DATAENGINE_IT_NATS_URL=nats://127.0.0.1:39222
  ROOST_DATAENGINE_IT_NATS_PROXIED_URL=nats://127.0.0.1:39611,nats://127.0.0.1:39612,nats://127.0.0.1:39613)
check() { printf '%s: bystander toxics=%s proxies=%s\n' "$1" "$(curl -s "$api/proxies/bystander-n15/toxics" | jq -c '[.[].name]')" "$(curl -s "$api/proxies" | jq -c 'keys')"; }
check before
"${common[@]}" go test -tags=integration -count=1 -run '^TestToxicRedisDroppedReleaseReplyLeavesTheLockUncertainUntilReconciled$' ./redis/driver 2>&1 | tail -3
check "after redis/driver"
curl -s -o /dev/null -X POST "$api/proxies/bystander-n15/toxics" -H 'Content-Type: application/json' -d '{"name":"someone-elses-latency","type":"latency","stream":"downstream","attributes":{"latency":50}}'
"${common[@]}" go test -tags=integration -count=1 -run '^TestToxicJetStreamRPCCallHonoursItsDeadlineWhileHalfOpen$' ./kit/nats 2>&1 | tail -3
check "after kit/nats"
# 收尾：删掉本脚本建的代理
for n in redis nats-1 nats-2 nats-3 bystander-n15; do curl -s -o /dev/null -X DELETE "$api/proxies/$n"; done
check cleanup
