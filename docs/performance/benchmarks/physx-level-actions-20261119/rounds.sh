#!/usr/bin/env bash
# 改前 / 流程关 / 流程开 交替测量：rounds.sh <改前的 physicsroom.test> <改后的 physicsroom.test> [轮数=3] > rounds.txt
# 二进制：在各自的工作树里 GOWORK=off CGO_ENABLED=1 go test -tags physx -c -o <out> ./wiring/physicsroom
# （改前的工作树要用它自己的 bridge.cpp 链出 ABI 2 的 libroost_physx）。每行：标签 轮次 MULTI_ROOM_RESULT 的 JSON。
set -euo pipefail
base=$1 after=$2 rounds=${3:-3}
run(){ ROOST_PHYSICS_LOAD=1 "$1" -test.run "TestMultiRoomReplicationLoad/$2/(1|4|8)\$" -test.count=1 -test.timeout=3m -test.v 2>&1 | grep MULTI_ROOM | sed "s/.*RESULT /$3 $4 /"; }
for r in $(seq 1 "$rounds"); do
 run "$base" mechanisms base "$r"
 run "$after" mechanisms off "$r"
 run "$after" flow on "$r"
done
