#!/usr/bin/env bash
# 原型用的私有 3 节点 JetStream 集群（R3、文件存储，生产缺省的刷盘设置）。
# 用法：nats-cluster.sh start|stop|kill <n>|restart <n>|status|clean
# 环境：SAGAJS_ROOT（数据与日志目录，缺省 $TMPDIR/sagajs-nats）、SAGAJS_PORT_BASE（客户端端口起点，缺省 25622）。
# 不碰共享环境：端口、目录都是私有的；clean 只删 SAGAJS_ROOT。
set -euo pipefail
ROOT="${SAGAJS_ROOT:-${TMPDIR:-/tmp}/sagajs-nats}"
BASE="${SAGAJS_PORT_BASE:-25622}"
CBASE=$((BASE + 1000))
MBASE=$((BASE + 3000))
BIN="${NATS_SERVER_BIN:-nats-server}"

conf() {
  local i=$1 port=$((BASE + i - 1)) cport=$((CBASE + i - 1)) mport=$((MBASE + i - 1))
  local routes="" j
  for j in 1 2 3; do
    [ "$j" = "$i" ] && continue
    routes="$routes${routes:+, }nats-route://127.0.0.1:$((CBASE + j - 1))"
  done
  mkdir -p "$ROOT/n$i"
  cat > "$ROOT/n$i.conf" <<CONF
server_name: sagajs-n$i
listen: 127.0.0.1:$port
http: 127.0.0.1:$mport
jetstream {
  store_dir: "$ROOT/n$i"
  max_memory_store: 256MB
  max_file_store: 8GB
}
cluster {
  name: sagajs
  listen: 127.0.0.1:$cport
  routes: [ $routes ]
}
CONF
}

start_one() {
  local i=$1
  # 已在运行就不再起第二个：失败的第二个进程会用 -P 覆盖 pid 文件。
  if [ -f "$ROOT/n$i.pid" ] && kill -0 "$(cat "$ROOT/n$i.pid")" 2>/dev/null; then
    return 0
  fi
  conf "$i"
  # pid 文件由脚本立即写（$!），不用服务端的 -P：紧接着的第二次 restart 才看得到第一次已经起了。
  nohup "$BIN" -c "$ROOT/n$i.conf" -l "$ROOT/n$i.log" >/dev/null 2>&1 &
  echo $! > "$ROOT/n$i.pid"
}

case "${1:-}" in
  start)
    mkdir -p "$ROOT"
    for i in 1 2 3; do start_one "$i"; done
    for _ in $(seq 1 100); do
      ok=0
      for i in 1 2 3; do
        curl -fsS "http://127.0.0.1:$((MBASE + i - 1))/healthz?js-enabled-only=true" >/dev/null 2>&1 && ok=$((ok + 1))
      done
      [ "$ok" = 3 ] && { echo "nats-cluster up: nats://127.0.0.1:$BASE,nats://127.0.0.1:$((BASE+1)),nats://127.0.0.1:$((BASE+2))"; exit 0; }
      sleep 0.2
    done
    echo "nats-cluster did not become healthy" >&2; exit 1 ;;
  kill)   # kill -9 一个节点
    kill -9 "$(cat "$ROOT/n$2.pid")" ;;
  restart)
    start_one "$2" ;;
  stop)
    for i in 1 2 3; do [ -f "$ROOT/n$i.pid" ] && kill "$(cat "$ROOT/n$i.pid")" 2>/dev/null || true; done ;;
  status)
    for i in 1 2 3; do
      if [ -f "$ROOT/n$i.pid" ] && kill -0 "$(cat "$ROOT/n$i.pid")" 2>/dev/null; then echo "n$i up"; else echo "n$i down"; fi
    done ;;
  clean)
    "$0" stop || true
    sleep 1
    rm -rf "$ROOT" ;;
  *) echo "usage: $0 start|stop|kill <n>|restart <n>|status|clean" >&2; exit 2 ;;
esac
