#!/usr/bin/env bash
# 真实进程演练的环境准备（docs/bugfix/REAL-PROCESS-DRILLS-2026-10-06.md）。全部放在私有目录 DRILL_HOME：
#   1. scripts/mirror-local.sh up：私有 Mongo 副本集 / NATS JetStream 3 节点 / Redis（端口偏移 DRILL_OFFSET，缺省 30000）；
#   2. 自起一个 etcd（127.0.0.1:52379，数据在 $DRILL_HOME/etcd）；
#   3. 生成 game-demo 工程 rpd（replace 到 CORE_DIR），DAO 库名常量改成 DRILL_DAO_DB，装入两个演练用的注入件，编译；
#   4. 改写生成配置指向私有依赖，另生成 sid 1001 的 game 配置。
# 用法：DRILL_HOME=<私有目录> CORE_DIR=<roost-core 检出> bash setup.sh
#   DRILL_REDIS=cluster：生成配置的 redis.addr 换成 redis.cluster_addrs（mirror-local 的 3 主 3 从 Cluster），
#   单实例锁、account / session / activity 等全部 Redis 用户都走 Cluster（lib.sh 的 redis-cli / accountctl 随之切换）。
#   DRILL_DAO_DB：DAO 库名（缺省 rpd2drill_game），每轮用唯一名字；DRILL_ETCD_PORT：自起 etcd 的客户端端口（缺省 52379，
#   peer 端口 +1）。
# 清理：bash teardown.sh（停 bin/app、etcd，mirror-local clean）。
set -euo pipefail
: "${DRILL_HOME:?set DRILL_HOME}" "${CORE_DIR:?set CORE_DIR to the roost-core checkout under test}"
D=$DRILL_HOME
OFFSET=${DRILL_OFFSET:-30000}
DAO_DB=${DRILL_DAO_DB:-rpd2drill_game}
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export PATH=/opt/homebrew/bin:$PATH
mkdir -p "$D/logs/dev"

ROOST_MIRROR_LOCAL_HOME="$D/env" ROOST_MIRROR_LOCAL_OFFSET="$OFFSET" bash "$CORE_DIR/scripts/mirror-local.sh" up > "$D/env-up.log" 2>&1

ETCD_PORT=${DRILL_ETCD_PORT:-52379}
ETCD_PEER=$((ETCD_PORT + 1))
mkdir -p "$D/etcd"
if ! curl -fsS --max-time 1 "http://127.0.0.1:$ETCD_PORT/health" >/dev/null 2>&1; then
  nohup etcd --name rpd --data-dir "$D/etcd/data" \
    --listen-client-urls "http://127.0.0.1:$ETCD_PORT" --advertise-client-urls "http://127.0.0.1:$ETCD_PORT" \
    --listen-peer-urls "http://127.0.0.1:$ETCD_PEER" --initial-advertise-peer-urls "http://127.0.0.1:$ETCD_PEER" \
    --initial-cluster "rpd=http://127.0.0.1:$ETCD_PEER" > "$D/etcd/etcd.log" 2>&1 &
  echo $! > "$D/etcd/pid"
  for _ in $(seq 1 50); do curl -fsS --max-time 1 "http://127.0.0.1:$ETCD_PORT/health" >/dev/null 2>&1 && break; sleep 0.2; done
fi

if [ ! -d "$D/rpd" ]; then
  (cd "$CORE_DIR" && GOWORK=off go run ./codegen/cmd/roost project new rpd -module example.com/rpd -out "$D/rpd" -template game-demo) > "$D/gen.log" 2>&1
fi
cd "$D/rpd"
GOWORK=off go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE_DIR"
# DAO 库名是编译期常量（db/def → db/gen_*_dao.go），不受 dataengine.database 配置影响。
sed -i '' -E "s/(DaoDBName[[:space:]]*= )\"[^\"]*\"/\1\"$DAO_DB\"/" db/gen_*_dao.go
mkdir -p cmd/drillinject
cp "$HERE/drillinject.go.txt" cmd/drillinject/main.go
# 包名跟随生成工程（game-demo 的 internal/service/game 包名是 Game）。
pkg=$(sed -n 's/^package //p' internal/service/game/service.go | head -1)
sed "s/^package game\$/package $pkg/" "$HERE/drill_hooks.go.txt" > internal/service/game/zz_drill_hooks.go
grep -q 'registerDrillHooks(registry)' internal/service/game/service.go || \
  perl -0pi -e 's/(func \(s \*Service\) Init\(registry \*app\.Registry\) error \{\n)/$1\tif err := registerDrillHooks(registry); err != nil {\n\t\treturn err\n\t}\n/' internal/service/game/service.go
