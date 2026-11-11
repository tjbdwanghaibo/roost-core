# scene：场景、分线与附近公开视图

`roost module add scene` 自动安装 actor、Nest、heartbeat、Entity/DAO/协议与 Player TCP self_sync/lifecycle 接线。启动时打开编号 1 的默认场景：plain、分线 1、范围 [0,1000)×[0,1000)、出生点 (50,50)、视距 64、容量 1000。场景由配置重建，运行期成员和时间域不持久恢复。

## 正式入口与一致性

`game/scene/service.Manager.Open` 只供可信服务端开图，最多 128 张，每张容量最多 10000。构造和生命周期操作在 Nest worker 外；客户端不能选择配置或创建地图。配置切片在创建时复制，Ground/Surface 的提供者必须支持并发读取已提交状态。

`SceneChange` 的 PlayerID 来自认证上下文。Controller 解析旧图/新图，生成的 MultiGroup Sender 把 Player 与最多两张 Scene 放进同一 Nest undo 事务：锁内重新检查实际归属、容量和关闭状态，离开旧图、存档、进入新图一起成功或撤销。旧 `FromID` 不匹配就拒绝；同图重连保留站位并重订阅。归属在 Actor/Scene 成员账本，AOI 不是授权依据。

普通 `Close` 拒绝仍有成员的场景。可信服务端 `Drain(ctx, id)` 只撤离并关闭指定地图，供 terrain 等模块释放该图资源；失败可重试，不影响其他地图或继续开图。`Stop` 先禁止新开图，再逐图关闭准入，经 Player＋Scene 事务撤离玩家，最后经 Entity lifecycle 销毁空图。依赖模块通过 RegisterDrainer 按实体kind登记正式退出协调器；monster启动时自动登记，关图时先关准入，再在Manager锁外清理该图怪物（含死亡待重生槽），其他图继续运行。结束时按Scene实例复核，迟到清理不会关闭同号新图。未登记的其他实体仍占位、退出/离场钩子或提交失败时返回错误并保留场景，后续 Stop 可以重试。卸载角色残留的成员在 Admit/Close/Stop 持锁核实后清理，AOI/感知索引在提交后才清；仍加载但断线的角色继续占容量，方便重连，不靠 TCP 断线事件擅自改成员。

该模块管理本进程实体，不承诺跨 game 进程切图、全服同图成员或分布式容量。跨服迁移需要另行实现所有权交接与恢复协议。

## 空间、时间、区域与交互

一个 Space 同时承接 movement.Space、AOI Interest 与独立的已提交玩家站位索引。进出与位移只在提交后影响旁观者。`Sense` 只返回同场景玩家的已提交位置，查询半径不得超过视距；它适合 AI/附近消息候选，不能代替伤害事务中的归属、距离和存活代际重查。场景元数据通过全场关系视图下发，独立 TimeDomain 的倍率范围 0–4。

Config 支持最多 64 个静态区域、256 个静态交互对象。`OnRegion` 在 Player 位置变化的同一事务中得到进入/离开事件；`OnSceneInteract` 在场景、距离和连续序号校验之后触发。重复最后一次相同请求返回上次应答，序号跳跃、过期、同序号改内容均拒绝。只保留最后一份交互回执。回调只修改已持锁实体，错误原样返回，不能在回调内发 RPC 或加载别的实体。需要有状态的 NPC/建筑应建立独立实体，不能把共享可变业务状态塞入 Config。

## 公开字段白名单

Player 使用 `NewActorPlayerSyncPacker`。本人视图包含所有可同步字段，附近视图只包含 `OnSceneFields` 在 init 明确登记的顶层字段；快照和增量使用同一掩码。未登记字段默认私有，已有自定义 packer 的工程安装时拒绝覆盖。

| 安装组合 | 附近公开字段 | 仍私有的示例 |
| --- | --- | --- |
| scene | Actor | ActorSaved、SceneInteraction |
| with profile | Profile（名字、等级） | ProfilePrivate（经验） |
| with attribute | AttributeFinal | AttributeBase、AttributeGear |
| with equipment | Equipment | Bag |
| with combat | Combat（战斗公开投影） | CombatPrivate、附加 combat_state 权威 |

`OnSceneFields` 登记的是整个顶层字段；新增公开字段之前应检查其嵌套内容。不得把 `*` 用作附近白名单。各联动都实际解码快照/增量，检查公开值存在、私有值不泄露且本人仍能看到私有值。

## with chat

安装 chat 后，原有 `ChatSend` 支持 `Kind="nearby", Target=0`。统一端点先验证 UTF-8/256 字节上限，再检查发送者在本进程有连接，通过 Nest 读取其场景与位置，选取同场景视距内已提交的玩家站位快照。离开 Nest 后只向本进程仍有连接的收件人推送 `ChatMessage`，包括发送者自己；不会持实体锁做网络写。

这是即时消息：`Seq=0`，没有 chat 服务持久历史、去重、离线补发或跨服务器广播。ChatHistory 拒绝 nearby。收件范围以取快照时为准，之后移出场景的角色可能收到已经发出的消息；网络推送失败不回滚任何游戏状态。世界/私聊继续走原来的 chat 服务政策和历史。

## 验证

`roost module verify scene` 固定 26 个基础场景、零跳过；with chat 为 5，with profile/attribute/equipment 各 2；combat 模块拥有的 with scene 另有 2。正式组合安装验收还覆盖重复安装不漂移、构建、vet、ID 和生成检查。变异测试实际运行可编译的错误实现，覆盖占用场景关闭、交互重放、关闭准入缺失、私有快照/增量泄露与离线收件人推送。

这些离线场景使用实际 Controller、生成 Sender、Nest 多实体事务、DAO、movement、Sync AOI 和时间域，提交器/网络传输受控替换；不声称已验证真实数据库、跨进程 RPC、TCP 链路或性能容量。
