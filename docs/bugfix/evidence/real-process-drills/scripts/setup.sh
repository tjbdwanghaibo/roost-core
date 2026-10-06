#!/usr/bin/env bash
# 真实进程演练的环境准备（docs/bugfix/REAL-PROCESS-DRILLS-2026-10-06.md）。全部放在私有目录 DRILL_HOME：
#   1. scripts/mirror-local.sh up：私有 Mongo 副本集 / NATS JetStream 3 节点 / Redis（端口偏移 DRILL_OFFSET，缺省 30000）；
#   2. 自起一个 etcd（127.0.0.1:52379，数据在 $DRILL_HOME/etcd）；
#   3. 生成 game-demo 工程 rpd（replace 到 CORE_DIR），DAO 库名常量改成 DRILL_DAO_DB，装入两个演练用的注入件，编译；
#   4. 改写生成配置指向私有依赖，另生成 sid 1001 的 game 配置。
# 用法：DRILL_HOME=<私有目录> CORE_DIR=<roost-core 检出> bash setup.sh
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

mkdir -p "$D/etcd"
if ! curl -fsS --max-time 1 http://127.0.0.1:52379/health >/dev/null 2>&1; then
  nohup etcd --name rpd --data-dir "$D/etcd/data" \
    --listen-client-urls http://127.0.0.1:52379 --advertise-client-urls http://127.0.0.1:52379 \
    --listen-peer-urls http://127.0.0.1:52380 --initial-advertise-peer-urls http://127.0.0.1:52380 \
    --initial-cluster rpd=http://127.0.0.1:52380 > "$D/etcd/etcd.log" 2>&1 &
  echo $! > "$D/etcd/pid"
  for _ in $(seq 1 50); do curl -fsS --max-time 1 http://127.0.0.1:52379/health >/dev/null 2>&1 && break; sleep 0.2; done
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
for f in configs/service/config.*.yaml; do
  case $f in *prod.example*|*config.game1001.yaml|*config.game.hook.yaml) continue;; esac
  sed -i '' \
    -e "s#mongodb://127.0.0.1:27017/?replicaSet=rs0#${ROOST_DATAENGINE_IT_MONGO_URI}#" \
    -e "s#nats://127.0.0.1:4222#${ROOST_DATAENGINE_IT_NATS_URL}#" \
    -e "s#addr: 127.0.0.1:6379#addr: ${ROOST_DATAENGINE_IT_REDIS_ADDR}#" \
    -e "s#endpoints: 127.0.0.1:2379#endpoints: 127.0.0.1:52379#" \
    -e "s#addr: 127.0.0.1:91\([0-9][0-9]\)\$#addr: 127.0.0.1:361\1#" \
    -e "s#addr: 0.0.0.0:7000\$#addr: 0.0.0.0:36700#" \
    -e "s#advertise_addr: 127.0.0.1:9000\$#advertise_addr: 127.0.0.1:36900#" \
    -e "s#^    max_bytes: 8589934592#    max_bytes: 268435456#" \
    -e "s#stream_max_bytes: 8589934592#stream_max_bytes: 268435456#" \
    -e "s#^  database: game\$#  database: $DAO_DB#" \
    "$f"
done
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
echo "setup done: project $D/rpd, env $D/env (offset $OFFSET), etcd 127.0.0.1:52379"
