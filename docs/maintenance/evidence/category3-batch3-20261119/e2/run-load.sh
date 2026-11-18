#!/bin/bash
# usage: run-load.sh <build-dir> <out-dir> <n-yes> <rounds> <budget-s>
B=$1; O=$2; NY=$3; R=$4; BUDGET=$5
mkdir -p "$O"; : > "$O/rounds.txt"; : > "$O/yes.pids"
pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null; done
  for p in "${pids[@]}"; do wait "$p" 2>/dev/null; done
  echo "cleanup done $(date -u +%T) load=$(sysctl -n vm.loadavg)" >> "$O/env.txt"
}
trap cleanup EXIT INT TERM
echo "start $(date -u +%T) load=$(sysctl -n vm.loadavg) swap=$(sysctl -n vm.swapusage)" > "$O/env.txt"
for i in $(seq "$NY"); do yes >/dev/null & pids+=($!); echo $! >> "$O/yes.pids"; done
sleep 60
echo "warm $(date -u +%T) load=$(sysctl -n vm.loadavg)" >> "$O/env.txt"
start=$(date +%s); fails=0
for i in $(seq "$R"); do
  now=$(date +%s)
  if (( now - start > BUDGET )); then echo "TIME_LIMIT after $((i-1)) rounds" >> "$O/env.txt"; break; fi
  ctest --test-dir "$B" -R '^native_weak$' --timeout 60 -V > "$O/round-$i.log" 2>&1; rc=$?
  ld=$(sysctl -n vm.loadavg | awk '{print $2}')
  np=$(grep -c "^4: PASS weak" "$O/round-$i.log")
  echo "$i $rc $np $ld" >> "$O/rounds.txt"
  if (( rc != 0 || np != 5 )); then
    fails=$((fails+1))
    if (( fails <= 5 )); then
      lsof -nP -iTCP > "$O/fail-$i.lsof" 2>&1
      netstat -an > "$O/fail-$i.netstat" 2>&1
    fi
  fi
done
echo "end $(date -u +%T) fails=$fails load=$(sysctl -n vm.loadavg) swap=$(sysctl -n vm.swapusage)" >> "$O/env.txt"
