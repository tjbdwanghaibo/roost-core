#!/bin/bash
# E-1：三个二进制交替 10 轮；每轮顺序轮换，每次前后记 uptime。
cd "$(dirname "$0")"
sides=(base head nobr)
for r in $(seq 1 10); do
  off=$(( (r-1) % 3 ))
  for k in 0 1 2; do
    side=${sides[$(( (off+k) % 3 ))]}
    echo "round=$r side=$side before: $(uptime)" >> load.log
    ./aoi-$side.test -test.run '^$' -test.bench 'AOIBatchRecompute/dense=false/coverage=false/workers=(1|4)$' -test.count 1 -test.benchmem >> raw-$side.txt 2>&1
    echo "round=$r side=$side after: $(uptime)" >> load.log
  done
done
echo DONE >> load.log
