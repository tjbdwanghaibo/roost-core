#!/usr/bin/env bash
# C r1 100%（00:16:11）与本 agent 的一次 policy 基准编译 / 运行重叠、门控没采到：补跑一次 C 100%（r4）替换。
S="$1"
until grep -q "all done" "$S/sweep/run.log" 2>/dev/null; do sleep 20; done
mkdir -p "$S/redo"
cd "$S"
wait_quiet() { local q=0; while [ $q -lt 30 ]; do if [ -n "$("$S/foreign.sh" "$S")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
while true; do
  wait_quiet
  echo "$(date +%H:%M:%S) C r4 active=1 start loadavg=$(sysctl -n vm.loadavg)" >> "$S/redo/run.log"
  rm -f "$S/redo/tmp.jsonl"
  HB_DELIVERY=1 HB_MODE=c HB_ACTIVE=1 HB_RES=10ms HB_LABEL="C/r4" HB_OUT="$S/redo/tmp.jsonl" ./bin-final.test -test.run '^TestDeliveryRun$' -test.count=1 -test.timeout 0 > "$S/redo/last.out" 2>&1 &
  pid=$!; dirty=""
  while kill -0 $pid 2>/dev/null; do sleep 2; f="$("$S/foreign.sh" "$S")"; [ -n "$f" ] && dirty="$f"; done
  wait $pid
  if [ -n "$dirty" ] || [ ! -s "$S/redo/tmp.jsonl" ]; then echo "$(date +%H:%M:%S) DISCARDED $dirty" >> "$S/redo/run.log"; continue; fi
  cat "$S/redo/tmp.jsonl" >> "$S/redo/delivery.jsonl"; echo "$(date +%H:%M:%S) all done" >> "$S/redo/run.log"; break
done
