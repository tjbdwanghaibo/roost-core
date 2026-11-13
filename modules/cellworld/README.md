# cellworld：固定分区大世界的进程装配

## 能力

`roost module add cellworld` 安装 `game/cellworldhost`。它复用 `gameplay/cellworld`，负责取得 Nest、NATS、Redis、App 单实例身份与租约守卫，建立共享授权和出生目录，启动 Node，管理出生槽巡检，并在停机时排空已接纳的工作。game-demo 的分区场景使用同一个模块。

玩法状态仍在 Entity 的 DAO 中：Player 留在 Base，SceneActor / Monster 由其所在 Cell 执行。内部状态用 MessagePack 增量传输，客户端仍走 Base 的 Sync mirror 与 EntitySync。详见 [CellWorld 契约](../../gameplay/cellworld/README.md)。

## 边界

- 固定网格、一个进程一个 Cell；不提供动态拆格、跨多 Cell 原子战斗结算。
- 模块没有预设 Player / Scene / Monster 类型，不会自动替业务创建这些 Entity。也不替业务决定战斗、掉落与玩家复活规则。
- Cell 必须开启 App singleton。Redis 权威键须持久化、禁止淘汰，不允许在丢失授权记录后靠旧 Mongo 快照直接重建。
- 所有节点使用相同 world、网格与协议版本。内网 NATS 必须隔离环境并使用连接级认证。旧协议 / schema 不兼容，数据升级由离线迁移负责。
- `Runtime.Stop` 超时意味着 Node 仍未排空：不得先关 Nest / NATS / Redis 或释放单实例锁。`Node.Drain` 本身可以换新 ctx 重试，但 App 不重试 `Service.Shutdown`：生成工程里 Shutdown 把这个错误交给 App，App 照停机流程继续停 Mod（依赖未排空时不停它依赖的更底层 Mod），Mod 没有全部停完就不释放锁，进程随后退出；没排空的入场与交接由授权租约到期后的接管处理。

## 复用方式

1. 在服务的 Nest 注册阶段调用 `cellworld.NewHandlers().Register(engine)`，把同一个 Handlers 传入 NodeConfig。
2. 业务 Entity 实现 `Actor`、`RegionHost`、`BasePlayer`。Actor 的 setter 必须参与 DAO 回滚；Player 保存入场计数、恢复快照与死亡回执。位置 / HP / Buff / 冷却随回写恢复，回写周期不是硬 RPO。
3. 调 `cellworldhost.New(registry, cfg)`。它只装配；先安装 Entity 心跳，再 `Runtime.Start(ctx)`。Actor 心跳和 Scene 心跳按 50ms 驱动，普通业务仍走 Nest。
4. NPC 启动时用 `Births.Reserve` 固定名字与序号，用 `SpawnID` 构建稳定 ID，调用 `Node.ReconcileSpawn` / `Runtime.StartSpawns`。改名、删槽、改变已存在槽的规则被拒绝，须明确离线处理旧槽。
5. 有奖励时配置 `NodeConfig.SettleDeath`，在获奖 Player 的 strict 事务里调用钱包 / 背包组件；不得做外部 I/O、另开事务或在观察 Hook 中发奖。死亡回执和奖励同提交；重复投递不会重复奖励。
6. 停止业务准入后调用 `Runtime.Stop(ctx)`，成功后才卸载实体与底层 Mods。

完整且可生成运行的 Entity / DAO / 服务适配在 [game-demo](../../demo/README.md) 的 `game/partition`、`game/entities/player/cell_base.go` 与 `internal/service/game/partition.go`。

## 联动

依赖 `nest`、`redis`、`nats` 与 `entity` / `dao` 特性；没有 `with/` 文件。基础 `scene` / `monster` 模块的单进程运行时不是 CellWorld，不可对同一个 Actor 同时安装两套调度。可复用组件的数据与规则，但迁移、心跳及同步归属必须唯一。

## 验证范围

- `roost module verify cellworld`：3 条装配契约，覆盖出生号稳定性、越界拒绝与缺失 Registry 拒绝。
- core `go test -race ./gameplay/cellworld`：授权交错、丢包 / 乱序、死亡与奖励回滚、出生代次、出站背压、排空，以及本机真实 NATS / Redis / App 进程故障。
- 生成 game-demo 后测试 `./game/partition`：类型映射、回滚及真实文件 WAL 恢复。`scripts/test-seamless-demo.sh`：私有真实依赖下的账号、玩家 TCP、跨 Cell 与进程重启。
- 容量入口及已测边界见 [CellWorld README](../../gameplay/cellworld/README.md)。Linux / 跨机 / 生产 Redis HA 的结果须在目标环境实跑，不能由本机样本替代。
