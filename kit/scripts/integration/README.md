# 隔离集成环境（dataengine-env.sh）

`dataengine-env.sh` 用本机二进制（brew 的 `mongod` / `mongosh` / `nats-server` /
`redis-server` / `redis-cli`，可选 `toxiproxy-server`）起一套只供集成测试使用的环境：
Mongo 三副本 `roost-it`、NATS JetStream 三节点、一个 Redis，装了 toxiproxy 时再给
NATS 和 Redis 各加一层代理。不用 docker，也不碰开发机自带的 27017 / 4222 / 6379。

```bash
bash kit/scripts/integration/dataengine-env.sh up      # 起并初始化，最后打印 status
bash kit/scripts/integration/dataengine-env.sh status
source "<根目录>/env.sh"                                # 导出 ROOST_DATAENGINE_IT_*，不要打印
bash kit/scripts/integration/dataengine-env.sh test    # 起环境并跑全部 integration 套件
bash kit/scripts/integration/dataengine-env.sh fault mongo-primary | nats-leader STREAM | nats-all
bash kit/scripts/integration/dataengine-env.sh heal
bash kit/scripts/integration/dataengine-env.sh down    # 停进程，保留数据
bash kit/scripts/integration/dataengine-env.sh reset   # 停进程并删除根目录
```

## 根目录与端口：`ROOST_IT_HOME`、`ROOST_IT_PORT_OFFSET`

| 变量 | 缺省 | 含义 |
| --- | --- | --- |
| `ROOST_IT_HOME` | `/tmp` | 已存在的绝对路径父目录。根目录是 `$ROOST_IT_HOME/roost-dataengine-it`，按物理路径解析（macOS 上 `/tmp` 解析为 `/private/tmp`）。 |
| `ROOST_IT_PORT_OFFSET` | `0` | 十进制非负整数，所有端口统一加上它；上限是 `65535 - 27119 = 38416`。 |
| `ROOST_IT_MONGO_CACHE_GB` | `1` | 每个 mongod 的 `--wiredTigerCacheSizeGB`。三个副本在同一台机器上，缺省缓存（物理内存的一半）合计会超过内存，长跑时宿主机换页、被测进程停顿数秒。只在 `up` 启动 mongod 时生效，已在跑的节点要 `down` 再 `up`。 |

两个都不设时与之前完全一致：根目录 `/tmp/roost-dataengine-it`，端口如下表的“偏移 0”列。

| 用途 | 偏移 0 | 偏移 1000（示例） |
| --- | --- | --- |
| Mongo `roost-it` 成员 | 27117-27119 | 28117-28119 |
| NATS 客户端 | 14222-14224 | 15222-15224 |
| NATS 路由 | 16222-16224 | 17222-17224 |
| NATS 监控 | 18222-18224 | 19222-19224 |
| Redis | 16379 | 17379 |
| toxiproxy API | 18474 | 19474 |
| toxiproxy NATS 代理 | 24222-24224 | 25222-25224 |
| toxiproxy Redis 代理 | 26379 | 27379 |

长期使用的环境（比如要跑几十分钟的压测、要跨天复用的环境）**不要放在 /tmp**：macOS
会清理 /tmp 下几天没被访问的文件，进程还在跑，数据文件、pid 文件、`env.sh`、`nats.conf`
却会被删掉。2026-09-27 旧环境的 mongo-3 因此丢了一个 WiredTiger 数据文件，之后持续报
FTDC 错误（见 `docs/review/REVIEW-2026-09-29-b30.md`「环境异常」）。推荐：

```bash
mkdir -p "$HOME/.roost-it"
lsof -nP -iTCP -sTCP:LISTEN          # 先确认偏移后的端口全部空闲
ROOST_IT_HOME="$HOME/.roost-it" ROOST_IT_PORT_OFFSET=1000 \
  bash kit/scripts/integration/dataengine-env.sh up
source "$HOME/.roost-it/roost-dataengine-it/env.sh"
```

`up` 生成的 `env.sh` 除了 `ROOST_DATAENGINE_IT_*`，还导出 `ROOST_IT_HOME` 和
`ROOST_IT_PORT_OFFSET`。source 过它的 shell 再调 `heal` / `fault` / `status`，或
`scripts/remote-fault.sh`、`scripts/test-remote-matrix.sh`、`scripts/perf/remote.sh`，
操作的就是这一套环境。另开的 shell 要么先 source `env.sh`，要么每条命令都带上这两个变量。

