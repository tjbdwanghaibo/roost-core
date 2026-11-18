#!/bin/bash
# usage: detail.sh <tag> —— 对正在跑的 soak 进程取一次逐区域 vmmap（会短暂挂起目标进程）。
S=<scratch>
pid=$(awk '/^pid/{print $2}' "$S/host.txt")
kill -0 "$pid" || exit 1
echo "tag=$1 wall=$(date +%T) rss_kb=$(ps -o rss= -p "$pid")" >> "$S/detail.meta"
vmmap -w -interleaved "$pid" > "$S/detail-$1.txt" 2>&1
echo "after vmmap rss_kb=$(ps -o rss= -p "$pid")" >> "$S/detail.meta"
wc -l "$S/detail-$1.txt"
