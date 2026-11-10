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
| `game/terrainmap/`、`game/entities/terrain_chunk/`、`game/entities/building/`、`configs/schema/building*.go`、`internal/service/<game>/terrain.go`、`terrain_sync.go`、`building_sync.go`、`building_config.go`、`game/controllers/player/terrain_edit.go`、`cmd/terrainpack` | 可编辑地形（P1 / P2 单格、P3 普通场景的移动与寻路，缺省关闭）：地形包、被改过的块的实体、启动装入、玩家笔刷编辑与地形同步、高度场上的行走与寻路、P4 水位区域与普通场景的建筑，见[可编辑地形](#可编辑地形p1)、[玩家编辑与同步](#玩家编辑与同步p2)、[移动与寻路](#移动与寻路p3)、[水与建筑](#水与建筑p4) |
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
go run ./cmd/terrainpack -out configs/terrain/demo.pack                              # 16 × 16 块（1024m × 1024m）的平地，BaseRevision 2
go run ./cmd/terrainpack -out configs/terrain/hills.pack -shape generated -seed 1    # 起伏的程序化地形（原来的缺省），BaseRevision 1
```

缺省是 **1024m 见方的平地**（维护者 2026-11-10）：16 × 16 块，正好是普通场景能放下的最大地图（见下面“场景大小”；
包本身约 18KB，启动装入 256 块约 20～60ms、视图约 3MiB）。整张图 401m（水位 400m 之上 1m）、全是草，45° 行走规则下每一步都走得通；挖下去超过
1m 就是水（超过 1.5m 要游泳），填回去又是陆地，“挖坑成湖、填平成陆”在缺省包上直接能演示。`-ground` 改平地的世界高度（厘米，
10 厘米一档）。原来的缺省 `terrain.Generated` 起伏地形在 45° 规则下向东一步只有约 45.5% 的边走得通，缺省地图基本走不动，改为
`-shape generated -seed N` 显式选择（测试、压测与客户端金样仍用它）。

新生成包默认 `HeightBase = -10000` 厘米（−100 米），普通地面若取世界高度 0，对应原始高度 1000；`-height-base` 可显式覆盖。
`-water-level` 是独立的世界水位，仍默认 40000 厘米，不随高度基准平移（平地缺省跟着它取水位 + 1m）。
更改已有地图基准要换 BaseRevision 并走现有 D13 处理，不能只改旧包头；通用 terrain 库仍支持其他基准。`-base-revision` 不给时按
形状取：平地 2、起伏地形 1（同一个版本号只对应一份原始地形：缺省包从起伏换成平地，版本号跟着换）。

```yaml
scene:
  terrain:
    pack: configs/terrain/demo.pack   # 相对进程工作目录；空 = 没有地形
```

- **装哪些块、写哪些块**：不分区时整张图都归本进程；分区场景（`scene.partition`）里本 sid 托管的格的块归本进程写，本格外扩
  `ghost_band` 范围内的块也装入（邻格的块只读：启动时按库里的记录装入，之后由写它的格推送，见下面的 P3）；只做 Base 的 sid 不装块。开地形的分区场景要求 `cell_size`
  是 6400（一块）的整数倍，一块不跨两个格，加载配置时检查；demo 缺省的 3000 不满足，例如：
  `cell_size: 51200`、`cols: 2`、`rows: 2`、`owners: ["1001", "1002", "1003", "1004"]`（四个 512m 的格，世界 102400 × 102400，
  正好是缺省地形包的 16 × 16 块；`spawn_x` / `spawns` 要落在新的世界里）。
- **查询**：能力 `<game>.terrain`（`*terrainmap.Map`）的 `Height` / `Material` / `IsWater` / `Probe`，世界坐标是厘米；
  不取锁，可以在 Nest handler 里调。块没装（不在本进程范围、或它的记录装不进来）答 `terrain.ErrNotLoaded`。
- **改块**：内部写入口 `Map.Edit`（一组采样点的高度与材质；一块一笔 strict 事务，handler `TerrainEdit`），提交后才进视图；
  玩家的笔刷编辑见下面的 P2。两者都会等数据库与 WAL，不能在 Nest 的业务 worker 上调用。
- **换了原始地形**（维护者 2026-11-08 决定丢弃旧编辑）：地形包的 `BaseRevision` 与本进程写的某块记录不同时，启动时经一笔正式
  的 strict 事务把这块重置为“建了、还没改过”（Version 0、数据清空；玩家编辑的判重在玩家的编辑账本里，不受影响），改用新的原始块，启动日志 Warn
  `its edits were discarded` 点名块坐标与新旧版本，`discarded` 计数；重置失败时这块不装入（查询答 not loaded、不能改）、
  记录不动，日志 Error。邻格 Ghost 带里的旧记录只由它的格丢弃，本进程不装入、不动它（方案 §14）。
- **测试**：`internal/service/<game>/terrain_test.go` 是改块、回滚、重启一致、换版丢弃与笔刷；`TestTerrainRestartRealMongo` 在
  `ROOST_TERRAIN_IT_MONGO_URI` 指向副本集时用真实 DataEngine 再跑一遍重启与换版丢弃（`scripts/test-demo-generated.sh` 挂入）。

### 玩家编辑与同步（P2）

单格（[方案 §14](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#14-p2-服务端单格实施结果2026-11-08)）：玩家连在
自己所在格的 game 进程上，跨格编辑（转发给目标格）是 P3。

- **协议**：`TerrainEdit`（msgid 10038，`protocol/def/terrain_edit.go`）：编辑序号 `Seq`（见下面“幂等”）、操作（1 抬高 /
  2 降低 / 3 整平 / 4 改材质）、中心（厘米）、半径（厘米）、衰减（0 常数 / 1 线性 / 2 平滑）、强度（高度原始值，1 = 10cm）、
  整平目标、材质；答复是各块的新版本、`duplicate`（这个序号已经生效过，这次没有再改）与 `last_seq`（服务端已应用的最大序号）。
  `Seq = 0` 是续号查询：只答 `last_seq`，不编辑。改动本身经同步流到达。
  笔刷的整数算法见 [gameplay/terrain“笔刷”](../gameplay/terrain/README.md#笔刷p2brushgo客户端须逐位相同)，客户端可同算做预测。
- **校验**（`terrainmap.DefaultPolicy`，`Map.SetPolicy` 可换；不是配置键，与客户端笔刷 UI 一起定）：半径 ≤ 16m、至多 4 块、
  强度 ≤ 50（5m）、高度夹在 `[0, 16383]`；领地 / 权限与建筑占位是钩子 `terrainmap.Rules`（缺省都放行，`Map.SetRules` 装）。拒绝时
  什么都不写，业务码 100052～100059（`internal/errors/terrain.go`）。
- **编辑配额**（维护者 2026-11-08，取代每秒 2 次；[方案 §17](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#17-编辑配额与材质守卫-500-实施结果2026-11-08)）：
  每人每秒 270 个改动点（`Policy.EditPointsPerSecond`）。一次编辑的点数是它会改变的高度点数 + 材质点数（事务之前在视图上算，
  共享边点只算一次，一个点两者都改算 2），至少 20（`MinEditPoints`）；令牌桶容量 270，桶满时可以透支一次，所以 r=16m 笔刷
  （约 650～800 点）、整平 / 地基照样能做，做完等补满（r=16m 约 2～3 秒一笔）。不够时整笔拒绝（`terrain_rate`），不部分执行。
  重复请求、续号查询、被领地 / 建筑占位 / 序号窗口拒绝的只计 20 点。270 档已按维护者决定落地；方案 §17 的 300 档带宽数字是历史结果，
  270 档临时测量也是历史证据，本次没有重跑压测，不把它视为最终容量验收。
- **幂等**（T01/T02，[方案 §20](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#20-编辑请求身份与判重t01t02d182026-11-09)）：
  一次编辑的身份是（玩家 ID, 编辑序号），原样比较、不混合不截断。客户端按玩家单调递增地取序号（新编辑 = 上一次 + 1，可以跳号），
  结果未知时用同一序号重发；不知道上一次用到哪里时（会话开始、重装、换设备）先发 `Seq = 0` 查 `last_seq`，从 `last_seq + 1` 开始。
  判重在玩家的编辑账本 `TerrainEditor`（kind 12，集合 `terrain_editors`：已应用的最大序号 + 它下面 64 个序号的位图）里，
  与改块同一笔事务锁住、一起提交或回滚，重启后从库里读回。窗口内应用过的答 `duplicate`，没应用过的照常执行（乱序到达）；低于窗口的
  答 `terrain_seq`（100059，带 `last_seq`：已无法确认当时是否生效，这次不执行——同一序号至多执行一次，不依赖窗口外的记录）；
  比 `last_seq` 大出 2^20 以上也答 `terrain_seq`（从 `last_seq + 1` 续号）。
- **事务**：覆盖到的块（至多 4 块）一笔 Nest 事务（handler `TerrainBrush`，`rollback=undo durability=strict`），第一次改的块
  与玩家第一次用的编辑账本先 `GetOrCreate`，账本与块在同一笔事务里锁住；提交被接受后逐块装视图并登记同步，回滚时视图与同步都不动。高度与材质都按下标写（DAO 里 `Materials` 是
  `[]uint16`，取值 0～255，原始地形包与 `gameplay/terrain` 里仍是每点 1 字节；记录里出现 > 255 的材质按损坏处理、这块不装入）；
  一块改动超过 1000 个高度（或 500 个材质，方案 §17）时该字段整字段写，两个字段各自计数（方案 §16）。
- **生产同步节奏**（2026-11-09）：Service 的 `NestSyncObserver()` 在 Nest 构造前绑定，Init 创建 Scene 后接入同一个观察者。
  Player/Monster 等类型采用 on_change；TerrainChunk 采用首次变脏起算 1 秒的周期脏增量。首次全量、恢复与 remove 不等该周期，
  仍受提交确认/持久水位与预算约束。周期内新观察者取得即时快照时，下一次周期更新回退 Full 以保证已有观察者不丢变化。
  服务自建的观察者与 `self_sync`/`EntitySyncSetup` 或显式第二观察者冲突时在 Provide 阶段拒绝启动；不要手工伪造绑定。
  新生成工程需与包含本接线的同一 main/tag core 配套使用。旧功能/测量夹具保留 periodic 构造器，不代表生产默认；新模式未做容量压测。
- **同步**：改过的块是 `terrain` 命名空间的同步对象（编辑账本不下发）；场景另有一个地形 Interest（视野 200m、至多 64 块），
  玩家主视野是它的观察者。地形按命名空间单独计（方案 D15，`ManagerConfig.Namespaces`）：一个会话至多持有 64 块地形、地形的全量每会话每次
  Flush 至多 4 个；玩家 / 怪 / 场景仍是 100、不限快照，与不开地形时逐字节相同（会话的引用表合起来 164，每帧对象数仍按 100 切）。
  任一类超出上限不断开（超额降级，D17）：放不下的对象暂缓，对应的 Interest 从最远处收紧视野，有空位时逐步放宽。
  客户端用生成的 `ApplySyncTerrainChunkDao` 应用，按 `Version` 重建 mesh（C# 见 `client/dotnet/Roost.Client/Terrain/`）。
  分区场景的地形同步见下面的 P3。
- **测试**：`terrain_test.go` 的 `TestTerrainBrush*`（重复 / 乱序只生效一次、窗口外与重启后的旧请求重放答过期且不写、旧算法下编辑 ID
  相同的两个请求都执行、拒绝零写入、改动点数配额、多块回滚连同账本不变、整字段守卫）与 `TestTerrainLastSeqTellsAClientWhereToContinue`；
  `TestTerrainEditLedgerRealMongo` 在真实 Mongo 上验证账本落库与重启后的重放；
  `terrain_sync_test.go` 走真实 TCP（客户端收到全量与增量、断线重连后逐点一致、视野满 64 块不断开会话、视野上限配大于 64 时收紧到最近的 64 块）；
  `ROOST_TERRAIN_MEASURE=1` 时跑字节与耗时测量（[P2 实测](../docs/maintenance/evidence/terrain-p2-20261108/README.md)、
  [材质改列表后重测](../docs/maintenance/evidence/terrain-materials-list-20261108/README.md)、
  [编辑配额与材质守卫 500](../docs/maintenance/evidence/terrain-quota-20261108/README.md)）。
- **升级**：`TerrainChunkDao` 是 schema 2（`Materials` 从 `[]byte` 改成 `[]uint16`，store 键 4 不变）。项目未上线，不迁移：
  schema 1 的记录按 `ErrSchemaMismatch` 拒绝（启动装入时这块按损坏不装入），升级前清空 `terrain_chunks`。

### 分区场景：Ghost 副本与邻格可见（P3）

[方案 §21](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#21-p3-ghost-副本t05d192026-11-09)；实现在
`internal/service/<game>/terrain_partition.go`，协议是 `gameplay/terrain` 的 `Replicator`（[包说明“块副本”](../gameplay/terrain/README.md#块副本p3replicago)）。
开地形的分区场景（`scene.partition` + `scene.terrain.pack`，`cell_size` 是 6400 的整数倍）自动接上，没有新的配置键。

- **Ghost 带跟随邻格**：一块只归包含它的格的进程写。邻格改了本格 Ghost 带里的块，写它的进程在改块事务提交之后经 cellworld
  扩展消息（命名空间 `terrain`，线上格式 v7）推整块，本进程装进地形视图（`Map.ApplyReplica`），`Height` / `Probe` 读到邻格
  最新的已提交数据。订阅每秒续期兼补齐：推送丢了、任一边重启，至多 1 秒后对齐；旧的、重复的、倒序的消息按版本丢弃。
- **换版（D13）**：邻格换了原始地形包、把旧编辑重置之后，本进程（同样换包重启）撤回到新的原始块；换版前的推送带旧
  `BaseRevision`，一律丢弃，不会把旧编辑装回来。换版要整组进程换包重启：包版本不同的两个进程之间不复制。
- **邻格玩家看到别的格改过的块**：玩家连在自己的 Base 进程上。Base 进程按本进程玩家的地形视点（进场位置，之后是 cellworld
  witness 里玩家自己的位置）订阅周围 292m 内别的进程写的块，改过的块作为只读副本（`terrain_chunk.Replica`）进场景的同一个
  同步 Manager 与地形 Interest：对象 ID、命名空间、编码、按下标的增量、1 秒周期与 64 块上限都与本格的块相同，客户端不用区分。
- **编辑**：玩家的编辑在他连着的进程上执行，仍只能改这个进程写的块；改 Ghost 带或别的格的块答 `terrain_not_owned`（100054，`terrainmap.ErrNotOwned`），整笔不写，
  跨格编辑是 P5。
- **观测**：`gm.scene.partition` 多一栏 `terrain`（副本计数：订阅、推送、应用、旧版本、包版本不同丢弃、错拥有者等；Ghost 带块数、
  客户端想要的块与副本数）。只读 GM `gm.terrain.probe`（`{"chunks": [[x, y], ...], "points": [[x, y], ...]}`，至多 64 块、256 点）报本进程
  地形视图里每块是否本进程写、是否装着、版本、副本订阅报的版本与数据摘要（FNV-1a 64：高度小端 2 字节、再材质 1 字节），每点的世界
  高度；不开分区的进程同样可用。比较各进程（拥有者、Ghost 带）与客户端按同一规则算的摘要，就知道是不是同一份数据。
- **测试**：`terrain_partition_test.go`（三个进程：两个格 + 一个只做 Base，真实 TCP 客户端）：边界编辑到邻格 Ghost 与别的进程的
  客户端、小编辑是增量、玩家走远订阅跟着走、推送全丢由续期补齐、旧消息倒序重放不回退、重启补齐、D13 撤回、跨格编辑仍拒绝。
  真实多进程（真实 NATS / Mongo / Redis，kill -9 格进程）：仓库的 `ROOST_SEAMLESS_TERRAIN=1 scripts/test-seamless-demo.sh`
  （[演练证据](../docs/maintenance/evidence/terrain-partition-drill-20261110/README.md)）。
- **升级**：cellworld 线上格式 v7，v6 及更早的消息按坏消息丢弃；未上线，先停旧进程再起新进程。存储不变。

### 移动与寻路（P3）

开了地形的**普通场景**（不分区）上，玩家与怪的服务端行走、寻路和落点按高度场判定（[方案 §22](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#22-p3-移动与寻路实施结果2026-11-09)）；
分区场景的格内积分按同一套规则判定（见下面“分区场景”）。不开地形时一切与之前相同（`TestWithoutASurfaceTheMapIsTheFlatGrid` 与原有移动用例）。

- **接线**：服务 Init 在地形同步就绪后调 `installSurface`，把 `terrainmap.Map.Walker()`（`terrain.DefaultWalkRules`，建筑占位取
  进程内的建筑占位表或 `Rules.Occupied`，见下面 P4）装到场景的 `Terrain().SetSurface`，范围夹到场景的世界（即地形图，见下面“场景大小”）。`actor.Surface` 是每个 `Move` 的 `MoveConfig.Surface`：
  每拍读一次，地形改动后的下一步就按新地形判定。
- **规则**：1m 地面节点之间坡度 ≤ 45°、陡降 ≤ 3 米可走；水深 > 50 厘米是游泳（速度 60%，站立高度是水面）；地图外、块没装的节点
  不可进入。服务端位置只有 X / Y，高度按 `Walker.StandZ` 推出，客户端同算；不加 Z 字段，cellworld 线上格式不变。
- **寻路**：`PathFind.Path` 走 `Walker.FindPath`（八方向、距离 + 爬升代价、加权 1.5、上限 4096 节点）；路线经过节点中心，
  每段只跨一个节点边界。不缓存：每次 `MoveTarget` 算一次，地形在路上被改时行走在被挡的那一步停下（`Blocked`），下一个请求重新寻路。
  预算用尽时仍按原来的做法走直线、逐节点挡住。
- **落点**：`Terrain().Standable` = `Walkable` 且是能站的节点；瞬移、`MoveTarget` 的终点、出生、刷怪按它判；`Place` 有地形时按
  1m 节点向外找。
- **同步窗口与纠偏**：TerrainChunk 每秒同步一次、玩家位置变化即发，客户端最多晚约 1 秒看到地形改动；服务端行走总按最新地形，
  客户端预测与服务端不同时以收到的位置为准，新块到达后两边按同一规则一致（`terrain_move_test.go`：挖在前方、挖在脚下、抬地、
  陡坑困住后填平）。
- **场景大小**（[方案 §23](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#23-普通场景取地形图大小实施结果2026-11-09)）：开了地形的普通场景，世界就是
  地形图（`gamescene.TerrainConfig(meta.Bounds())`）：场景实体的地面、场景同步的 AOI 与行走边界都是整张图，地图外的目标、瞬移、落点一律拒绝；
  出生点 (500, 500) 不变。服务 Init 先装地形、再按 `sceneWorld` 建场景（`lifecycle.EnsureWorldScene` 带上地图）。
  普通场景能放下的地形图至多 1024m 见方的面积（如 16 × 16、32 × 8 块），更大的图（如 1w 方图）在打开地形包后、装入任何块前拒绝，
  进程启动失败——大地图用分区场景。demo 缺省包（16 × 16 块）正好在这个上限上。不开地形、分区场景的场景实体仍是 `gamescene.DefaultConfig`（1000 × 1000）。
- **主视野尺度**（[方案 §28](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#28-开地形场景的主视野尺度2026-11-10)）：主视野的尺度是场景配置
  `gamescene.Config.View`。开了地形的普通场景按游戏尺度（`TerrainView`）：进入 30m、离开 33m，客户端（`scene_view`）可要 15～60m、步长 3m，
  AOI 块边长 32m（`TerrainBlockSize`；1m 块在这个视野下超过观察者块预算 1024，构造被拒）；玩家、怪、建筑同一个视野，建筑按占地中心进出，
  站在建筑旁边看得见它。“最近 64 个”与会话上限不变。不开地形的场景与分区场景仍是 demo 尺度（`DefaultView`：进入 1.2m、离开 1.5m、
  客户端 0.6～2.7m、1m 块）。测量与待确认项（客户端开到 60m 时密集处 33m 外的玩家 / 怪会被建筑挤掉、下行上限约 35～46KB/s）见方案 §28。
- **分区场景**（跨格移动，[方案 §24](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#24-p3-跨格移动按地形判定t06-剩余2026-11-09)）：托管格的进程把
  `partitionSurface`（同一个 `Map.Walker()`，范围夹到分区世界）装成 cellworld 的地面钩子 `NodeConfig.Surface`：角色每拍沿位移逐点
  走，进入新的 1m 节点问 `Step`，被挡时玩家停下（速度清零，等下一个移动输入）、怪原路折回；交接之前走出本格的那一段读本进程视图里
  邻格块的 Ghost 副本，块没装入时停下、不猜平地；交接之后由新格按自己的视图判定。只做 Base、不开地形的进程不装。分区场景的刷怪点、
  进场位置不按 `Standable` 检查（站在不可走的节点上的角色一步不动）。测试：`terrain_partition_test.go` 的
  `TestTerrainPartitionWalkAcrossCellsFollowsTheEdits`（真实 Map、改块事务与副本，真实 cellworld Node 积分替身 NPC）。
- **限制**：陡壁深坑会困住玩家（服务端不替玩家找出路）；路线开头可能先折回起点节点中心（≤ 约 0.7m）。

### 水与建筑（P4）

[方案 §25](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#25-p4-水与建筑实施结果2026-11-10)；建筑的数据模型、配置表与同步见
[方案 §27](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#27-建筑实体化与同步2026-11-10)。

- **水位**：地形包（格式版本 2）里除了全局水位线 `WaterLevel`，还可以按块对齐的矩形区域配水位（`terrain.Meta.WaterRegions`，
  `terrainpack -water-region minX,minY,maxX,maxY,level` 可重复，块坐标、半开区间）。一个块只有一个水位：在区域里取区域的，否则取全局的。
  `IsWater` / `Probe` / 行走 / 放置都按地面高度与所在块的水位比较（同一个来源，客户端 C# 也按它），所以挖到水位以下就是水、填回水位
  以上就是陆地，规则确定、可以复现。区域水位不随原始地形之外的东西变（换水位 = 换包，走 D13）。
- **数据**：建筑是一种预制的整体加修改参数：`BuildingDao`（schema 2）记配置 ID（`ConfigID`，配置表 `building`）、修改参数值
  （`Params []int32`，下标是配置表 `building_param` 的槽）、属主、锚点（`GX` / `GY`）、朝向、地基高度，以及只持久的占地宽高与
  `BaseRevision`。配置表（`configs/schema/building.go`、`building_param.go`，CSV 在 `configs/table/`）：每种建筑的模型标识
  （`Model`，客户端预制体）、占地宽高、放置规则（`MaxRise` / `MaxCut` / `MustLevel`）与参数槽（名字、最小、最大、缺省）。demo：
  1 hut 4×4（1 槽）、2 hall 8×6（2 槽）、3 tower 3×3（3 槽）、4 keep 12×12（无槽，必须整平），朝向 1、3 时宽高互换。**占地只由
  配置的宽高与朝向决定，参数不改变占地。** 跨行规则（槽从 0 连续、名字不重复、`Min ≤ Default ≤ Max`、至多 8 槽、宽高 ≤ 64）在
  启动与每次热更新时检查（`internal/service/<game>/building_config.go`），热更新不许删已有配置 ID、不许改已有配置的宽高。
- **协议**：`Building`（msgid 10039，`protocol/def/building.go`）：`op` 1 放置（配置 ID、参数、锚点节点、朝向、是否整平）/ 2 拆除
  （锚点）/ 3 改参数（锚点、参数，只有属主），编辑序号 `Seq` 与 `TerrainEdit` 同一个（玩家, 序号）空间、同一份账本：重发答
  `duplicate`，续号用 `TerrainEdit` 的 `Seq = 0`。参数个数不能多于槽数，没给的（从末尾起）取缺省值，每个值在槽的范围里（放置与
  改参数同一规则）。
- **放置校验**（拒绝时零写入，业务码 100060～100067）：配置 ID 在配置表里（`building_config`）、参数个数与范围（`building_params`）、
  必须整平的整平了；占地在地图里、块归本进程且装着；锚点与占地上没有别的建筑；玩家站着的建筑数
  未到上限（`Policy.MaxBuildings`，缺省 16）；占地内地面高差不超过配置的上限（不整平 `MaxRise`，整平 `MaxCut`）；每个节点（整平
  时按整平之后的高度）不低于所在块的水位。整平把占地压到占地各节点高度的平均（四舍五入），是同一笔事务里的一次地形编辑，计改动点数
  配额（每次至少 20 点，整平再加改动节点数）。
- **事务与串行化点**：一次放置 / 拆除是一笔 strict 事务，锁住占地覆盖的块（至多 4 块）、建筑实体（ID 由锚点节点决定）与玩家的
  编辑账本；改参数不改变占地，只锁建筑与账本。块上的 `TerrainChunkDao.Lots`（schema 4，只持久不同步）是服务端的串行化索引：重叠
  检查在块的锁里读它——两份占地只要有一个公共节点就有一块共同的块，Nest 按块串行，并发放置不会重叠。笔刷也在块的锁里读它，改到建筑
  脚下答 `terrain_occupied`。客户端不收 `Lots`，它的占位由建筑实体 + 配置推出。
- **占位**：行走与寻路（`Map.Walker()`）读进程内的占位表：每块一个原子槽位，启动时按块的记录建好，放置 / 拆除提交之后换新；
  `Rules.Occupied` 仍是业务另外的占位钩子。
- **拆除**：只有属主能拆；建筑记录清空（确定性 ID 不删档，同一锚点以后还能放），地形不复原；锚点上没有建筑答 `building_missing`
  （重发已生效的拆除也答这个，不是 `duplicate`）。
- **同步**：建筑是普通实体，与玩家、怪走同一条 entitysync 链路（`sync=true`，命名空间 `building` 只是客户端的分流键，生成的同步字段与
  packer），订阅跟随玩家与怪：与怪完全相同，经场景的 `ShowSubject` / `HideSubject` 进出**场景主视野**（位置是占地中心），同一套半径、
  “最近 64 个”与超额降级；没有建筑自己的 Interest、`Namespaces` 一类或 `TypeSchedules` 条目（会话引用表仍是 164），按 Manager 的模式
  发（on_change）。放置提交后进主视野，拆除提交后撤出并退役（客户端收到 remove；remove 还没发出去就在同一锚点再放的，登记排在退役之后），
  改参数是普通的 DAO 写、发只带 `Params` 的增量。客户端用生成的 `ApplySyncBuildingDao`，按 `ConfigID` 查配置表实例化预制体，按 `Params`
  设参数，按 `GX` / `GY` / `Facing` / `Foundation` 摆放。整平改的地形照常走地形块的同步（可编辑地形里只有地形块有自己的订阅与节奏）。
  **主视野尺度**：建筑按占地中心进视野；开地形的普通场景主视野是游戏尺度（进入 30m、离开 33m、客户端最大 60m，见上面“主视野尺度”），
  站在建筑旁边看得见它（keep 从外面看）。客户端开到 60m 时密集处 33m 外的玩家 / 怪会被建筑挤掉（“最近 64 个”不按类型分，维护者按推荐），
  见[方案 §28](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#28-开地形场景的主视野尺度2026-11-10)。
- **重启与换版**：建筑（集合 `buildings`）、块的占地、账本的建筑数（`terrain_editors` schema 2）都随 DataEngine 落库，重启后
  原样读回，启动时对账（`ReconcileBuildings`）核对建筑与块的占地，并核对配置（配置 ID 没了或宽高变了计 `misconfigured`、保留不改）；
  核对通过的建筑在场景启动后进场景主视野。原始地形换版（D13）时块的地形改动照旧丢弃，建筑与占地保留：
  整平过地基的按原来的地基世界高度在新地形上重新整平，然后按放置规则检查，不合规的保留并计 `unfit`、记 Warn（推荐做法，待维护者确认）。
- **范围**：只在不分区的普通场景（分区场景不接建筑，答 `building_disabled`，跨格建筑是 P5）。
- **测试**：`internal/service/<game>/terrain_building_test.go`（校验、整平、判重、拆除、笔刷、绕行、并发、重启、换版、对账）；
  `building_sync_test.go`（真实 TCP 上另一个客户端在主视野里看到出现 / 改参数 / 拆除、走出主视野消失回来再出现、拆除欠着 remove 时再放，
  没有建筑自己的同步配置，与玩家 / 怪共享主视野名额与超额降级，demo 尺度测量，重启后同步与库一致，迟到的提交后通知不倒序，改参数的属主与
  判重，配置的跨行规则与热更新，配置漂移的对账）；`TestBuildingRestartRealMongo` 在
  `ROOST_TERRAIN_IT_MONGO_URI` 指向副本集时用真实 DataEngine 跑重启（含改过的参数）与换版。

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