GOWORK=off go mod tidy > "$D/tidy.log" 2>&1
GOWORK=off go build -o bin/app .
GOWORK=off go build -o bin/loadtest ./cmd/loadtest
GOWORK=off go build -o bin/accountctl ./cmd/accountctl
GOWORK=off go build -o bin/drillinject ./cmd/drillinject

# shellcheck disable=SC1091
source "$D/env/roost-dataengine-it/env.sh"
if [ "${DRILL_REDIS:-standalone}" = cluster ]; then
  REDIS_LINE="cluster_addrs: \"$ROOST_MIRROR_LOCAL_REDIS_CLUSTER\""
else
  REDIS_LINE="addr: $ROOST_DATAENGINE_IT_REDIS_ADDR"
fi
for f in configs/service/config.*.yaml; do
  case $f in *prod.example*|*config.game1001.yaml|*config.game.hook.yaml) continue;; esac
  sed -i '' \
    -e "s#mongodb://127.0.0.1:27017/?replicaSet=rs0#${ROOST_DATAENGINE_IT_MONGO_URI}#" \
    -e "s#nats://127.0.0.1:4222#${ROOST_DATAENGINE_IT_NATS_URL}#" \
    -e "s#addr: 127.0.0.1:6379#${REDIS_LINE}#" \
    -e "s#endpoints: 127.0.0.1:2379#endpoints: 127.0.0.1:${ETCD_PORT}#" \
    -e "s#addr: 127.0.0.1:91\([0-9][0-9]\)\$#addr: 127.0.0.1:361\1#" \
    -e "s#addr: 0.0.0.0:7000\$#addr: 0.0.0.0:36700#" \
    -e "s#advertise_addr: 127.0.0.1:9000\$#advertise_addr: 127.0.0.1:36900#" \
    -e "s#^    max_bytes: 8589934592#    max_bytes: 268435456#" \
    -e "s#stream_max_bytes: 8589934592#stream_max_bytes: 268435456#" \
    -e "s#^  database: game\$#  database: $DAO_DB#" \
    "$f"
done
if [ "${DRILL_REDIS:-standalone}" = cluster ]; then
  # activity / platform / rank 有跨键原子写，Cluster 下 key_prefix 必须带 hash tag（mods.ValidateClusterKeyPrefix，
  # 不带则 Init 报错）；单实例锁与 account 是单键操作，不需要。game 配置里的 activity / platform key_prefix 是同一
  # 键空间（生成时从协调方配置抄来），两边必须一起改：只改 platform 的，购买的发货写进带 tag 的键、game 去不带
  # tag 的键里取，机器人报 "delivered 10 of item 1001 but the bag holds 0"（本轮第二次尝试实测）。
  for f in configs/service/config.activity.yaml configs/service/config.platform.yaml configs/service/config.rank.yaml configs/service/config.game.yaml; do
    sed -i '' -E 's#^  key_prefix: roost:rpd:(activity|platform|rank)$#  key_prefix: "{roost:rpd:\1}"#' "$f"
  done
  # remote_entity 的版本锁（lock:<lock_key>:<id> 与 :fence）在 Cluster 下也要 hash tag；生成配置不写 lock_key
  # （缺省 e），Init 报 "Redis Cluster requires a non-empty hash tag in remote_entity.lock_key"。
  grep -q '^  lock_key:' configs/service/config.game.yaml || \
    sed -i '' -e 's#^  snapshot_l2_key_prefix: ""$#&\
  lock_key: "{roost:rpd:remote}"#' configs/service/config.game.yaml
fi
# 第二个 game：sid 1001，自己的 ops / 客户端端口与 WAL 目录。
sed -e 's/^sid: 1000$/sid: 1001/' -e 's#addr: 127.0.0.1:36100$#addr: 127.0.0.1:36110#' -e 's#addr: 0.0.0.0:36700$#addr: 0.0.0.0:36701#' \
    -e 's#advertise_addr: 127.0.0.1:36900$#advertise_addr: 127.0.0.1:36901#' \
    -e 's#dir: data/wal/dataengine$#dir: data/wal/dataengine-1001#' configs/service/config.game.yaml > configs/service/config.game1001.yaml
# 演练 3：停机总预算缩到 10s，其余与 config.game.yaml 相同（同一 sid、同一 WAL 目录）。
sed -e 's#^  total_timeout: .*#  total_timeout: 10s#' configs/service/config.game.yaml > configs/service/config.game.hook.yaml
mkdir -p "$D/scenarios-idle"
cat > "$D/scenarios-idle/idle.yaml" <<'YAML'
# Drill only: log in, enter the game, then hold the connection idle.
scenarios:
  - name: idle
    node:
      sequence:
        - action: connect
        - action: enter_game
        - wait: 120s
YAML
echo "setup done: project $D/rpd, env $D/env (offset $OFFSET), redis ${DRILL_REDIS:-standalone}, etcd 127.0.0.1:$ETCD_PORT"
