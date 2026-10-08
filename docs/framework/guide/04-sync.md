# 核心：Sync、Lockstep 与客户端：设计与使用

适用运行时：v1.24.0；文档维护版：v1.24.1。[实现与维护入口](../impl/04-sync.md) · [模块总目录](../README.md)

## 先理解这一块

服务器的数据改变后，客户端需要知道哪些变化；对局也可能需要把玩家输入按帧转发。Sync提供这些同步能力。

背包和位置通常看状态同步，按输入推进同一场战斗看Lockstep。两种方式解决不同问题，不能直接互换。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 子系统分工

entitysync 维护 subject、session、subscription、profile 与每会话帧；policy 提供 Direct/Group/Interest/AOI 的可见关系。frame 定义帧格式，nettransport 负责发送能力与背压。syncbus/mirror 是服务间复制，syncstream 是带基线/增量与 journal 的流。lockstep 传输输入、帧与校验，不替业务保证跨引擎确定性。

## 2. 状态同步流程

注册 subject → OpenSession（可 held）→ Subscribe → ReadySession → Flush/运行循环。held 会话可以建立订阅但不出帧；ready 后从新 epoch 全量开始。多个来源订阅同一 subject，各自持有引用，最后一个离开才真正取消。subject 保存订阅真相，session 保存其对象生命周期关系。

PrepareViews 汇总所需 profile；优先级配置先参与选择，再按确定顺序选择视图。视图必须限制字段可见范围，不能因空视图或缓存复用扩大权限。快照预算限制尚未持有对象的冷创建；增量、remove 和已持有对象的全量不能被当成同一种冷创建计费。

## 3. periodic 与 on_change

periodic 按 Interval 合并检查；on_change 在正式 handler 提交边界汇总：setter 只标脏，持 Guard 冻结需要的视图，解锁且满足持久确认后唤醒。唤醒可合并，pending 才是事实来源，不能依赖每次通知恰好被接收。两种模式共用订阅、版本和线协议。

配置 DurableWatermark 时，冻结状态的 CommitLSN 超出水位则暂不发送。不要把 MarkDirty 的时刻当成客户端可见时刻，也不要把 on_change 描述成每个 setter 都立即发包。

## 4. 背压与恢复

状态全量、增量、remove 走可靠有序通道。Push 返回 nil 仅表示传输接纳；ErrRetryLater 保持脏状态等待重试。基线缺口、epoch 切换或未知对象必须请求/等待全量恢复，客户端不继续在错误基线上应用 delta。会话替换与旧异步回调必须按代际核对。

## 5. 服务间总线与流

JetStream SyncBus 默认有限投递次数 5，不能宣称无限重试或绝不丢。topic 创建/退役期间拒绝冲突订阅，旧 consumer 和在途回调排空后才能重订。无 handler 准入不 ACK；业务错误与坏信封按各自规则结算。流容量、保留期限与次数上限均可能终止投递，恢复由快照或业务对账承担。

syncstream 碎片 TTL 在新多片输入时清扫，不是独立定时器。FileJournal 直接并发 Append 可合批，但单一 History.Record 的写锁串行化不能据此承诺每业务记录减少 fsync。订阅停止、回调排空、journal 生命周期须一起关闭。

## 6. Lockstep

客户端 Command 提供 Input/Hash/Catchup；Room 根据当前 session 映射席位，不信任客户端自报 seat。Room 汇集输入、广播 C7 帧和历史追帧；历史需宿主主动 TrimBefore/TrimHistory，不能假定自动无限期有界。输入校验、hash 判定、重连追帧与游戏模拟分开。两人不形成多数时 NoMajority 不等于已经识别作弊者。

## 7. 公共客户端协议

RS v2 的 16 字节头保留 MsgID、Seq、PayloadSize；Flags bit0 是 push，bits1..2 区分 PB=0、Sync=1、Lockstep=2，值3及保留位拒绝。Lockstep 上行 flags=4，push=5。MsgID 0 的心跳不能携带业务类型标记。

C# 客户端提供 TCP、Sync receiver、Lockstep codec/assembler，Unity 提供主线程适配源码。Go wire 与生成 TCP 共享头验证；业务载荷类型须与注册路由一致。正式 Go/C# 及 Unity 源码测试不代表 Unity/Godot/Unreal 实机已验收；原生 C++ 客户端不在本版交付范围。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
