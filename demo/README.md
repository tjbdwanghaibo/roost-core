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
| `game/`（其余子目录） | 玩法与策略：`matchmaking`、`chatroom`、`dungeon`、`ranking`、`rewards`、`battle`、`scene`、`skills`、`gift` 等；`actor` 是玩家与怪物共用的心跳驱动、动作词表、状态目录与移动接线 |
| `saga/gift_item/` | 送礼 saga 定义 |
| `internal/service/<服务>/` | 各服务进程：game 的消费者、matchmaker、scene、battle、GM；account / chat / session 的 collaborators |
| `internal/rpc/alliance/`、`internal/service/alliance/`、`game/alliancerule/`、`game/entities/alliance*/` | 联盟服务（[联盟服务方案](../docs/maintenance/FEATURE-2026-11-04-alliance-service.md)）：独立的 alliance 进程独占 Alliance / AllianceMember（Remote 托管、`service=alliance`），对已有实体的写走路由 RPC（发往实体的 owner；Alliance 起服预加载并登记路由），建盟走随机路由，请求回执与改动同事务；守卫是名单的缓存、跨实例收尾丢了由下一个请求自愈（联盟不产生任何 effect：成员通知由服务层写成功后直接发，预约期限是持有守卫的进程里的到期回调，跨实例宣战是宣战方事务里的持久“等确认”标记 + 直接调用对方 `AcknowledgeWar`），守卫 `snapshots=none` 不发 Remote 快照；运维命令 `alliance.info` / `alliance.guard` / `alliance.transfer` / `alliance.stop_founding` |
| `game/guild/`、`saga/alliance_*/`、`internal/service/<game>/alliance_saga.go`、`alliance_notice.go` | game 侧的公会：game 只装联盟的只读 Mirror（`game/entities/alliance/mirror.go`），玩家所在公会是自己的副本 `PlayerDao.GuildID`，入会 / 退会一次 RPC，建盟扣费 / 捐献 / 领奖各一个 saga（Player 原生步骤 + 调 RPC 的 Mongo 步骤），接收成员变化通知推给在线玩家；见 [WALKTHROUGH](WALKTHROUGH.md#跨服务公会--alliance-服务写走-rpc-或-saga读走-mirror) |
| `game/terrainmap/`、`game/entities/terrain_chunk/`、`internal/service/<game>/terrain.go`、`terrain_sync.go`、`game/controllers/player/terrain_edit.go`、`cmd/terrainpack` | 可编辑地形（P1 / P2 单格，缺省关闭）：地形包、被改过的块的实体、启动装入、玩家笔刷编辑与地形同步，见[可编辑地形](#可编辑地形p1)、[玩家编辑与同步](#玩家编辑与同步p2) |
| `internal/access/player/tcp/` | 玩家 TCP 接入的鉴权（`auth.go`） |
| `configs/` | 配置表 schema、CSV、生成数据与各服务配置 |
| `cmd/loadtest`、`loadtest/` | 机器人压测与场景脚本；`cmd/accountctl` 是操作员工具 |
| `db/migrations/`、`cmd/schemamigrate` | Player schema 1 → 2 的离线迁移示例（plan / apply / verify / rollback），见 [schema 变更操作手册](../docs/framework/SCHEMA-MIGRATION.md) |
| `cmd/sidmove` | 把一个 game sid 搬到另一台机器时的检查工具（WAL 已排空、单实例锁、玩家文档未变），见[参考流程](../docs/framework/PLAYER-MIGRATION.md) |
| `cmd/sidmerge` | 把一个服并入另一个服（别名合服）的离线数据工具：合服清单、前置检查、按 sid 分库的迁移与回退；account 记录用 `cmd/accountctl merge-server`，见[参考流程 §6](../docs/framework/PLAYER-MIGRATION.md#6-合服把服-a-并入服-b) |
| `deploy/dev/` | 本机 compose、`run.sh` 与可选的 Prometheus + Grafana |

## 本地实跑

三条命令（需要 Docker）：

```bash
make dev-up      # deploy/dev/docker-compose.yaml：Mongo 副本集 rs0、NATS JetStream、Redis
make dev-run     # deploy/dev/run.sh start：编译 bin/app，先起九个托管服务（account → activity → chat → global → mail → match → platform → rank → session），
                 # 再起 alliance（联盟服务，game 的公会走它），最后起 game，共十一个进程，等每个 /readyz，
                 # 再用 cmd/accountctl 把 sid 1000 注册进 account（否则 CreateRole 拒绝）；日志与 pid 在 .dev/
make dev-smoke   # 两个机器人走完整条链（登录 → 聊天 → 加道具 → 升级 → 匹配 → 世界计数 → 建盟 / 捐献 / 领奖 saga），
                 # 再跑一轮 guild_offline_join（离线时被运维入会、再登录看到公会；要能连 game 的 ops 9100）
sh deploy/dev/second-alliance.sh start   # 可选：第二个 alliance 实例（sid 1001），建盟分到两个实例；缩容步骤见脚本头
make loadtest LOADTEST_COUNT=20   # 更多机器人；偶数，duel 两人一组
make dev-stop
```

每个服务的 `configs/service/config.<服务>.yaml` 有自己的 ops 端口（game 9100，托管服务按名字顺序从 9101 起：account 9101、activity 9102、chat 9103、global 9104、mail 9105、match 9106、platform 9107、rank 9108、session 9109；alliance 9110），十一个进程可以同机共存；`make dev-status` 看谁在跑。第二个 game 进程（sid 1001）用生成的 `deploy/dev/second-game.sh`，见 [MMORPG 手册 §13](../docs/framework/MMORPG-INTEGRATION.md)。

没有 Docker、或本机 27017 / 4222 / 6379 已被别的东西占着（一个不带 `--replSet` 的 mongod、一个没开 JetStream 的 nats-server 都不行）时，
用仓库的隔离环境（`wiring/scripts/integration/dataengine-env.sh up`：默认 Mongo 副本集 `roost-it` 27117–27119、NATS JetStream 14222–14224、Redis 16379，
端口偏移与根目录见[该目录 README](../wiring/scripts/integration/README.md)），然后把十一份配置改指向它：

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

- `player` 集合在 DAO 逻辑库 `db=main` 经 `dataengine.databases` 映射到的物理库里（生成配置把 main 映射到工程名，工程 planet 即 `planet.player`；不是 `dataengine.database`，也不是名为 `main` 的库）；文档 `_id` 是完整 EntityID，按玩家 ID 查用 `roost id entity --unique <玩家ID> --entity Player` 换算；重启 game 再跑一轮，items 与 level 在原值上累加；
- mail 的 Redis 里每个升级的玩家一封 `box:<id>`，`send:<EffectID>` 是幂等键；`db.world` 的 `players_entered` / `matches_formed` 随每轮增长；
- 怪物不落库——工程库里没有怪的集合（`MonsterDao` 是 `//roost:dao nocoll`），GM `gm.scene.population` 才是看它们的地方；
- chat 的 Redis 里 world 频道的流每次登录多一条系统公告、每个机器人多一句 hello；
- 机器人进程以非零码退出时打印 JSON 报告，最后一行点名违反的阈值；压测参数与阈值见[功能逐项说明 · 机器人压测](WALKTHROUGH.md#机器人压测--回归测试)。

可选的观测：`deploy/dev/observability/` 是一套 Prometheus + Grafana（抓 game 与托管服务的 ops 端口与压测的 `-metrics-addr`，仪表盘 "Roost game-demo" 按链路分组），
指标与链路步骤的对应写在同目录 `README.md`。它与会被重生成的 `deploy/dev/docker-compose.yaml` 分开，观测是可选的。

### 常见问题

- **共享 NATS 上 RPC 大面积报 `bus: unsupported rpc response version 0`**：给每次实跑一个独立的 `nats.prefix`（全部进程一致）。共享的 JetStream 集群里若残留了
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
  计入 `player_tcp_connection_rejected_total{reason}`（2026-10-22 登录风暴 200 人里 72 个即此，`reason="per_ip"` 72）；要压更大就调高这个配置，
  例如 `sed -i '' 's#max_connections_per_ip: 128#max_connections_per_ip: 512#' configs/service/config.game.yaml`。
- **`scene_alive: … the server stopped replicating to it`，或机器人中途报连接被关**：这个机器人的复制会话被服务端关了。
  game 日志 `scene: replication session was closed by the manager` 给出原因（`reason`）；编码失败（`reason=too_large` / `encode_failed`）时服务端
  还会断开这个玩家（2026-10-22 起，`selfsync: a player's sync session was lost …` 限频 Warn 与 `player.sync.session_lost_disconnect.total{reason}`），
  机器人看到的是连接被关。持有的对象超过 Manager 的 `MaxObjects` 不再关会话（2026-11-08 起超额降级，方案 D17）：多出来的对象暂缓、视野从最远处收紧，
  game 日志是限频 Warn `entitysync: a session is over its object limit …`，计 `entitysync_over_limit_deferred_total{namespace,reason}`（主视野上限见 `internal/service/game/scene.go` 的 `sceneMaxVisible`）。
- **game 连本机 `127.0.0.1:2379` 的 etcd**：`configs/service/config.game.yaml` 的 `etcd.endpoints` 缺省指向本机，上面的 sed 不改它；
  game 用它做服务注册（`etcd.service_prefix`，键带工程名前缀 `/<工程名>/services/...`，租约 10 秒），与别的工程不冲突；
  2026-10-22 冒烟时本机恰好有一个在跑。本机没有、或要用隔离环境之外的 etcd 时改这一项。
- **`CreateRole` 拒绝**：account 还不知道这台服。环境准备时跑一次 `go run ./cmd/accountctl -redis 127.0.0.1:6379 upsert-server -sid 1000`
  （Redis Cluster 用 `-redis-cluster <种子,…>`；`make dev-run` 已代为执行）。
- **`player:<id>` 凭据与 demo 渠道不是认证**：只为了让裸客户端跑通 demo，上线前按[功能逐项说明 · auth.go](WALKTHROUGH.md#authgo两种凭据)替换。

## 分区场景（原型，分支 feat/seamless-world-prototype）

缺省不分区：没有 `scene.partition` 的配置，game 跑普通场景，上面的一切照旧。`scene.partition.enabled: true` 把本进程的场景变成
分区场景（`gameplay/cellworld`，Cell + Ghost）的一员：sid 在 `owners` 里的进程托管那个格（**Cell**），每个进程都是自己玩家的
**Base**（Player 不迁移，玩家站在哪个格，那个格的进程上就有他的 SceneActor）。设计、取舍与端到端结果见
[无缝大世界方案 §10](../docs/maintenance/FEATURE-2026-11-03-seamless-world.md#10-v22demo-接入分区-scene2026-11-03)。

每个 game 进程读同一份（加到 `configs/service/config.game*.yaml`；声明与校验在 `game/settings/partition.go`）：

```yaml
scene:
  partition:
    enabled: true
    world: planet          # Cell / Base 消息的 NATS 主题命名空间（core pub/sub）
    cell_size: 3000        # 2 × 1 个 3000 的格：世界 6000 × 3000
    cols: 2
    rows: 1
    owners: ["1001", "1002"]   # 行优先：格 (0,0) 归 sid 1001，(1,0) 归 sid 1002
    ghost_band: 500        # G：边界带
    hysteresis: 50         # H：越出本格多远才交接
    view_radius: 400       # R：witness 半径；R + H ≤ G，R ≥ Base 的最大离开半径 300
    idle_period: 1s        # 附近没有玩家的怪的心跳档位（D1，缺省 1s；0 不降档）
    writeback_every: 5s    # SceneActor 把 HP 与位置写回 Player
    attack_damage: 10      # AttackTarget 的固定伤害
    spawn_x: 2900          # 没有快照的玩家从这里进入
    spawn_y: 1500
    spawns:                # 按格的刷怪表：每个 Cell 启动时刷满落在自己格里的点
      near_border: {x: 2960, y: 1500, count: 3, spread: 15}
      walker: {x: 1500, y: 1700, count: 1, vx: 150}
    team:                  # 组队与队伍面板（原型缺省，都可省略）
      max_members: 4       # 一支队伍最多几人
      invite_ttl: 60s      # 邀请多久不接受就过期（整秒）
      snapshot_every: 1s   # 每名队员的 Base 多久把他的位置 / HP 发给其他队员的 Base；3 个周期没更新的队友从面板移除
      resync_every: 10s    # Base 多久从 Team 重读一次成员表（兜住丢失的变化通知），长于 snapshot_every
```

分区场景里玩家用 `MoveIntent(vx, vy)`（速度，`(0,0)` 停下）移动、`AttackTarget(target)` 攻击；`Move` / `MoveTarget` 与
`gm.scene.root` 返回 `scene_partitioned`；普通场景里 `MoveIntent` / `AttackTarget` 返回 `scene_not_partitioned`。客户端看到的
其他玩家与自己是 `player` 命名空间（位置与 `cell_hp`），怪是 `monster`。`gm.scene.partition` 看本进程的格、计数与区域表。
**组队与队伍面板**（维护者 2026-11-04 第 4 项 = A，只在分区场景）：`TeamInvite(invitee)`（没有队伍时建一支、自己当队长）、
`TeamAccept(team_id)`、`TeamLeave()`（队长离队时队长交给剩下玩家 ID 最小的那名）、`TeamDisband()`（只有队长）、`TeamInfo()`，
答复都是成员表（`TeamResponse`）。成员表在 Remote 托管的 `Team` 实体上（`service=game`、共享模式：成员在不同的 game 进程上；game 因此装着
`remote_entity`，联盟实体对它是别的服务的、写不了），每个端点是一笔 [Team, Player] 事务；普通场景答 `scene_not_partitioned`，普通场景的组队仍是匹配（不动）。队友的位置与 HP 从**队伍面板**来：每名队员的 Base
每 `snapshot_every` 把他的 SceneActor 状态发给其他队员的 Base，客户端在同一条同步流里收到队友的 SceneActor（`player`
命名空间，位置与 `cell_hp`），不论距离；在视野里的队友就是同一个对象，不重复推。被邀请的人从别处得知队伍 ID（原型不推送邀请）。
多进程端到端（两个 Cell、一个独立 Base、kill -9 后重建、跨 Base 的队伍面板、普通场景照常）：仓库的 `scripts/test-seamless-demo.sh`。

## 可编辑地形（P1）

缺省没有地形：不配 `scene.terrain.pack`，game 的其余功能一切照旧。配上之后，game 进程在启动时装入一张可编辑地形
（[可编辑地形方案 §12](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#12-p1-实体接入实施结果2026-11-08)，
框架部分是 [gameplay/terrain](../gameplay/terrain/README.md)）：原始地形来自一个只读的地形包，被改过的 64m 块各是一个持久的
`TerrainChunk` 实体（`db/def/terrain_chunk.go`），装入时覆盖原始块；从没改过的块不建实体、不读库。

```bash
go run ./cmd/terrainpack -out configs/terrain/demo.pack     # 2 × 1 块（128m × 64m）的确定性程序化地形，BaseRevision 1
```

新生成包默认 `HeightBase = -10000` 厘米（−100 米），普通地面若取世界高度 0，对应原始高度 1000；`-height-base` 可显式覆盖。
`-water-level` 是独立的世界水位，仍默认 40000 厘米，不随高度基准平移。该程序化地图不是平地 0 的美术地图。
更改已有地图基准要换 BaseRevision 并走现有 D13 处理，不能只改旧包头；通用 terrain 库仍支持其他基准。

```yaml
scene:
  terrain:
    pack: configs/terrain/demo.pack   # 相对进程工作目录；空 = 没有地形
```

- **装哪些块、写哪些块**：不分区时整张图都归本进程；分区场景（`scene.partition`）里本 sid 托管的格的块归本进程写，本格外扩
  `ghost_band` 范围内的块也装入（邻格的块只读，启动时读一次快照）；只做 Base 的 sid 不装块。开地形的分区场景要求 `cell_size`
  是 6400（一块）的整数倍，一块不跨两个格，加载配置时检查；demo 缺省的 3000 不满足，例如：
  `cell_size: 6400`、`cols: 2`、`rows: 1`、`owners: ["1001", "1002"]`（世界 12800 × 6400，正好是缺省地形包的 2 × 1 块；
  `spawn_x` / `spawns` 要落在新的世界里）。
- **查询**：能力 `<game>.terrain`（`*terrainmap.Map`）的 `Height` / `Material` / `IsWater` / `Probe`，世界坐标是厘米；
  不取锁，可以在 Nest handler 里调。块没装（不在本进程范围、或它的记录装不进来）答 `terrain.ErrNotLoaded`。
- **改块**：内部写入口 `Map.Edit`（一组采样点的高度与材质；一块一笔 strict 事务，handler `TerrainEdit`），提交后才进视图；
  玩家的笔刷编辑见下面的 P2。两者都会等数据库与 WAL，不能在 Nest 的业务 worker 上调用。
- **换了原始地形**（维护者 2026-11-08 决定丢弃旧编辑）：地形包的 `BaseRevision` 与本进程写的某块记录不同时，启动时经一笔正式
  的 strict 事务把这块重置为“建了、还没改过”（Version 0、数据清空，`Applied` 保留），改用新的原始块，启动日志 Warn
  `its edits were discarded` 点名块坐标与新旧版本，`discarded` 计数；重置失败时这块不装入（查询答 not loaded、不能改）、
  记录不动，日志 Error。邻格 Ghost 带里的旧记录只由它的格丢弃，本进程不装入、不动它（方案 §14）。
- **测试**：`internal/service/<game>/terrain_test.go` 是改块、回滚、重启一致、换版丢弃与笔刷；`TestTerrainRestartRealMongo` 在
  `ROOST_TERRAIN_IT_MONGO_URI` 指向副本集时用真实 DataEngine 再跑一遍重启与换版丢弃（`scripts/test-demo-generated.sh` 挂入）。

### 玩家编辑与同步（P2）

单格（[方案 §14](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#14-p2-服务端单格实施结果2026-11-08)）：玩家连在
自己所在格的 game 进程上，跨格编辑（转发给目标格）是 P3。

- **协议**：`TerrainEdit`（msgid 10038，`protocol/def/terrain_edit.go`）：请求 ID（同一玩家内唯一、非 0）、操作（1 抬高 /
  2 降低 / 3 整平 / 4 改材质）、中心（厘米）、半径（厘米）、衰减（0 常数 / 1 线性 / 2 平滑）、强度（高度原始值，1 = 10cm）、
  整平目标、材质；答复是各块的新版本与 `duplicate`（这个请求 ID 已经生效过，这次没有再改）。改动本身经同步流到达。
  笔刷的整数算法见 [gameplay/terrain“笔刷”](../gameplay/terrain/README.md#笔刷p2brushgo客户端须逐位相同)，客户端可同算做预测。
- **校验**（`terrainmap.DefaultPolicy`，`Map.SetPolicy` 可换；不是配置键，与客户端笔刷 UI 一起定）：半径 ≤ 16m、至多 4 块、
  强度 ≤ 50（5m）、高度夹在 `[0, 16383]`；领地 / 权限与建筑占位是钩子 `terrainmap.Rules`（缺省都放行，`Map.SetRules` 装）。拒绝时
  什么都不写，业务码 100052～100058（`internal/errors/terrain.go`）。
- **编辑配额**（维护者 2026-11-08，取代每秒 2 次；[方案 §17](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#17-编辑配额与材质守卫-500-实施结果2026-11-08)）：
  每人每秒 270 个改动点（`Policy.EditPointsPerSecond`）。一次编辑的点数是它会改变的高度点数 + 材质点数（事务之前在视图上算，
  共享边点只算一次，一个点两者都改算 2），至少 20（`MinEditPoints`）；令牌桶容量 270，桶满时可以透支一次，所以 r=16m 笔刷
  （约 650～800 点）、整平 / 地基照样能做，做完等补满（r=16m 约 2～3 秒一笔）。不够时整笔拒绝（`terrain_rate`），不部分执行。
  重复请求、被领地 / 建筑占位拒绝的只计 20 点。270 档已按维护者决定落地；方案 §17 的 300 档带宽数字是历史结果，
  270 档临时测量也是历史证据，本次没有重跑压测，不把它视为最终容量验收。
- **幂等**：编辑 ID 由（玩家，请求 ID）混合得出，与改动在同一笔事务里记进每块的 `Applied`（最近 32 个）；重复或乱序重放的请求
  不再生效。结果未知（超时）时用同一个请求 ID 重试。
- **事务**：覆盖到的块（至多 4 块）一笔 Nest 事务（handler `TerrainBrush`，`rollback=undo durability=strict`），第一次改的块
  先 `GetOrCreate`；提交被接受后逐块装视图并登记同步，回滚时视图与同步都不动。高度与材质都按下标写（DAO 里 `Materials` 是
  `[]uint16`，取值 0～255，原始地形包与 `gameplay/terrain` 里仍是每点 1 字节；记录里出现 > 255 的材质按损坏处理、这块不装入）；
  一块改动超过 1000 个高度（或 500 个材质，方案 §17）时该字段整字段写，两个字段各自计数（方案 §16）。
- **同步**：改过的块是 `terrain` 命名空间的同步对象（`Applied` 不下发）；场景另有一个地形 Interest（视野 200m、至多 64 块），
  玩家主视野是它的观察者。地形按命名空间单独计（方案 D15，`ManagerConfig.Namespaces`）：一个会话至多持有 64 块地形、地形的全量每会话每次
  Flush 至多 4 个；玩家 / 怪 / 场景仍是 100、不限快照，与不开地形时逐字节相同（会话的引用表合起来 164，每帧对象数仍按 100 切）。
  任一类超出上限不断开（超额降级，D17）：放不下的对象暂缓，对应的 Interest 从最远处收紧视野，有空位时逐步放宽。
  客户端用生成的 `ApplySyncTerrainChunkDao` 应用，按 `Version` 重建 mesh（C# 见 `client/dotnet/Roost.Client/Terrain/`）。
  分区场景还没有地形同步（P3）。
- **测试**：`terrain_test.go` 的 `TestTerrainBrush*`（重复 / 乱序只生效一次、拒绝零写入、改动点数配额、多块回滚、整字段守卫）；
  `terrain_sync_test.go` 走真实 TCP（客户端收到全量与增量、断线重连后逐点一致、视野满 64 块不断开会话、视野上限配大于 64 时收紧到最近的 64 块）；
  `ROOST_TERRAIN_MEASURE=1` 时跑字节与耗时测量（[P2 实测](../docs/maintenance/evidence/terrain-p2-20261108/README.md)、
  [材质改列表后重测](../docs/maintenance/evidence/terrain-materials-list-20261108/README.md)、
  [编辑配额与材质守卫 500](../docs/maintenance/evidence/terrain-quota-20261108/README.md)）。
- **升级**：`TerrainChunkDao` 是 schema 2（`Materials` 从 `[]byte` 改成 `[]uint16`，store 键 4 不变）。项目未上线，不迁移：
  schema 1 的记录按 `ErrSchemaMismatch` 拒绝（启动装入时这块按损坏不装入），升级前清空 `terrain_chunks`。

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
