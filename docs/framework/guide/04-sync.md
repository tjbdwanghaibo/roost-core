# 核心：Sync、Lockstep 与客户端：设计与使用

适用版本：v1.25.0 及当前主线（未发版变化见 [CHANGELOG](../../../CHANGELOG.md)，未发版 API 在对应章节注明）。[实现与维护入口](../impl/04-sync.md) · [模块总目录](../README.md)

## 先理解这一块

服务器的数据改变后，客户端需要知道哪些变化；对局也可能需要把玩家输入按帧转发。Sync提供这些同步能力。

背包和位置通常看状态同步，按输入推进同一场战斗看Lockstep。两种方式解决不同问题，不能直接互换。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 子系统分工

当前主线将同步流、对局和空间区域分别命名为 `StreamID`、`lockstep.Match`、`AreaID`。这是 v1.25.0 之后尚未发版的源码 API 调整，升级映射与验证见[维护记录](../../maintenance/ACCESS-SYNC-FOLLOWUPS.md)。流标识只在连接/同步上下文内有意义，Entity Sync 当前填入 1；不要把它当地图或副本 ID。

entitysync 维护 subject、session、subscription、profile 与每会话帧；policy 提供 Direct/Group/Interest/AOI 的可见关系。frame 定义帧格式，nettransport 负责发送能力与背压。syncbus/mirror 是服务间复制，syncstream 是带基线/增量与 journal 的流。lockstep 传输输入、帧与校验，不替业务保证跨引擎确定性。

工作分支已实现 [Block AOI 与手写 SpatialComponent](../INTEREST-BLOCK-AOI.md)：事实入队使用独立短锁，批次更新空间后并行计算observer；同一实体可在多个块被观察，实际位置仍唯一。内容发布必须同时满足提交门和政策事实已处理。接入不改Guard回调或生成器；完整重档性能尚待复验。

## 2. 状态同步流程

注册 subject → OpenSession（可 held）→ Subscribe → ReadySession → Flush/运行循环。held 会话可以建立订阅但不出帧；ready 后从新 epoch 全量开始。多个来源订阅同一 subject，各自持有引用，最后一个离开才真正取消。subject 保存订阅真相，session 保存其对象生命周期关系。

PrepareViews 汇总所需 profile；优先级配置先参与选择，再按确定顺序选择视图。视图必须限制字段可见范围，不能因空视图或缓存复用扩大权限。快照预算限制尚未持有对象的冷创建；增量、remove 和已持有对象的全量不能被当成同一种冷创建计费。

### 好友、队伍等关系如何更新

关系源必须先通过 Interest 配置声明，subject 和接收 session 也需要完成注册。已掌握完整成员表时用 `QueueRelation` 全量替换；只知道哪些成员加入、退出时，用当前主线新增、尚未发版的 `QueueRelationDelta`。例如好友列表有 1000 人，只增删 1 人时，不必再发送整张列表。

业务在已有 Nest handler 中先修改自己的关系 DAO，再调用以下入口；调用失败必须向 handler 返回错误。框架只维护同步可见关系，不替业务保存好友或队伍数据。

```go
// 已处于保护 player 的 Nest handler 内；team 是初始化时声明的关系源名。
if err := interest.QueueRelationDelta(player, "team", joinedIDs, leftIDs); err != nil {
    return err
}
```

两个 ID 切片在入队时复制，返回后可复用；空切片表示该方向无变化，两个都为空也仍走正常准入检查。单次调用中同一 ID 同时出现在两边时，退出优先；重复 ID 不重复发事件，零 ID 忽略。多次调用按同一实体的事实顺序执行，后一次全量替换会覆盖前面的增量结果。

只有提交确认且政策事实应用后才发布内容；回滚丢弃增量。Leave 会作废观察者自己的旧事实，Hide 只撤销被观察身份；退出 ID 会从尚未应用的增量中剔除，防止同 ID 重建时被旧新增/移除影响。移除 team 来源不会取消其他来源仍持有的订阅。

底层 `RelationSource.ApplyDelta` 是立即更新的政策 API，不参与事务确认；普通业务 handler 应用上面的排队入口。增量与全量对照、边界测试和剩余优化项见 [Sync 优化说明](../SYNC-OPTIMIZATION.md)。

## 3. periodic 与 on_change

periodic 按 Interval 合并检查；on_change 在正式 handler 提交边界汇总：setter 只标脏，持 Guard 冻结需要的视图，解锁且满足持久确认后唤醒。唤醒可合并，pending 才是事实来源，不能依赖每次通知恰好被接收。两种模式共用订阅、版本和线协议。

配置 DurableWatermark 时，冻结状态的 CommitLSN 超出水位则暂不发送。不要把 MarkDirty 的时刻当成客户端可见时刻，也不要把 on_change 描述成每个 setter 都立即发包。

### 可配置合批窗口（当前主线新增，尚未发版）

希望将短时间内多次已提交更新集中处理，可以设置：

```yaml
sync:
  entity:
    mode: on_change
    interval: 50ms
    on_change_batch_window: 5ms
```

