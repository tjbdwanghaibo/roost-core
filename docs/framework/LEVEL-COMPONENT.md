# 通用 LevelComponent：配置驱动的关卡阶段

`gameplay/level` 是实体拥有的通用关卡组件，供 UGC 闯关、合作 PvE 等玩法复用。它提供固定版本定义、多活动阶段、事件／条件／动作、作用域变量、逻辑定时器和外部动作回执。区域资格、物理重生、刷怪与永久奖励由业务宿主提供，框架不写专用检查点或排名状态机。

## 从 ssr 学什么，如何映射到 core

参考仓库 ssr 的 `ssr/kingdom/modules/level/internal/`：`level_state.go` 的生命周期、`level_director_trigger.go` 的多阶段驱动、`level_data.go` 的变量作用域，以及 `trigger/level_trigger_manager.go` 的事件／条件／动作注册。没有直接移植 ssr 的锁、模块和定时器实现，也不兼容其原始配置格式。

| ssr 思路 | core 中的实现 | 业务接入 |
| --- | --- | --- |
| Level 生命周期 | `Waiting → Preparing → Running → Finishing → Closing → Closed` | 初始化、开始、结算动作完成后关闭 |
| 多个活动 Stage | `Definition.Initial`、`start/end` 和阶段 generation | 并行目标、阶段切换、波次重入 |
| ECA 触发器 | `Compile` 校验后按阶段／事件索引 | 可信宿主投递事实，不执行用户脚本 |
| 全局／阶段／触发器变量 | 强类型 `Value`，`global/stage/trigger` 作用域 | 保存计数、标志与配置参数 |
| Director 与动作扩展 | `Registry` 类型表、`request`、`Complete` | 执行物理／刷怪／奖励适配器 |
| 心跳与定时器 | `AdvanceTick` 或 `OnBeat`、`NextWakeup` | 由现有物理循环、heartbeat 或 entitytimer 驱动 |
| 运行数据 | `LevelDAO` + Nest 回滚／持久提交 | 瞬态 Room 或持久关卡实体 |

## 实体装配与执行入口

可运行消费者在 [scripts/testdata/level](../../scripts/testdata/level)，执行：

```bash
bash scripts/test-level-generated.sh
go test -race -count=1 ./gameplay/level
```

脚本创建独立模块，使用正式 entity、nest 生成器生成实体装配和 Sender，再实际调用 Nest。只复制业务输入文件，不手写生成结果；不需要数据库。

实体嵌入 `EntityBase`、`ComponentManager`、`DaoManager`，挂载 DAO 与组件：

```go
state *LevelDao              `dao:"level_state"`
flow  *level.LevelComponent  `comp:"CompTypeLevel"`
```

生成器要求业务 wrapper 提供 `NewLevelDao()`、`LevelDaoCollection`；wrapper 嵌入 `*level.LevelDAO`，生成器设置持久 ID。组件 factory 从 owner 取得已创建的 DAO，用 `level.NewLevelComponent(dao, program)` 装配。Program 是编译后的深拷贝，允许多个实体共享；不要每次处理事件重新编译。

瞬态房间选择 `NewTransientLevelDAO`、实体 `noPersist=true` 和 Nest `rollback=undo durability=memory`；持久模式选择 `NewLevelDAO(id, logicalDB)` 并接入正式持久事务和 DAO descriptor。逻辑库映射遵循 DataEngine 约定。所有写 handler 必须声明 `rollback=undo` 或 `rollback=state`，默认无回滚 handler 没有组件所需事务，调用会拒绝。持久 DAO 不能放入 memory 写事务。生成消费者演示瞬态路径；持久模式的序列化与提交拒绝回滚在包测试中验证。

所有读写都由所属 Entity Guard 串行化。组件方法不是并发容器，也不自行启动 goroutine。读操作返回副本，但读取本身仍需要实体所有权；不要在网络线程直接访问 DAO 或组件。

典型调用顺序：

1. 可信宿主加载定义，`DecodeDefinition` → `Compile(def, registry)`，把定义 hash 固定到实例。
2. 在 Nest handler 中 `Initialize(instance, epoch, now)`，然后 `Start(now)`。实例标识与 epoch 来自服务器；同一 DAO 不重复初始化。
3. 模拟／定时驱动先按业务约定推进时间，再投递 `Handle(event, now)`。一个 tick 内“超时与抵达谁优先”由宿主固定顺序，组件不会在 Handle 内隐式 Advance。
4. 等写请求的 `RequestSettled` 确认成功后，再经受 Guard 保护的查询获取 `PendingActions` 副本，并在事务外执行宿主能力。不要把 handler 返回中的动作在提交结果未知时直接执行。
5. 以同一动作 ID 重试外部能力，通过 Nest handler 调用 `Complete(receipt, now)`。没有待办动作后才允许 `Close(now)`。

