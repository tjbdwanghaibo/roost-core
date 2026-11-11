# actor：共享角色运行体

`roost module add actor` 在新 game 工程安装 Player 的 Actor 组件、位置 DAO、ActorMove/ActorStop 协议及 heartbeat 依赖。actor 不要求 scene；房间可以提供自己的 `actor.Environment`，`actor.Nowhere` 可用于没有空间观察者的房间。Monster 复用同一个 `actor.Body`。

## 状态与执行边界

- `Actor` 保存当前 SceneID、坐标和朝向，`nopersist,sync`。每拍位置走 DAO 与 Sync，不写持久日志。
- `ActorSaved` 保存上次落盘的归属和位置，`persist,nosync`。每 10 秒及离场时保存；不承诺崩溃前最后一拍恢复。
- Body 组合 `movement.MapBasic`、`movement.Move`、`state.Set`、`heartbeat.Host`。200ms 移动拍由正式 heartbeat 驱动；路径、移动令牌、环境、心跳启停和坐标均参与 Nest undo。
- `Rooted` 阻止推进移动，`Silenced` 阻止施法检查。状态集合是进程内临时状态，不持久恢复。`CanCast` 供 combat 使用，actor 本身不实现伤害。

所有 Body 方法（除底层明确发布的只读快照）都必须在所属实体锁内调用。`Walk` 默认最多 256 个点；可信构造方可用 Config.MaxPathPoints 调整，上限 4096。客户端只提交目标坐标，正式 Controller→Sender→Nest 入口使用服务端固定速度 60。可信服务端可调用 `Walk/Follow/Stop`，但仍必须持锁并把错误返回 Nest。

共享实现只保存在 `files/game/actor/body.go.tmpl`。game-demo 消费同一模板并用自己的 Player 字段和 cellworld 接线适配；目前共用移动机制，并非完整安装 actor/scene 模块。`MoveHeartbeat` 返回正式注册供持锁诊断，启动与停止仍走 Walk/Follow/Stop；可选 Config.Surface 按拍读取可编辑地面。

Environment 由所在场景/房间提供：Space 只接收提交后进出与位移，Ground/Surface 校验每步站立与通行，Time 可提供独立时间域。跟随目标通过实体已发布的位置快照读取，不锁另一个玩家。跨场景目标不能直接追踪。每拍域时间有上限，不因延迟补跑无界帧数。

`OnActor` 只在 init 登记 Hooks。Enter、Leave、AfterTick 都在当前事务内执行，失败会撤销本次变更；回调不得发 RPC、取得其他实体锁或启动异步业务。需要多实体修改时由上层显式组织 Nest 参与者。Body 不持有 world-global Scene，也不把 actionflow runner 的非回滚状态塞进移动权威。

## 同步视图

Player 只有 `NewActorPlayerSyncPacker` 一个工厂，明确打包 PlayerDao。本人视图包含该DAO全部可同步字段；附加的CombatDAO等服务器权威不参与这个载荷。其他模块在init通过 `OnActorSyncFields` 登记有限视图与显式字段；scene沿此扩展附近白名单，不再换第二份工厂。已有明确业务packer的工程会拒绝安装，需业务自行接入权限规则，不能自动覆盖。

## 验证

`roost module verify actor` 固定 12 个独立场景、零跳过，并执行构建、vet、ID 与生成漂移检查。场景覆盖独立房间、PB/服务端速度、定身与恢复、位移和提交拒绝回滚、地形、路径限额及正式 heartbeat。离线测试使用实际 Nest/DAO/movement/heartbeat，提交器与空间替身用于控制失败，不代表真实数据库、TCP 或吞吐压力验收。
