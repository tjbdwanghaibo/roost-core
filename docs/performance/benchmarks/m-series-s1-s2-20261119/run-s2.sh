#!/bin/zsh
# S2 心跳投递：M-1（No.1①③、No.2）。二进制事先编好（编译期间不测），这里只跑。
set -u
S=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/s2
W=/Users/whb/roost/roost-core/.claude/worktrees/agent-a7f9590e34cecf2ab
E=$W/docs/performance/benchmarks/m-series-s1-s2-20261119
PKG=$W/gameplay/heartbeat/internal/bench
export GOMAXPROCS=0
host() { echo "== $1 $(date '+%F %T')"; sysctl -n vm.loadavg; sysctl vm.swapusage; }
cd $PKG
{ host "S2 start"; } > $E/host-s2.txt

# ③ + 热点：缺省参数（组 4 / 100µs）二进制，16 次
for round in 1 2; do for cb in 0 5 10 20; do for hot in 0 64; do
  share=$([ $hot = 0 ] && echo 0 || echo 0.5)
  echo "-- r$round cb=$cb hot=$hot start $(date '+%T') load $(sysctl -n vm.loadavg)" >> $E/host-s2.txt
  HB_DELIVERY=1 HB_MODE=c HB_CB_US=$cb HB_HOT=$hot HB_HOT_SHARE=$share HB_LABEL=r$round-cb$cb-hot$hot \
  HB_OUT=$E/s2-delivery.jsonl \
    $S/bin-default.test -test.run '^TestDeliveryRun$' -test.count=1 -test.timeout 0 >> $S/s2-main.log 2>&1
  echo "   exit=$?" >> $E/host-s2.txt
done; done; done

# ①：组 {2,4} × 时间片 {50,100}µs，HB_MODE=c HB_CB_US=10，交替 2 轮
host "S2-1 start" >> $E/host-s2.txt
for round in 1 2; do
  if [ $round = 1 ]; then order=(g2s50 g2s100 g4s50 g4s100); else order=(g4s100 g4s50 g2s100 g2s50); fi
  for v in $order; do
    echo "-- r$round $v start $(date '+%T') load $(sysctl -n vm.loadavg)" >> $E/host-s2.txt
    HB_DELIVERY=1 HB_MODE=c HB_CB_US=10 HB_LABEL=r$round-$v HB_OUT=$E/s2-param.jsonl \
      $S/bin-$v.test -test.run '^TestDeliveryRun$' -test.count=1 -test.timeout 0 >> $S/s2-param.log 2>&1
    echo "   exit=$?" >> $E/host-s2.txt
  done
done
host "S2 end" >> $E/host-s2.txt
