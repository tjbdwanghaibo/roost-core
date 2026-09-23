# ARCH-11：从 wdsync 借两件事——多视图优先级合并、实体 × 视图共享编码 + 网关扇出

- 状态：**方案**（维护者 2026-09-23 要求先出方案），未实施；实施时各占一个 M 编号
- 来源：对照 ssr `kingdom/wds/troop.wds.go` 与 `watcher/wdsync` 的同步实现（观察者表 `EntityEasyncable`、视图优先级 `compareViewWithObserved`、`syncEntityChangesTo` 的一份 payload 多连接投递、`ViewCache` 原地补丁）
- 前提：ARCH-10 已落地（M-13 / M-14）。两件事都在**机制层**（`entitysync`）内完成，组织层与内容层不动；第二件涉及 `Transport` 契约的可选扩展与一种新的帧模式。

## 1. 多视图（多 profile）优先级合并

### 现状与问题

`Manager` 里一对 (session, subject) 只有一个 profile；`policy.Interest` 内部用来源计数把空间与关系合成一个 band，但**跨组织**没有计数：`Group` 与 `Interest` 同时持有同一对时，一方 `Unsubscribe` 会撤掉另一方还想要的订阅（M-14 记录的边界）。wdsync 的做法：一个连接对同一实体可以同时持有多个视图（默认视图 + AOI 按 LOD 注册的额外视图），每视图一个计数，同步时只发**优先级最高**的那个视图（`ViewMatrix` + `compareViewWithObserved`），低优先级视图的变化被跳过。

### 方案

订阅由"一个 profile"变成"一组带计数的 profile，取其中最优的一个发"：

```go
type subscription struct {
	profiles map[entity.SyncProfile]int // 每个 profile 被多少个来源/政策持有
	active   entity.SyncProfile          // 当前生效的：优先级最高者
	kind     subscriptionKind
	baseVersion uint64
}
```

- `Subscribe(session, subject, profile)`：`profiles[profile]++`；若 profile 优先级高于 `active` → `active = profile`，`kind = 需要快照`（新视图下重发全量，与今天换 profile 的行为一致）；否则只计数。
- `Unsubscribe(session, subject, profile)`（**新增 profile 参数**）：`profiles[profile]--`，归零删除；若删掉的是 `active` → 回落到剩余最优者并 `kind = 需要快照`；集合空 → 现有的"正在离开 / 直接删"逻辑。
- 优先级规则（`SyncProfile` 上的全序，写成 `func (p SyncProfile) MoreDetailedThan(q) bool`）：`LOD` 小者优先（0 = 最近、最细），同 LOD 按 `Key` 字典序，再按 `SchemaVersion`。这与 `policy.Interest.bestBand` 的规则一致，后者以后可以退化成"每个来源直接 Subscribe 自己的 profile"，聚合交给 Manager；本方案先不动 Interest。
- 帧不变：一个会话一帧里一个 subject 仍只出现一次（用 `active` 的 payload）；`PrepareTick` 的 profile 集合按 `active` 收集，低优先级的 profile 不 pack。

### 收益

- 跨组织重叠有了正确语义（引用计数），M-14 那条边界消失。
- 一个会话通过多条路径看同一实体（AOI 近距 + 队友关系 + GM 盯人）时只收最细的一份，不重复、不打架。
- 换路径时（队友走远：team 视图仍在，spatial 近视图撤）自动回落，客户端收到一次全量而不是"什么都没变"。

### 代价与取舍

- `Unsubscribe` 签名破坏性变化；政策要记住自己订的 profile（Interest / Group / Direct 都记得）。
- 回落时重发全量而非"从细到粗的差量"：简单、正确；差量形式需要 profile 间的字段包含关系，那是 ARCH-06 字段词汇表之后的事。
- 每个订阅多一个小 map；订阅数 × 路径数级别，可忽略。

### 验收覆盖点

两个政策持同一对不同 profile → 只发更细的；撤掉更细的 → 回落并重发全量；全部撤掉 → remove；同一政策重复订同一 profile → 计数不重复出帧。

## 2. 实体 × 视图共享编码 + 网关扇出

### 现状与问题

wdsync 以"实体 × 视图"为单位编码**一份** payload，一次交给该视图的全部观察者（`SyncEntity(memo, payload, userConns, connSorted)`），扇出在网关：一支被上千人看的部队每 tick 只编码两次。roost 以"会话"为单位出帧：编码在 `session.encode` 里逐会话做，同一 subject 同一 profile 的组件字节被重复编码 N 次，且每帧单独 push。roost 这样做换来的是每会话一帧的一致性、会话私有时钟与失败域，不应放弃。所以方案分两层：先拿计算上的收益（不改线、不改契约），再把带宽上的收益做成**可选帧模式**。