## 安全约束

`fault`、`down`、`reset` 会按根目录下的 pid 文件杀进程，`reset` 还会 `rm -rf` 根目录，
所以脚本只接受按规则算出的 canonical 根：

- `ROOST_IT_HOME` 必须是已存在的绝对路径，且不含空白；根目录的 basename 固定是
  `roost-dataengine-it`，不能是 `/` 或 `$HOME` 本身。
- `ROOST_DATAENGINE_IT_ROOT`（`env.sh` 导出的根）只能等于 canonical 根，否则所有命令都以
  `refuse unsafe root` 拒绝。
- 根目录与偏移绑定：首次 `up` 把偏移写进 `<根目录>/port-offset`，之后换偏移会被拒绝，
  防止新端口上的进程去抢同一数据目录。改动前建的根没有这个文件，只认偏移 0。
- pid 文件对应的进程，只有命令行里带 `" <根目录>/"` 参数时才被认作本环境的进程；
  toxiproxy 的命令行不带根目录，按 `toxiproxy-server … -port <本环境 API 端口>` 认领
  （RR-20261005-NC-202），它的 API 端口被别的进程占着时，`up` 直接拒绝。
- `<根目录>/remote-acceptance.lock`（Remote 验收锁）被别的运行持有时，除 `status` 外的命令
  与 `scripts/remote-fault.sh` 都以 2 拒绝；`test` 运行期间自己持锁。故障矩阵和
  `scripts/perf/remote.sh` 取得锁后导出 `ROOST_REMOTE_ACCEPTANCE_LOCK_HELD`，它们调起的
  `heal` / `remote-fault.sh` 照常执行（RR-20261005-NC-203）。
- 自写的故障用例不要在环境的共享代理（`redis`、`nats-1..3`）上加毒，也不要 `POST /reset`：
  在 `ROOST_DATAENGINE_IT_TOXIPROXY_URL` 上自建唯一命名、`listen: 127.0.0.1:0` 的代理，
  只删自己的毒、清理时删掉代理（`redis/driver`、`kit/nats`、`kit/dataengine`、`versionstore`、
  `mongo/driver`、`remoteentity` 的 toxic 用例都是这样；RR-20261005-NC-208）。`kit/dataengine`
  的三条 `TestToxicNATS*` 曾经仍会 `/reset`、须独占环境，已修复，现在可与其他会话并行。
  仍会 `POST /reset` 的只有 `up` / `heal`（`toxiproxy_heal`），它们本来就是清空整套环境的命令。
- 端口被根目录之外的进程占着时，`up` 拒绝启动，不会复用别人的服务。

脚本自检：`bash kit/scripts/integration/dataengine_env_test.sh`。它只 source 库、只写
临时目录，不启动进程；开头那次 `status` 只读，会指向当前变量选中的环境。

## 需要 Redis Cluster 的套件：`redis-cluster-suites.sh`

以 `ROOST_REVIEW_CLUSTER` 为准入的 integration 用例（`kit/service/mail`、`redis/driver`、
`service/mail` 各一个文件）需要真实的 Redis Cluster。脚本会清掉 `ROOST_DATAENGINE_IT` /
`REDIS_ADDR`，在 source 过 env.sh 的 shell 里运行也不会把全环境故障套件跑到隔离环境上
（RR-20261005-NC-201）。CI 的 Redis job 只有单实例，
`dataengine-env.sh` 也不起集群，所以它们在 CI 里一律 skip；本机对着 3 主 3 从集群跑：

```bash
ROOST_REVIEW_CLUSTER=127.0.0.1:7000,127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003,127.0.0.1:7004,127.0.0.1:7005 \
  bash kit/scripts/integration/redis-cluster-suites.sh
```

包清单由 `integration_coverage_promises_test.go` 钉在磁盘上的文件：新增一个含
`ROOST_REVIEW_CLUSTER` 的 integration 测试文件，它所在的包必须加进脚本，否则根包测试
`TestRedisClusterScriptNamesEveryClusterKeyedSuite` 变红；同理，`ROOST_DATAENGINE_IT`
准入的包必须在 `dataengine-env.sh test` 的清单里，`REDIS_ADDR` 准入的包必须在 ci.yml 的
Redis job 里。
