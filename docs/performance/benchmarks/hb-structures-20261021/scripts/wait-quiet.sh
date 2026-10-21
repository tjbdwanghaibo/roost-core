#!/bin/zsh
# 等到连续 30 秒没有别的 go 构建 / 测试进程（最多 20 分钟），把等待记到 $1。
D=${0:A:h}; quiet=0; start=$(date +%s)
while [ $quiet -lt 6 ] && [ $(( $(date +%s) - start )) -lt 1200 ]; do
  f=$($D/foreign.sh)
  if [ -z "$f" ]; then quiet=$((quiet+1)); else quiet=0; echo "$(date +%T) wait: $f" >> $1; fi
  sleep 5
done
