#!/usr/bin/env bash
# 私有三节点 JetStream 集群（R3 流可用、文件存储、服务端缺省刷盘设置），给 JetStream 日志原语的真实语义用例与
# leader kill 用例用（infra/network/nats/driver/jetstream_log_real_integration_test.go）。
#
# 用法：scripts/nats-cluster.sh start|stop|kill <n>|restart <n>|status|env|clean|test
#   start        起三个节点并等到都健康，打印客户端地址
#   stop         停三个节点（SIGTERM，等进程退出）
#   kill <n>     kill -9 第 n 个节点（1～3）；restart <n> 重新拉起它（数据目录保留）
#   env          打印用例需要的环境变量（eval "$(scripts/nats-cluster.sh env)"）
#   clean        stop 之后删掉数据目录（只删带本脚本标记的目录）
#   test         start → 跑日志原语与 saga JetStream 协调器的真实用例（-race；ROOST_NATS_CLUSTER_COUNT 设 -count，缺省 1）→ clean；
#                失败时保留现场
#
# 环境：
#   ROOST_NATS_CLUSTER_ROOT       数据、配置、日志与 pid 目录（缺省 ${TMPDIR:-/tmp}/roost-nats-cluster）
#   ROOST_NATS_CLUSTER_PORT_BASE  第一个节点的客户端端口（缺省 29622）；路由端口 +1000、监控端口 +3000，各占连续三个
#   NATS_SERVER_BIN               nats-server 可执行文件（缺省 PATH 里的 nats-server；需要 >= 2.12.1）
#
# 不碰共享环境：端口与目录都是私有的，用前确认端口没有被占用；clean 只删 ROOST_NATS_CLUSTER_ROOT。
set -euo pipefail

ROOT="${ROOST_NATS_CLUSTER_ROOT:-${TMPDIR:-/tmp}/roost-nats-cluster}"
ROOT="${ROOT%/}"
BASE="${ROOST_NATS_CLUSTER_PORT_BASE:-29622}"
CBASE=$((BASE + 1000))
MBASE=$((BASE + 3000))
BIN="${NATS_SERVER_BIN:-nats-server}"
MARKER=".roost-nats-cluster"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SELF="$REPO/scripts/nats-cluster.sh"

urls() {
  echo "nats://127.0.0.1:$BASE,nats://127.0.0.1:$((BASE + 1)),nats://127.0.0.1:$((BASE + 2))"
}

conf() {
  local i=$1 port=$((BASE + i - 1)) cport=$((CBASE + i - 1)) mport=$((MBASE + i - 1))
  local routes="" j
  for j in 1 2 3; do
    [ "$j" = "$i" ] && continue
    routes="$routes${routes:+, }nats-route://127.0.0.1:$((CBASE + j - 1))"
  done
  mkdir -p "$ROOT/n$i"
  cat > "$ROOT/n$i.conf" <<CONF
server_name: roostjs-n$i
listen: 127.0.0.1:$port
http: 127.0.0.1:$mport
jetstream {
  store_dir: "$ROOT/n$i"
  max_memory_store: 256MB
  max_file_store: 8GB
}
cluster {
  name: roostjs
  listen: 127.0.0.1:$cport
  routes: [ $routes ]
}
CONF
}

running() {
  local i=$1
  [ -f "$ROOT/n$i.pid" ] && kill -0 "$(cat "$ROOT/n$i.pid")" 2>/dev/null
}

start_one() {
  local i=$1
  # 已在运行就不再起第二个：第二个进程起不来却会覆盖 pid 文件。
  if running "$i"; then
    return 0
  fi
  conf "$i"
  # pid 由脚本立即写（$!），不用服务端的 -P：紧接着的第二次 restart 才看得到第一次已经起了。
  nohup "$BIN" -c "$ROOT/n$i.conf" -l "$ROOT/n$i.log" >/dev/null 2>&1 &
  echo $! > "$ROOT/n$i.pid"
}

wait_healthy() {
  local _ i ok
  for _ in $(seq 1 150); do
    ok=0
    for i in 1 2 3; do
      curl -fsS "http://127.0.0.1:$((MBASE + i - 1))/healthz?js-enabled-only=true" >/dev/null 2>&1 && ok=$((ok + 1))
    done
    [ "$ok" = 3 ] && return 0
    sleep 0.2
  done
  return 1
}

stop_all() {
  local i pid _
  for i in 1 2 3; do
    [ -f "$ROOT/n$i.pid" ] || continue
    pid="$(cat "$ROOT/n$i.pid")"
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 100); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -9 "$pid" 2>/dev/null || true
    rm -f "$ROOT/n$i.pid"
  done
}

case "${1:-}" in
  start)
    command -v "$BIN" >/dev/null 2>&1 || { echo "nats-server not found (set NATS_SERVER_BIN or PATH)" >&2; exit 1; }
    mkdir -p "$ROOT"
    touch "$ROOT/$MARKER"
    for i in 1 2 3; do start_one "$i"; done
    if ! wait_healthy; then
      echo "nats-cluster did not become healthy; logs in $ROOT/n*.log" >&2
      exit 1
    fi
    echo "nats-cluster up ($("$BIN" --version)): $(urls)"
    ;;
  kill)
    kill -9 "$(cat "$ROOT/n${2:?node}.pid")"
    ;;
  restart)
    start_one "${2:?node}"
    ;;
  stop)
    stop_all
    ;;
  status)
    for i in 1 2 3; do
      if running "$i"; then echo "n$i up"; else echo "n$i down"; fi
    done
    ;;
  env)
    echo "export ROOST_NATS_CLUSTER_URL='$(urls)'"
    echo "export ROOST_NATS_CLUSTER_ROOT='$ROOT'"
    echo "export ROOST_NATS_CLUSTER_PORT_BASE='$BASE'"
    ;;
  clean)
    stop_all
    if [ -n "$ROOT" ] && [ "$ROOT" != "/" ] && [ -f "$ROOT/$MARKER" ]; then
      rm -rf "$ROOT"
    elif [ -d "$ROOT" ]; then
      echo "refusing to delete $ROOT: no $MARKER marker" >&2
      exit 1
    fi
    ;;
  test)
    "$SELF" start
    export ROOST_NATS_CLUSTER_URL="$(urls)" ROOST_NATS_CLUSTER_ROOT="$ROOT" ROOST_NATS_CLUSTER_PORT_BASE="$BASE"
    cd "$REPO"
    status=0
    GOWORK=off go test -tags integration -race -count="${ROOST_NATS_CLUSTER_COUNT:-1}" -timeout 20m -run '^TestRealLog' ./infra/network/nats/driver || status=$?
    [ "$status" = 0 ] && { GOWORK=off go test -tags integration -race -count="${ROOST_NATS_CLUSTER_COUNT:-1}" -timeout 20m -run '^TestRealJetStreamCoordinator' ./framework/saga || status=$?; }
    if [ "$status" = 0 ]; then
      "$SELF" clean
    else
      echo "tests failed (exit $status); cluster left running at $ROOT for inspection — run '$SELF clean' when done" >&2
    fi
    exit "$status"
    ;;
  *)
    echo "usage: $0 start|stop|kill <n>|restart <n>|status|env|clean|test" >&2
    exit 2
    ;;
esac
