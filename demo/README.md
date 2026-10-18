# `-template game-demo` 的源码

`roost project new <name> -module <mod> -template game-demo` 在 `game` 模板之上再生成**一条可运行的完整游戏链路**：
登录、背包与经验、升级发奖励邮件、匹配、聊天、副本、送礼 saga、地图与服务端权威同步、lockstep 战斗、GM 运维面和机器人压测。
这个目录就是它写进项目的那些文件，**按生成后的真实路径原样存放**（多一个 `.tmpl` 后缀）。

每块功能为什么这样写、刻意没做什么，见 [功能逐项说明](WALKTHROUGH.md)；一次请求的数据流见[数据流：六条路径](../codegen/docs/DATA_FLOW.zh-CN.md)；
框架契约见 [docs/framework](../docs/framework/README.md)。

## 为什么是文件而不是 Go 字符串

codegen 的其他模板是 `fmt.Sprintf` 出来的字符串字面量。demo 的体量大、要被人照着改，所以存成真文件：
能直接读、能 diff、能整段 review，不用在字符串里数 `%q`。

`.tmpl` 后缀让它们不参与本模块的编译：文件里带 `{{MODULE}}` 占位符，生成工程之前不是合法的 Go。
[embed.go](embed.go) 把它们嵌进生成器，`TestDemoEmbedCoversEveryFile` 保证磁盘上每个 `.tmpl` 都被嵌入。

| 占位符 | 替换成 |
| --- | --- |
| `{{MODULE}}` | 项目的 Go module 路径 |

## 生成

    roost project new planet -module example.com/planet -out ./planet -template game-demo
    cd planet

