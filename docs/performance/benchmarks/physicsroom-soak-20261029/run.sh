#!/bin/zsh
# usage: run.sh <soak.test 二进制> <输出目录> [分钟=20]
# 二进制：GOWORK=off go test -tags physx -c -o <soak.test> ./wiring/physicsroom
# 环境变量 INTERVAL（测试窗口秒，缺省 15）、SAMPLE（footprint 间隔秒，缺省 30）、VMMAP_EVERY（缺省 5）。
set -eu
bin=$1 out=$2 minutes=${3:-20}
here=${0:A:h}
mkdir -p "$out"
cd "$(git -C "$here" rev-parse --show-toplevel)/wiring/physicsroom"
ROOST_PHYSICS_SOAK_MINUTES=$minutes ROOST_PHYSICS_SOAK_INTERVAL=${INTERVAL:-15} ROOST_PHYSICS_SOAK_TSV="$out/go.tsv" \
  "$bin" -test.run '^TestPhysicsRoomSoak$' -test.v -test.timeout $(( minutes + 5 ))m > "$out/test.log" 2>&1 &
pid=$!
python3 -I "$here/sample.py" $pid "$out/proc.tsv" ${SAMPLE:-30} ${VMMAP_EVERY:-5} &
sampler=$!
wait $pid; rc=$?
wait $sampler || true
tail -2 "$out/test.log"
exit $rc
