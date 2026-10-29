#!/bin/zsh
# usage: measure.sh <label=binary>... ; env ROUNDS (default 3), RUN regex
ROUNDS=${ROUNDS:-3}
RUN=${RUN:-'TestMultiRoomReplicationLoad/(crowd|mechanisms)/8$'}
cd "$(git rev-parse --show-toplevel)/wiring/physicsroom"
for r in $(seq 1 $ROUNDS); do
  for pair in "$@"; do
    label=${pair%%=*}; bin=${pair#*=}
    ROOST_PHYSICS_LOAD=1 $bin -test.run "$RUN" -test.count=1 -test.timeout=3m -test.v 2>&1 | grep MULTI_ROOM | sed "s/.*RESULT /$label $r /"
  done
done
