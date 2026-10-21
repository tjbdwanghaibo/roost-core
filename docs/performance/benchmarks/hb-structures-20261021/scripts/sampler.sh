#!/bin/zsh
# 每 5 秒记一次别的进程（有才记），直到 $2 文件出现。
D=${0:A:h}
while [ ! -e $2 ]; do f=$($D/foreign.sh); [ -n "$f" ] && echo "$(date +%T) $f" >> $1; sleep 5; done