### 第一层：tick 内组件编码缓存（无线格式变化）

同一 tick 内，同一 (subject, profile, 全量/增量) 的组件字节对所有会话是**完全相同**的——所有在线会话的 base 都是 subject 的当前版本（M-13 的不变量：会话要么在线且与 subject 同版本，要么需要快照）。因此 `Manager.Flush` 里加一张 tick 级缓存：

```go
type encodedKey struct{ subject int64; profile entity.SyncProfile; full bool }
cache map[encodedKey][]byte   // EncodeSubjectUpdate 的结果，一 tick 一份
```

`session.encode` 取缓存而不是重新 `EncodeSubjectUpdate`；帧组装只剩每会话的 statesync 外层（ObjectRef、Epoch/Tick）与一次 memcpy。旧 room sink 曾有 `componentCache`，M-13 丢了它，这一层等于找回来并放到正确的位置（Manager 的 tick 作用域）。
收益：编码开销从 O(会话 × 订阅) 降到 O(变化的 subject × 不同 profile)。改动约 40 行 + 一条基准（N 会话 × M subject 同 profile，pack 次数 == M）。

### 第二层：共享帧模式 + 网关扇出（可选，需要新的传输能力）

真正省带宽要求服务端到网关只发一份。做法是给 `Transport` 加一个**可选**接口，并给 Manager 加一种帧模式：

```go
// 可选：能把同一帧交给多个会话的传输（网关在另一端复制）
type MulticastTransport interface {
	PushMany(ctx context.Context, sessions []SessionID, frame []byte) error
}
```

- **共享帧的单位是 (subject, profile)**，不是会话：帧头的 `Epoch/Tick` 换成 subject 的 `version/baseVersion`（statesync 帧的 `RoomID` 位放 subject 的 namespace hash，`Epoch` 固定 1，`Tick`=version），一帧一个对象。这样同一 (subject, profile) 的帧对所有在线会话**字节相同**，可以 `PushMany`。
- Manager 按 `ManagerConfig.FrameMode` 二选一：
  - `FramePerSession`（默认，今天的行为）：一致性、会话时钟、失败域 = 会话。
  - `FramePerSubject`：每个变化的 (subject, profile) 一帧，`PushMany` 给该 profile 的全部在线会话；快照与 remove 仍按会话单发（只发给新订阅者 / 离开者）。失败域变成"这一帧对这批会话"，`PushMany` 的部分失败要由传输返回失败的会话列表（`PushMany` 返回 `[]SessionID`，或 `MulticastError{Failed []SessionID}`），Manager 对失败者 `loseSession`。
- **客户端侧**：`FramePerSubject` 下客户端按 subject 维护版本链（今天组件里已经带 `Version/BaseVersion`，机器人也是按 subject 合并的），丢掉的只是"一帧 = 一刻的世界"这个单位。这是产品层面的取舍，写进配置注释。
- **网关**：demo 的接入层是 player TCP，一玩家一连接，没有网关，`PushMany` 退化为循环 `Push`（收益只剩第一层）。真正的扇出需要 nettransport 之上有一个"网关会话"概念：Manager 看到的一个 `SessionID` 对应网关上的一组终端连接，`PushMany` 一次到网关。roost 今天没有这个组件；本方案只定义传输契约，网关是另一条线。

### 借与不借

- 借：tick 级编码缓存（立即）、`MulticastTransport` 契约与 `FramePerSubject` 模式（有网关时）。
- 不借：wdsync 的 `ViewCache` 原地补丁（全量快照缓冲由 setter 就地改写）。它依赖字段级生成的偏移表，是 codegen 的事，等 ARCH-06 字段词汇表落地后作为 packer 优化单独评估。
- 不借：事务边界即发。roost 的 tick 是持久化门槛与批量的前提，改成即发会绕过门槛。

## 3. 实施顺序建议

1. M-15：多 profile 引用计数 + 优先级（§1），`Unsubscribe` 加 profile 参数，三个政策跟随，promise test 四条。
2. M-16：tick 级编码缓存（§2 第一层）+ 基准。
3. 待网关线立项后：`MulticastTransport` + `FramePerSubject`（§2 第二层）。
