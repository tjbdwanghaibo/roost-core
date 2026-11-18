#!/bin/bash
# usage: inject.sh <weak.py> <label>  —— 用诊断二进制在 send_stall 就绪后人为延迟发请求，每档 3 次。
S=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/e2
PY=$1; LABEL=$2
cd "$S/diag-build" || exit 1
for d in 0 400 550 650 1000 2000; do
  for k in 1 2 3; do
    out=$(ROOST_DIAG_DELAY_MS=$d python3 "$PY" "$S/diag-build/roost_native_weak" 2>&1); rc=$?
    line=$(echo "$out" | grep -E "send_stall|send failed|receive failed|peer closed \[.*since_admit_ms=[0-9-]|disconnected|deadline" | grep -v "PASS weak truncated" | head -2 | tr '\n' ' ')
    echo "$LABEL delay=$d try=$k rc=$rc :: $line"
  done
done
