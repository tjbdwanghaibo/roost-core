#!/bin/zsh
# 打印本机上别的 go 构建 / 测试 / 压测进程（CPU>5%），排除本轮的 bench.test。无输出 = 安静。
ps -Ao pcpu=,args= | awk -v me="hbbench/bench.test" '
  $1+0 > 5 && index($0, me) == 0 && ($0 ~ /\.test( |$)/ || $0 ~ /\/go( |$)/ || $0 ~ /\/compile /|| $0 ~ /\/link /|| $0 ~ /sync-aoi/ || $0 ~ /mongod|nats-server|redis-server/) {
    n = split($2, p, "/"); printf "%s:%s ", p[n], $1 }'
