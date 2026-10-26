#!/usr/bin/env bash
# 测试 / 压测用的私有权威 Redis：单主、AOF always、noeviction（与 Saga 权威 Redis 的部署契约一致）。
#
#   eval "$(scripts/it-redis-aof.sh start)"   # 起实例，导出 ROOST_SAGA_IT_REDIS_ADDR
#   scripts/it-redis-aof.sh env               # 只打印 export 行
#   scripts/it-redis-aof.sh stop              # 停实例并删数据目录
#
# 端口 ROOST_IT_REDIS_AOF_PORT（缺省 26391），数据目录 ROOST_IT_REDIS_AOF_ROOT/<端口>（缺省 ~/.roost-it/redis-aof）。
# 不碰共享隔离环境的 17379，也不碰本机常驻的 6379。多个会话并行时各自换端口。
set -euo pipefail
port="${ROOST_IT_REDIS_AOF_PORT:-26391}"
root="${ROOST_IT_REDIS_AOF_ROOT:-$HOME/.roost-it/redis-aof}/$port"
maxmemory="${ROOST_IT_REDIS_AOF_MAXMEMORY:-2gb}"
case "$port" in 6379|17379) echo "refusing shared port $port" >&2; exit 2 ;; esac
cmd="${1:-}"
emit() { printf 'export ROOST_SAGA_IT_REDIS_ADDR=127.0.0.1:%s\n' "$port"; }
case "$cmd" in
 start)
  command -v redis-server >/dev/null || { echo 'redis-server not found' >&2; exit 2; }
  if redis-cli -p "$port" ping >/dev/null 2>&1; then
   echo "port $port already answers; refusing to reuse an instance this script did not start" >&2
   [[ -f "$root/redis.pid" ]] || exit 2
   emit; exit 0
  fi
  mkdir -p "$root"
  redis-server --port "$port" --bind 127.0.0.1 --dir "$root" --daemonize yes --pidfile "$root/redis.pid" \
   --logfile "$root/redis.log" --appendonly yes --appendfsync always --no-appendfsync-on-rewrite no \
   --maxmemory "$maxmemory" --maxmemory-policy noeviction --save "" >/dev/null
  for _ in $(seq 1 50); do redis-cli -p "$port" ping >/dev/null 2>&1 && break; sleep 0.1; done
  redis-cli -p "$port" ping >/dev/null || { echo "redis on $port did not start; see $root/redis.log" >&2; exit 1; }
  emit ;;
 env) emit ;;
 stop)
  if [[ -f "$root/redis.pid" ]]; then
   pid="$(cat "$root/redis.pid")"
   kill "$pid" 2>/dev/null || true
   for _ in $(seq 1 50); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
  fi
  rm -rf "$root" ;;
 *) echo "usage: $0 start|env|stop" >&2; exit 2 ;;
esac
