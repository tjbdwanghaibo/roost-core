#!/bin/sh
# 用法: run.sh <label> <roost args...>；在 SIG 窗口内向 roost 发 SIGINT，报告退出状态和工程旁的暂存目录。
S=${N08_SCRATCH:?set N08_SCRATCH to a scratch dir holding demo/, roost and fakego/}
label=$1; shift
parent=$S/sigexp/$label; rm -rf "$parent"; mkdir -p "$parent"
cp -R $S/demo "$parent/proj"
export FAKE_GO_MARK=$parent/mark
( cd "$parent/proj" && PATH=$S/fakego:$PATH exec perl -e '$SIG{INT}="DEFAULT"; exec @ARGV or die' $S/roost "$@" >"$parent/out.txt" 2>&1 ) &
pid=$!
i=0; while [ ! -f "$FAKE_GO_MARK" ] && [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done
echo "fake go cwd: $(cat "$FAKE_GO_MARK" 2>/dev/null | sed "s#$parent/##")"
kill -${SIG:-INT} $pid
wait $pid; echo "roost exit status: $?"
echo "siblings of project:"; ls -a "$parent" | grep -v '^\.\.\?$'
pgrep -f "$S/fakego/go" >/dev/null && echo "fake go still alive" || echo "no fake go left"