生成后工程依赖已发布的 roost-core；模板用到了尚未发版的 API 时，见下文[改这里的东西之后](#改这里的东西之后)用本仓库工作树验证。

## 目录

| 路径（生成后） | 内容 |
| --- | --- |
| `protocol/def/` | 玩家协议定义（`//roost:protocol` / `//roost:msg`） |
| `game/controllers/player/` | 端点：协议 → Controller → 生成的 Sender，错误在这里换成 `(code, reason)` |
| `game/handler/` | Nest handler（背包、经验、领奖、送礼开始等） |
| `game/entities/`、`db/def/` | Player / World / Scene / Monster 实体与 DAO 定义 |
| `game/`（其余子目录） | 玩法与策略：`matchmaking`、`chatroom`、`dungeon`、`ranking`、`rewards`、`battle`、`scene`、`skills`、`gift` 等 |
| `saga/gift_item/` | 送礼 saga 定义 |
| `internal/service/<服务>/` | 各服务进程：game 的消费者、matchmaker、scene、battle、GM；account / chat / session 的 collaborators |
| `internal/access/player/tcp/` | 玩家 TCP 接入的鉴权（`auth.go`） |
| `configs/` | 配置表 schema、CSV、生成数据与各服务配置 |
| `cmd/loadtest`、`loadtest/` | 机器人压测与场景脚本；`cmd/accountctl` 是操作员工具 |
| `db/migrations/`、`cmd/schemamigrate` | Player schema 1 → 2 的离线迁移示例（plan / apply / verify / rollback），见 [schema 变更操作手册](../docs/framework/SCHEMA-MIGRATION.md) |
| `cmd/sidmove` | 把一个 game sid 搬到另一台机器时的检查工具（WAL 已排空、单实例锁、玩家文档未变），见[参考流程](../docs/framework/PLAYER-MIGRATION.md) |
| `deploy/dev/` | 本机 compose、`run.sh` 与可选的 Prometheus + Grafana |

## 本地实跑

三条命令（需要 Docker）：

```bash
make dev-up      # deploy/dev/docker-compose.yaml：Mongo 副本集 rs0、NATS JetStream、Redis
make dev-run     # deploy/dev/run.sh start：编译 bin/app，先起九个托管服务（account → activity → chat → global → mail → match → platform → rank → session），
                 # 最后起 game，共十个进程，等每个 /readyz，
                 # 再用 cmd/accountctl 把 sid 1000 注册进 account（否则 CreateRole 拒绝）；日志与 pid 在 .dev/
make dev-smoke   # 两个机器人走完整条链（登录 → 聊天 → 加道具 → 升级 → 匹配 → 世界计数）
make loadtest LOADTEST_COUNT=20   # 更多机器人；偶数，duel 两人一组
make dev-stop
```

每个服务的 `configs/service/config.<服务>.yaml` 有自己的 ops 端口（game 9100，托管服务按名字顺序从 9101 起：account 9101、activity 9102、chat 9103、global 9104、mail 9105、match 9106、platform 9107、rank 9108、session 9109），十个进程可以同机共存；`make dev-status` 看谁在跑。第二个 game 进程（sid 1001）用生成的 `deploy/dev/second-game.sh`，见 [MMORPG 手册 §13](../docs/framework/MMORPG-INTEGRATION.md)。

没有 Docker、或本机 27017 / 4222 / 6379 已被别的东西占着（一个不带 `--replSet` 的 mongod、一个没开 JetStream 的 nats-server 都不行）时，
用仓库的隔离环境（`wiring/scripts/integration/dataengine-env.sh up`：默认 Mongo 副本集 `roost-it` 27117–27119、NATS JetStream 14222–14224、Redis 16379，
端口偏移与根目录见[该目录 README](../wiring/scripts/integration/README.md)），然后把十份配置改指向它：

```bash
for f in configs/service/config.*.yaml; do sed -i '' \
  -e 's#mongodb://127.0.0.1:27017/?replicaSet=rs0#mongodb://127.0.0.1:27117,127.0.0.1:27118,127.0.0.1:27119/?replicaSet=roost-it#' \
  -e 's#nats://127.0.0.1:4222#nats://127.0.0.1:14222#' -e 's#^  addr: 127.0.0.1:6379#  addr: 127.0.0.1:16379#' \
  -e 's#max_bytes: 8589934592#max_bytes: 67108864#' "$f"; done
make dev-run
go run ./cmd/loadtest -count 6 -account-nats nats://127.0.0.1:14222
```

总线前缀、Data Engine / saga 的库、effects / saga 的主题与流在生成配置里都按工程名派生（工程 planet：`nats.prefix: planet`、`dataengine.database: planet`、
`dataengine.effects.stream: PLANET_EFFECTS`、`saga.database: planet_saga`……），与 DAO 库名同一规则，所以不同名字的工程共用这套环境不撞库撞流，不用再改前缀；
`cmd/loadtest` 的 `-nats-prefix` 缺省也是工程名。

`dataengine.effects.max_bytes` 要调小是因为隔离集群只预留了 1GB 存储，默认 8GB 会报 `insufficient storage`。

### 验证跑通了

- `player` 集合在 DAO 标记写的 `db=<工程名>` 库里（工程 planet 即 `planet.player`，不是 `dataengine.database`）；文档 `_id` 是完整 EntityID，按玩家 ID 查用 `roost id entity --unique <玩家ID> --entity Player` 换算；重启 game 再跑一轮，items 与 level 在原值上累加；
- mail 的 Redis 里每个升级的玩家一封 `box:<id>`，`send:<EffectID>` 是幂等键；`db.world` 的 `players_entered` / `matches_formed` 随每轮增长；
- 怪物不落库——工程库里没有怪的集合（`MonsterDao` 是 `//roost:dao nocoll`），GM `gm.scene.population` 才是看它们的地方；
- chat 的 Redis 里 world 频道的流每次登录多一条系统公告、每个机器人多一句 hello；
- 机器人进程以非零码退出时打印 JSON 报告，最后一行点名违反的阈值；压测参数与阈值见[功能逐项说明 · 机器人压测](WALKTHROUGH.md#机器人压测--回归测试)。

可选的观测：`deploy/dev/observability/` 是一套 Prometheus + Grafana（抓十个进程的 ops 端口与压测的 `-metrics-addr`，仪表盘 "Roost game-demo" 按链路分组），
指标与链路步骤的对应写在同目录 `README.md`。它与会被重生成的 `deploy/dev/docker-compose.yaml` 分开，观测是可选的。

### 常见问题

- **共享 NATS 上 RPC 大面积报 `bus: unsupported rpc response version 0`**：给每次实跑一个独立的 `nats.prefix`（十个进程一致）。共享的 JetStream 集群里若残留了
  别的测试建的流、且它的 subject 过滤覆盖 `roost.rpc.>`，JetStream 会用 PubAck 回应每一个 RPC 请求、与真正的服务端抢先，于是读调用大面积失败、写调用偶尔成功
  （Wiring 集成测试留下的 `ROOST_IT_RPC_REQ_*` 流就是一例）。换前缀即可，无需删流。
- **同一个 Mongo 上用同一个工程名重新生成后重跑，game 就绪后几秒 fail-stop：`dataengine outbox: hard backlog limit exceeded`**：库名与流名按工程名派生，
  同名的新工程会接着用上一轮的库（planet 工程是 `planet`）；上一轮留下的 effect outbox 行没有消费者会 ack，超过 30 分钟触发 Data Engine 的硬积压熔断——这是它该有的行为。
  处置：`mongosh --eval 'db.getSiblingDB("planet").dropDatabase()'`（saga 库是 `planet_saga`），或换一个工程名。
- **同名工程重跑后日志里有 `saga: consumer processing failed … kind=completion … err="saga: not found"`**：同名工程共用 `saga.stream` 与 effects 流，
  上一轮的完成消息对应的 saga 记录已随库删除，协调器答 not found、重投几次后丢弃，不影响本轮。要干净就在删库之外再删这两条流
  （`nats stream rm PLANET_SAGA`、`PLANET_EFFECTS`）。这次改动（CHANGELOG [Unreleased]）之前建的工程，配置里仍是所有工程共用的
  `roost` / `game` / `ROOST_*`，照 CHANGELOG 的升级说明改成按工程名的值。
- **连续两次压测间隔不到 60s 时某个机器人 `poll_match: ticket still waiting`**：上一轮失败机器人的 duel 票还在队列里（TicketTTL 60s），被这一轮配走，剩下奇数个。
  等 sweep 清掉过期票再跑，或起偶数个再加一个。
- **`connect: auth send: robot session: closed`**：`-count` 超过了 `player_access.tcp.max_connections_per_ip`（默认 128）。机器人全从一个 IP 来，超出的连接在握手前被关掉，
  计入 `player_tcp_connection_rejected_total{reason}`；要压更大就调高这个配置。
- **`CreateRole` 拒绝**：account 还不知道这台服。环境准备时跑一次 `go run ./cmd/accountctl -redis 127.0.0.1:6379 upsert-server -sid 1000`
  （Redis Cluster 用 `-redis-cluster <种子,…>`；`make dev-run` 已代为执行）。
- **`player:<id>` 凭据与 demo 渠道不是认证**：只为了让裸客户端跑通 demo，上线前按[功能逐项说明 · auth.go](WALKTHROUGH.md#authgo两种凭据)替换。

## 改这里的东西之后

1. `GOWORK=off go test ./codegen/internal/roost/ -run TestDemo` —— 嵌入清单、步骤与文件一致性、生成后可解析。
2. 真正的验收是 CI 的 `demo` scenario（`framework-compat.yml`）：生成 + `go build` + `go vet` + `go test`。本地等价做法（仓库根）：

   ```bash
   go build -o /tmp/roost ./codegen/cmd/roost
   /tmp/roost project new planet -module example.com/planet -out /tmp/planet \
     -mods configdata,mongo,nats,dataengine,nest -template game-demo
   cd /tmp/planet && go build ./... && go vet ./... && go test ./...
   ```

   模板用到尚未发版的 roost-core API 时，生成工程要用 `go.work`（或临时 `replace`）指向本仓库工作树才能编译，不能只用已发布 tag；发版后按[维护手册 §5](../docs/maintenance/README.md#5-发版)对着 tag 重新生成验证。
   生成后再跑 `roost id check` 与 `roost generate --check`，确认 demo 留下的是一份干净的生成态。
3. 根包 `TestExamplesRun` 另外编译并实际运行独立 examples 模块中的示例。
