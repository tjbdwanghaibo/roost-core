#!/usr/bin/env bash
# 通用门控运行器（sync-aoi 类两进程运行）：列表文件每行 "<运行名> <二进制> <参数...>"，按顺序跑。
# 每次运行前等 QUIET 秒（缺省 10）没有外来进程（foreign-ext.sh，排除本目录 $P），运行中每 2 秒检查并用 ps 采样
# 本次二进制的服务端 / 客户端累计 CPU 秒；采到外来进程就作废重跑。GOMAXPROCS 缺省 4（同 sync-aoi.sh）。
set -u
P="$1"; LIST="$2"; OUT="$3"; QUIET="${QUIET:-10}"
mkdir -p "$OUT/runs"; export GOMAXPROCS="${GOMAXPROCS:-4}" GOWORK=off
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
wait_quiet() { local q=0; while [ $q -lt $QUIET ]; do if [ -n "$("$P/cdefault/foreign-ext.sh" "$P")" ]; then q=0; else q=$((q+2)); fi; sleep 2; done; }
while read -r name bin args; do
  [ -z "$name" ] && continue; case "$name" in \#*) continue;; esac
  [ -e "$OUT/runs/$name/server.json" ] && { log "$name skip (done)"; continue; }
  while true; do
    wait_quiet; rm -rf "$OUT/runs/$name"
    log "$name start loadavg=$(sysctl -n vm.loadavg)"
    "$bin" $args -output="$OUT/runs/$name" > "$OUT/runs/$name.log" 2>&1 &
    pid=$!; dirty=""; : > "$OUT/runs/$name.cpu"
    while kill -0 $pid 2>/dev/null; do
      sleep 2
      ps -Ao pid=,time=,args= | awk -v b="$bin " 'index($0,b)>0 {role="server"; if (index($0,"-role=client")>0 || index($0,"-role client")>0) role="client"; print $1, role, $2}' >> "$OUT/runs/$name.cpu"
      f="$("$P/cdefault/foreign-ext.sh" "$P")"; [ -n "$f" ] && dirty="$f"
    done
    wait $pid; rc=$?
    if [ -n "$dirty" ]; then log "$name DISCARDED: $(echo "$dirty" | head -1)"; mv "$OUT/runs/$name" "$OUT/runs/$name.discarded.$(date +%s)" 2>/dev/null; continue; fi
    if [ ! -s "$OUT/runs/$name/server.json" ]; then log "$name FAILED rc=$rc"; tail -3 "$OUT/runs/$name.log" | tee -a "$OUT/run.log"; echo "$name" >> "$OUT/failed.txt"; break; fi
    log "$name ok rc=$rc loadavg_end=$(sysctl -n vm.loadavg)"; break
  done
done < "$LIST"
log "all done"
