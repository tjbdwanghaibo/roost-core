#!/bin/zsh
# 对比二正式测量：16 组参数 × 3 轮，轮内按轮号轮转顺序；每次运行前等机器安静，运行中采样外来进程，
# 有外来进程就丢弃该次（记在 dirty 文件）并重跑，每次最多重跑 3 回。
set -u
SP=${0:A:h:h}; S=$SP/scripts; BIN=$SP/bench.test; D=$SP/delivery; mkdir -p $D
configs=()
for res in 10ms 50ms; do for active in 0.2 1; do for mode in x y z; do configs+=("main $mode $res $active 1"); done; done; done
for mode in x y z; do configs+=("shards $mode 10ms 1 4"); done
configs+=("base none 10ms 0 1")
n=${#configs}
for rep in 1 2 3; do
  for ((i=0; i<n; i++)); do
    c=${configs[$(( (i + (rep-1)*5) % n + 1 ))]}
    set -- ${=c}
    for try in 1 2 3 4; do
      $S/wait-quiet.sh $D/wait.log
      rm -f $D/stop $D/cur.foreign $D/cur.jsonl
      $S/sampler.sh $D/cur.foreign $D/stop &
      echo "$(date +%T) rep=$rep try=$try $c load=$(sysctl -n vm.loadavg)" >> $D/host.log
      HB_DELIVERY=1 HB_LABEL=$1 HB_MODE=$2 HB_RES=$3 HB_ACTIVE=$4 HB_SHARDS=$5 HB_OUT=$D/cur.jsonl \
        $BIN -test.run TestDeliveryRun -test.count=1 -test.timeout 0 > /dev/null 2>> $D/err.txt || echo "FAIL $c" >> $D/host.log
      touch $D/stop; wait
      if [ -s $D/cur.foreign ]; then
        { echo "== rep=$rep try=$try $c"; cat $D/cur.foreign; } >> $D/dirty.log; cat $D/cur.jsonl >> $D/dirty.jsonl 2>/dev/null
      else
        cat $D/cur.jsonl >> $D/delivery.jsonl; break
      fi
    done
  done
done
echo "$(date +%T) done" >> $D/host.log