正式消费者通过 memory 同步 Sender 和后续只读查询展示步骤 4；持久模式应显式遵循 settled 结果与未知提交恢复契约。跨 sid 奖励继续使用 Remote／Saga 和永久去重账本，不把本地组件当作分布式事务。

## 定义与规则语义

定义版本为 1，名称、修订号和时间域必填。`TimeDomain` 为 `tick` 或 `millisecond`，Delay、Cooldown、Now 使用同一单位；Unity 时间不作为服务器可信时间。`DecodeDefinition` 拒绝未知 JSON 字段与尾随内容，Compile 拒绝未知事件、能力、变量、阶段引用、类型不匹配及动作无关字段。能力注册表只保存类型与版本，**注册能力意味着宿主已经提供真实执行器**，不是注册名称就实现了该能力。

表达式只能选择 Literal、Variable、EventField 之一，值限 int64、bool、最长 1024 字节的 string。条件支持 eq/ne，整数另支持 lt/le/gt/ge；不运行任意 Lua／Go 或网络查询。公开变量通过 `Project()` 投影，未写值显示声明默认值；私有变量只留服务端。

| 动作 | 用法与结果 |
| --- | --- |
| `set/add` | 指定 Scope、Name、Value；add 只接受整数，溢出拒绝整次操作 |
| `start/end` | Name 为目标阶段；启动已活动阶段报错，结束不存在阶段无操作 |
| `timer` | 当前阶段内命名的一次性计时器，Delay > 0；同名重设 |
| `cancel_timer` | 取消当前阶段同名计时器，包括本轮已入队但尚未派发的到期事件 |
| `request` | Name 为注册能力，Args 类型和字段必须精确匹配；只记录请求 |
| `finish` | Result 为终局标识；清理活动阶段，进入 Finishing |

内建事件：`level.start`、`stage.start`、`stage.end`、`timer{name}`、`action.done{id,name,success}`。外部 Handle 不能伪造内建事件。Start 在准备时间到达时启动 Initial 阶段，然后分派 level.start。

广播事件按阶段名称排序，各阶段触发器按定义顺序执行；后面的条件可读到前面动作写入的变量。阶段 start/end/finish 使用队列，当前事件的规则先处理完再变更生命周期，避免递归调用；预算包括队列和触发器执行。多个 finish 首个生效。正常 end 先发 stage.end 再清理；finish 是终局清场，不再发各阶段 stage.end，结算请求应显式写在 finish 前并设 `LevelLifetime=true`。

阶段结束清除其变量、触发次数、计时器，取消阶段生命周期的 pending 请求。重入同名阶段获得新 generation，旧定时器、定向事件和动作回执不能推动新阶段。LevelLifetime 请求可在原阶段结束后完成，但不会向新阶段投递旧 action.done。已在外部执行的动作不会因取消而自动撤销，宿主必须检查有效代际或用业务补偿处理。

## 两种玩法用同一组件

完整 JSON 在 [obstacle.json](../../gameplay/level/testdata/obstacle.json) 和 [waves.json](../../gameplay/level/testdata/waves.json)，类型注册表与执行断言见 [scenarios_test.go](../../gameplay/level/scenarios_test.go)。

**UGC 闯关示例**：宿主判定 `region.enter(actor, region, checkpoint_ready)`。checkpoint 阶段请求 checkpoint 能力，成功回执后切到 finish；合格的 finish 事实生成 settle 请求并终局。提前进入终点或资格不符不会完成关卡。`checkpoint_ready` 必须由服务器根据参与者事实计算，不直接相信客户端布尔值。示例仅演示一条阶段流程，不是完整多人检查点系统。

**合作 PvE 波次示例**：stage.start 请求 spawn_wave 并设置超时；enemy.dead 累计本阶段 kills；第一波完成后重入 wave，清除阶段计数而保留全局 wave；第二波完成生成 settle 请求。超时进入失败终局，旧波次回执不能影响新波次。victory 规则放在 next 规则前，避免同一事件中 wave 被更新后误判胜利。

