#!/bin/zsh
# 对比一正式测量：count=1 跑 10 遍（每遍全部子基准），遍与遍之间等机器安静；遍内有外来进程的标记为 dirty，
# 再补跑直到有 10 遍干净（最多 16 遍）。
set -u
SP=${0:A:h:h}; S=$SP/scripts; mkdir -p $SP/sched
clean=0; pass=0
while [ $clean -lt 10 ] && [ $pass -lt 16 ]; do
  pass=$((pass+1))
  $S/wait-quiet.sh $SP/sched/wait.log
  rm -f $SP/sched/stop
  $S/sampler.sh $SP/sched/pass-$pass.foreign $SP/sched/stop &
  echo "$(date +%T) pass=$pass start load=$(sysctl -n vm.loadavg)" >> $SP/sched/passes.log
  $SP/bench.test -test.run '^$' -test.bench 'BenchmarkSched|BenchmarkSSRMapHint' -test.benchtime=500ms -test.count=1 -test.benchmem -test.timeout 0 > $SP/sched/pass-$pass.txt 2> $SP/sched/pass-$pass.err
  touch $SP/sched/stop; wait
  if [ -s $SP/sched/pass-$pass.foreign ]; then st=dirty; else st=clean; clean=$((clean+1)); fi
  echo "$(date +%T) pass=$pass end $st" >> $SP/sched/passes.log
done
