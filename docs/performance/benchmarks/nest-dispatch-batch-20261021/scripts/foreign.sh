#!/usr/bin/env bash
# 打印本机上“别人的” go 构建 / 测试 / 压测进程（CPU>5%，或 compile/link 任意 CPU）；排除本目录下自己的二进制。
ME_DIR="$1"
ps -Ao pid=,pcpu=,comm=,args= | awk -v me="$ME_DIR" '
{
  pid=$1; cpu=$2+0; comm=$3; args=""; for(i=4;i<=NF;i++) args=args" "$i
  if (index(args, me) > 0) next
  base=comm; sub(".*/","",base)
  if (base ~ /^(bin|nest|bench)-[a-z0-9]+\.test$/) next   # 本轮自己的二进制（以相对路径启动）
  hit=0
  if (base=="compile" || base=="link" || base=="asm" || base=="cgo") hit=1
  else if (base=="go" && args ~ / (build|test|vet|run|generate|install) /) { if (cpu>5) hit=1 }
  else if (base ~ /\.test$/ && cpu>5) hit=1
  else if (base=="benchstat" && cpu>5) hit=1
  if (hit) print pid, cpu, base, substr(args,1,120)
}'