两个例子的 checkpoint、spawn_wave、settle 均用测试替身，不表示 native PhysX 已具备传送／重生或永久奖励已到账。真实 UGC 关卡的形状、关节、移动平台仍由 [Physics Room](PHYSICS-ROOM-SERVICE.md) 加载；LevelComponent 负责流程，物理关卡描述负责世界，两份固定版本配置由业务一起绑定。

## 时间、去重、恢复与同步

- Tick 关卡在服务器模拟循环经 Nest 调用 `AdvanceTick`。毫秒关卡可将 `OnBeat` 注册到 heartbeat.Host，或取 `NextWakeup` 重新安排一个 entitytimer；组件不自动注册／启停宿主时钟。没有期限或尚未到期时 Advance 不写 DAO、不增 Revision。
- 到期计时器按 Due、阶段／名称稳定排序。晚到只消费原来已到期的单次计时器，重新设置的期限基于本次 Now；离线不会循环追帧。宿主处理所有状态变更后更新下次唤醒，不只在 Tick 后更新。
- 外部事件携带 Instance、Epoch、Source、Sequence；定向阶段另带 Generation。同来源必须有序，允许跳号；相同序列相同内容无操作，异内容冲突，更小序列拒绝。只保存每个来源最新水位，不是可查询全部历史回执的命令账本。事件目标、actor 和 source 均由认证宿主确定。
- 外部动作 ID 为 `instance:epoch:sequence`，同实例不复用。回执成功／失败均是终态，完全重复无操作，矛盾回执冲突；执行器超时且结果未知时不要伪造失败回执，应查账／按同一 ID 重试。业务要重新尝试已明确失败的动作，应由配置生成新请求。
- State 内含定义 hash、变量、阶段代际、计时器、水位和动作账本。hash 包含归一化定义和能力版本；恢复必须匹配，当前没有自动配置迁移。不要给语义不同的能力沿用同一版本。
- DAO 使用有界 JSON 字节封装进 BSON，保留完整 uint64 身份；写入采用整状态 mutation。Undo／State 两种回滚模式、提交拒绝、编解码恢复、错误定义绑定均有测试；尚未验证该组件真实 Mongo/WAL 多进程崩溃恢复，也不恢复 PhysX 世界。
- `Project()` 只含公开阶段、结果与 Public 变量，不含隐藏变量、source 水位、动作参数／账本。不要发送 Snapshot。参与者私有视图、鉴权、增量／ACK、重连基线由宿主接入 Sync；首版 Project 不是新的网络协议。

## 预算与验证范围

默认每次操作 512 步、128 个活动计时器、1024 个动作账本项、64 个来源；配置允许更小预算，硬上限分别为 4096、128、4096、64。定义最多 32 阶段、每阶段 64 触发器、128 变量声明，展开后的变量槽最多 8192；状态编码最多 2 MiB。动作账本不淘汰以复用 ID，满额拒绝，长期玩法应分实例并设计归档与永久去重。单来源适合服务器事件流；不要无界地按每个客户端连接创建来源。

每次有效写操作先复制状态，在草稿上完整执行成功后才替换 DAO；即使 handler 吞掉组件返回的错误也不会泄漏半个事件。复制和整状态序列化适合有界流程，尚无大规模吞吐承诺；不要每个物理接触点都建持久流程事件，可先由宿主汇聚语义事实。

已验证的入口：包级 race 回归覆盖配置、两种玩法、两种回滚模式、提交拒绝、版本恢复、定时器取消／重设、心跳迟到、重入／旧代际拒绝、事件重试和回执、公开投影与预算；生成消费者覆盖真实工厂、DAO 装配、生成 Sender、Nest 回滚、到期终局、动作回执和关闭。真实 UGC 物理动作执行器、Unity UI、网络弱网、持久崩溃恢复、多人完整玩法与容量仍需各自验收。

本轮检查（2026-10-26）：`go test -race -count=1 ./gameplay/level`、`bash scripts/test-level-generated.sh`、`go test -count=1 .`、全仓 `go build ./...` 与 `go vet ./...` 通过。全仓 `go test ./...` 未通过：Lockstep 的 `TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive` 在本机 Linux 出现停读未溢出与快照未完成，和 [此前 main 对照记录](PHYSICS-ROOM-SERVICE.md#共享索引与多房间测量2026-10-26)相同；没有修改该用例或将全仓检查记为通过。