代码配置对应 `entitysync.ManagerConfig.OnChangeBatchWindow`。缺省为 0，保持原来的立即处理唤醒；正值从循环收到第一次唤醒时开始计时，后续通知不延长这次窗口。周期检查可提前触发，因此持续写入不会一直等下去。5ms 是示例取舍，不是端到端延迟保证：Flush 执行、队列、提交确认、网络和重试仍需要时间。

只有 on_change 能设置非零窗口；负值或 periodic 搭配非零值会拒绝启动。配置文件显式写 0 可覆盖代码中的非零窗口，`EntitySyncSetup.Configure` 最后执行，仍有最终覆盖权。启动日志会打印实际窗口。

手动 `Flush`、`Drain` 和 `Stop` 的最后一次 Flush 不等窗口；周期 Interval 继续作为兜底与原有快照预算窗口，不能把合批配置当成提高冷创建额度。该选项只改变发送循环时机，不减少 handler 的捕获次数，也不改变提交水位或可靠增量规则；冻结数据可能保留更久，应按实际内容测量峰值内存和延迟。单独执行 Flush 不会取消循环中已经安排的计时器，之后可能多一次空 Flush。

## 4. 背压与恢复

状态全量、增量、remove 走可靠有序通道。Push 返回 nil 仅表示传输接纳；ErrRetryLater 保持脏状态等待重试。基线缺口、epoch 切换或未知对象必须请求/等待全量恢复，客户端不继续在错误基线上应用 delta。会话替换与旧异步回调必须按代际核对。

## 5. 服务间总线与流

SyncBusMod 的主题前缀缺省 `roost.sync`，与驱动缺省、生成配置一致；JetStream 流名显式 `syncbus.stream` 优先，否则由前缀推出（`roost.sync` → `ROOST_SYNC`，`zz.sync` → `ZZ_SYNC`），共用一个 NATS 的不同前缀各有各的流。没有按旧前缀的特例映射：要沿用已有的流与 durable 游标只能显式写 `syncbus.stream`；新流与已有流的 subjects 重叠时启动明确失败，不静默换流。

JetStream SyncBus 默认有限投递次数 5，不能宣称无限重试或绝不丢。topic 创建/退役期间拒绝冲突订阅，旧 consumer 和在途回调排空后才能重订。无 handler 准入不 ACK；业务错误与坏信封按各自规则结算。流容量、保留期限与次数上限均可能终止投递，恢复由快照或业务对账承担。

syncstream 碎片 TTL 在新多片输入时清扫，不是独立定时器。FileJournal 直接并发 Append 可合批，但单一 History.Record 的写锁串行化不能据此承诺每业务记录减少 fsync。订阅停止、回调排空、journal 生命周期须一起关闭。

## 6. Lockstep

客户端 Command 提供 Input/Hash/Catchup；Match 根据当前 session 映射席位，不信任客户端自报 seat。Match 汇集输入、广播 C7 帧和历史追帧；历史需宿主主动 TrimBefore/TrimHistory，不能假定自动无限期有界。输入校验、hash 判定、重连追帧与游戏模拟分开。两人不形成多数时 NoMajority 不等于已经识别作弊者。

### 长局如何控制内存

先按允许的断线时长确定保留帧数。例如 30Hz 保留最近一分钟，就是 1800 帧。宿主在驱动 Match 的同一串行 handler 内按以下方式裁剪；不需要另起 goroutine：

```go
// frame 是本次 match.Tick 返回的帧；需要更长重连窗口时提高 keepFrames。
const keepFrames lockstep.FrameID = 1800
if frame.ID >= keepFrames {
    match.TrimHistory(frame.ID - keepFrames + 1)
}
// 如果业务上报了模拟状态 hash，也应定期在同一串行 handler 中调用
// match.TrimHashReports(保留起点)，避免只裁历史而遗留 hash 报告。
```

当前主线裁剪会立即清除丢弃帧的引用，通常只推进切片；大幅裁剪才收缩数组。这样频繁裁掉一帧不再复制整个保留窗口。`History.ReadRange` 和 `Replay` 返回只读视图，仅在下一次 Append/TrimBefore 前有效；需要长期保存回放时自行复制并持久化，不能留着内部切片当独立回放文件。异步追帧已在启动发送前编码成独立字节，不依赖后续可变历史。

早于保留起点的追帧会以 `ErrCatchupUnservable` 明确失败，业务应选择完整状态恢复、重新入场或其他产品策略。保留窗口越短越省内存，也越容易使慢追帧失效。框架不会静默补齐被裁掉的历史。本机已验证一小时逻辑帧和滚动追帧；这不是一小时真实网络运行或跨引擎确定性的证明，测量见[优化说明](../SYNC-OPTIMIZATION.md)。

## 7. 公共客户端协议

RS v2 的 16 字节头保留 MsgID、Seq、PayloadSize；Flags bit0 是 push，bits1..2 区分 PB=0、Sync=1、Lockstep=2，值3及保留位拒绝。Lockstep 上行 flags=4，push=5。MsgID 0 的心跳不能携带业务类型标记。

C# 客户端提供 TCP、Sync receiver、Lockstep codec/assembler，Unity 提供主线程适配源码。Go wire 与生成 TCP 共享头验证；业务载荷类型须与注册路由一致。正式 Go/C# 及 Unity 源码测试不代表 Unity/Godot/Unreal 实机已验收；原生 C++ 客户端不在本版交付范围。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
