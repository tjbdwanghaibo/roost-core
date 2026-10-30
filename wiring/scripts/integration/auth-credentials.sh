#!/usr/bin/env bash
# 连接 Mod 从环境变量读凭据（R11，`<键>_env`）的真实依赖验收：起一套只给这组用例用的、开了鉴权的私有
# mongod / redis-server / nats-server / etcd（本机 brew 二进制，不用 docker），建账号，跑
# `*ReadsCredentialsFromTheEnvironment` 用例，结束时停掉并删除根目录。用例以 ROOST_IT_AUTH_* 为准入，没有这些变量时 skip；
# 这里是它们唯一的执行入口（共享隔离环境与 CI 都不开鉴权）。包清单由根包 integration_coverage_test.go 钉住：每个含
# ROOST_IT_AUTH_ 测试文件的包都要列在这里，没有的包不能列。
#
# 用法：bash wiring/scripts/integration/auth-credentials.sh
#   ROOST_AUTH_IT_PORT_BASE（缺省 39700）：mongod = base+17、redis = base+79、nats = base+22、etcd 客户端 / 对等 = base+31 / base+32。
#   口令每次随机生成，只在本进程的环境里，不打印、不落盘到仓库。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$repo_dir"
export GOWORK=off

base="${ROOST_AUTH_IT_PORT_BASE:-39700}"
mongo_port=$((base + 17)) redis_port=$((base + 79)) nats_port=$((base + 22)) etcd_port=$((base + 31)) etcd_peer=$((base + 32))
for port in "$mongo_port" "$redis_port" "$nats_port" "$etcd_port" "$etcd_peer"; do
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "auth-credentials: port $port is in use; set ROOST_AUTH_IT_PORT_BASE" >&2
    exit 1
  fi
done

root="$(mktemp -d "${TMPDIR:-/tmp}/roost-auth-it.XXXXXX")"
pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; done
  [ -f "$root/mongo.pid" ] && kill "$(cat "$root/mongo.pid")" 2>/dev/null || true
  [ -f "$root/redis.pid" ] && kill "$(cat "$root/redis.pid")" 2>/dev/null || true
  sleep 1
  rm -rf "$root"
}
trap cleanup EXIT

password="$(openssl rand -hex 16)"
mkdir -p "$root/mongo" "$root/redis" "$root/etcd"

mongod --port "$mongo_port" --bind_ip 127.0.0.1 --dbpath "$root/mongo" --auth --fork \
  --logpath "$root/mongo.log" --pidfilepath "$root/mongo.pid" >/dev/null
redis-server --port "$redis_port" --bind 127.0.0.1 --daemonize yes --pidfile "$root/redis.pid" \
  --logfile "$root/redis.log" --dir "$root/redis" --save "" --appendonly no >/dev/null
nats-server -a 127.0.0.1 -p "$nats_port" --user r11user --pass "$password" -l "$root/nats.log" >/dev/null 2>&1 &
pids+=("$!")
etcd --name auth-it --data-dir "$root/etcd" \
  --listen-client-urls "http://127.0.0.1:$etcd_port" --advertise-client-urls "http://127.0.0.1:$etcd_port" \
  --listen-peer-urls "http://127.0.0.1:$etcd_peer" --initial-advertise-peer-urls "http://127.0.0.1:$etcd_peer" \
  --initial-cluster "auth-it=http://127.0.0.1:$etcd_peer" >"$root/etcd.log" 2>&1 &
pids+=("$!")

for _ in $(seq 1 30); do
  if redis-cli -p "$redis_port" PING >/dev/null 2>&1 && etcdctl --endpoints="127.0.0.1:$etcd_port" endpoint health >/dev/null 2>&1 &&
    lsof -nP -iTCP:"$nats_port" -sTCP:LISTEN >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

# 账号：Mongo 经 localhost 例外建第一个 root 用户；Redis 建 ACL 用户并关掉 default；etcd 建 root 后开鉴权。
ROOST_AUTH_IT_PASSWORD="$password" mongosh --quiet --port "$mongo_port" \
  --eval "db.getSiblingDB('admin').createUser({user:'r11admin',pwd:process.env.ROOST_AUTH_IT_PASSWORD,roles:['root']})" >/dev/null
redis-cli -p "$redis_port" ACL SETUSER r11user on ">$password" '~*' '&*' '+@all' >/dev/null
redis-cli -p "$redis_port" ACL SETUSER default off >/dev/null
printf '%s\n%s\n' "$password" "$password" | etcdctl --endpoints="127.0.0.1:$etcd_port" user add root --interactive=true >/dev/null 2>&1
etcdctl --endpoints="127.0.0.1:$etcd_port" user grant-role root root >/dev/null
etcdctl --endpoints="127.0.0.1:$etcd_port" auth enable >/dev/null

export ROOST_IT_AUTH_MONGO_URI="mongodb://127.0.0.1:$mongo_port" ROOST_IT_AUTH_MONGO_USERNAME=r11admin ROOST_IT_AUTH_MONGO_PASSWORD="$password"
export ROOST_IT_AUTH_REDIS_ADDR="127.0.0.1:$redis_port" ROOST_IT_AUTH_REDIS_USERNAME=r11user ROOST_IT_AUTH_REDIS_PASSWORD="$password"
export ROOST_IT_AUTH_NATS_URL="nats://127.0.0.1:$nats_port" ROOST_IT_AUTH_NATS_USERNAME=r11user ROOST_IT_AUTH_NATS_PASSWORD="$password"
export ROOST_IT_AUTH_ETCD_ENDPOINTS="127.0.0.1:$etcd_port" ROOST_IT_AUTH_ETCD_USERNAME=root ROOST_IT_AUTH_ETCD_PASSWORD="$password"
# 同包里以别的变量准入的用例在这里 skip（它们各有入口），清掉免得调用者的环境把它们带到这套实例上。
unset ROOST_DATAENGINE_IT REDIS_ADDR ROOST_DATAENGINE_IT_MONGO_URI ROOST_DATAENGINE_IT_NATS_URL ROOST_REVIEW_CLUSTER
go test -tags=integration -count=1 -run 'ReadsCredentialsFromTheEnvironment' -v ./wiring/mongo ./wiring/redis ./wiring/nats ./wiring/etcd |
  grep -E '^(=== RUN|--- |ok|FAIL|PASS|panic)|_test\.go:'
