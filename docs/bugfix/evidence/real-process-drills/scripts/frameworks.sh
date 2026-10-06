#!/usr/bin/env bash
# 起 game 依赖的九个框架服务（各 sid 1000，ops 端口 36101～36109），并在 account 里登记 game sid 1000 / 1001。
# 用法：DRILL_HOME=<私有目录> bash frameworks.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
i=1
for svc in account activity chat global mail match platform rank session; do
  port=3610$i; i=$((i+1))
  nohup bin/app $svc --sid 1000 --config configs/service/config.$svc.yaml > "$D/logs/dev/$svc.log" 2>&1 &
  echo $! > "$D/logs/dev/$svc.pid"
  for _ in $(seq 1 60); do curl -fsS --max-time 1 -o /dev/null "http://127.0.0.1:$port/readyz" 2>/dev/null && break; sleep 0.5; done
  curl -fsS --max-time 1 -o /dev/null "http://127.0.0.1:$port/readyz" && echo "$svc ready" || { echo "$svc NOT ready"; tail -5 "$D/logs/dev/$svc.log"; }
done
for sid in 1000 1001; do
  bin/accountctl -redis "$ROOST_DATAENGINE_IT_REDIS_ADDR" -prefix roost:rpd:account upsert-server -sid $sid && echo "registered $sid"
done
