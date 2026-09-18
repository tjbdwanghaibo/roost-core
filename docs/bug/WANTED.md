# Wanted：实现侧提交的待审查候选

这里放**实现 / bugfix 一侧在干活时看到、但自己不该拍板的疑点**。它们不是 RR：没有编号、没有状态、不进覆盖矩阵。
review agent 每轮看一眼，对每条做三选一——登记为 RR（分配编号、写 REVIEW-*.md、移出本表）、判为"不是问题"（写一行结论后移出）、
或"再观察"（留着，写明还缺什么证据）。实现侧**不要**据此直接改码；这张表存在的意义就是把"发现"和"定契约"分开。

格式：一条一个二级标题，写清位置（仓 / 文件 / 行 / SHA）、现象、为什么觉得可疑、能怎么复现、候选修法（可选）、来源。

## 已分流记录（W-2026-09-16-01 已分流）

W-2026-09-16-01 已于 2026-09-16 登记为 [RR-20260916-05](REVIEW-2026-09-16-04.md)，不再属于待审表。确认的是策略注入承诺无效；原草稿预设 Enqueue/Sweep 自动成组，与当前 Store 契约不符，不直接作为修复测试。建议保留调用方驱动，移除无效 Mod/Config/codegen 注入入口。具体实施与验收以链接文档为准。

### W-2026-09-16-01 原始候选（仅保留来源，不代表最终方案）

- **位置**：roost-kit `527eecd`，`service/match/match_mod.go`（`NewMod(grouping Grouping, …)`，头注释"Grouping is a constructor
  argument because 'which candidates form a match' is the whole of a game's matchmaking policy"）；`service/match/queue_store.go:118`
  （`Config.Grouping`）与 `:159-161`（`if cfg.Grouping == nil { cfg.Grouping = FirstComeGrouping{} }`）。
- **现象**：`grep -rn '\.Group(' service/match/*.go | grep -v _test | grep -v grouping.go` 为空——`cfg.Grouping` 被赋值后再没有被读。
  `Enqueue` 只入队，成组完全由调用方 `Candidates → Commit` 驱动；`Sweep` 只处理过期票。生成工程的
  `internal/service/match/collaborators.go` 因此有一个 `Grouping()` 返回 nil 的函数，注释却说"replace it with the project's own rules"。
- **为什么可疑**：Mod 的公开签名与注释承诺了一个策略注入点，运行时不履行。一个项目实现了自己的 `Grouping` 交给 `NewMod`，
  会以为匹配按它的规则进行，实际仍是调用方（若有）拿 `Candidates` 自己配对——静默失效，属于 C2（承诺无实现）/ C4（跨包契约不一致）一类。
- **复现（会红的测试草稿）**：
  ```go
  // service/match/grouping_wired_promises_test.go
  type recordingGrouping struct{ calls atomic.Int32 }
  func (g *recordingGrouping) Group(q Queue, c []Ticket) ([]Ticket, bool, error) { g.calls.Add(1); return FirstComeGrouping{}.Group(q, c) }
  // 用现有 harness 建一个 Store（Config.Grouping = &recordingGrouping{}），Enqueue 两张不同 subject 的票到 GroupSize=2 的队列，
  // 再 Sweep 一次；断言 g.calls.Load() > 0，或第二张票的 State 为 matched。当前实现两者都不成立。
  ```
- **候选修法（定契约后再选）**：
  A. 服务端驱动——`Enqueue` 末尾在同一次 CAS 里 `Grouping.Group(queue, waiting)`，成组即 `Commit`，票据直接以 matched 返回；
     调用方不再需要 matchmaker 循环，但 `Grouping` 进入热路径，且要论证与 `Commit` 的原子性一致。
  B. 删掉 collaborator——`NewMod` 去掉 `grouping` 参数，`Grouping` 保留为调用方工具；codegen `internal/roost/framework_services.go`
     的 match collaborators 模板同步删 `Grouping()`。改动更小，也让签名与行为一致。
- **来源**：game-demo 第五批实施时发现（roost-core `docs/feature/GAME_DEMO_TEMPLATE.md` §7.5）；demo 的
  `internal/service/<game>/matchmaker.go` 直接调 `svcmatch.FirstComeGrouping{}.Group`，是这个接口目前唯一的使用方式。

## 09-17 第二轮已分流：W-2026-09-17-01

不单列功能 bug；已转 [ARCH-05 七包迁移交接](../review/IMPLEMENTATION-SERVICE-PLACEMENT-AND-RECOVERY.md)。六个服务的领域实现迁 Core，directory 先迁，Kit 保留装配；尚未实施。审查另发现 [RR-20260917-04](REVIEW-2026-09-17-02.md)。下面保留来源，不再属于待审候选。

### W-2026-09-17-01 原始候选：其余六个 kit RPC 服务（account / chat / global / activity / platform / rank）是否按 M-06～M-11 的形状下沉 core

- **位置**：roost-kit `4830150`，`service/account`（`Accounts`，`account_rpc.go:41`）、`service/chat`（`Messaging`，`chat_rpc.go:66`）、
  `service/global`（`Routing`，`global_rpc.go:45`）、`service/global/activity`（`Coordinator`，`activity_rpc.go:39`）、
  `service/platform`（`Platform`，`platform_rpc.go:46`）、`service/rank`（`Rank`，`rank.go:23`）。六个包的领域文件
  （types / service / store / redis_store / identity / admin / member）目前只 import core 与 kit 的 `mods`、`service/servicemetrics`（后者已是 core 别名）；
  account 另有 `service/directory`。
- **现象**：ARCH-01 点名的 session / mail / match 已按"领域实现 + RPC 接口 + 传输半进 core，kit 留 Mod / 装配半 / 别名"的形状完成
  （M-06～M-11，core v1.15.5 / kit v1.14.6 / codegen v1.15.8）。剩下六个服务仍整体在 kit：领域规则（账号身份与目录、聊天频道与保留、全局路由、
  活动窗口协调、平台身份、排行榜）和 Mod 混在同一包里，与 core README"核心实现在 core、kit 只装配"的自述不一致。
- **为什么可疑 / 为什么不自己拍板**：这是职责归属（ARCH 类）而不是缺陷；review 的 ARCH-01 只点名三个，并写明"没有穷尽盘点 Kit 全目录"、
  "statslog 一类运行时统计与接入便利逻辑可以保留在 Kit"。六个里哪些算"通用服务领域"（大概率：rank、chat、activity）、哪些算"接入便利 / 平台胶水"
  （可能：platform、account 的目录部分、global 路由）需要 review 定，避免为了对称把不该下沉的也下沉。account 的 `RegistryBound` 钩子
  （U-0217 同批加的 collaborator 绑定）与 `service/directory` 依赖也要先定去向。
- **候选修法**：按 M-07/M-08 + M-11 的模板逐包做——core 得领域文件 + 接口 + `-emit transport`，kit 留 Mod / server run + `alias.go`
  + `-emit assembly -dir github.com/tjbdwanghaibo/roost-core/service/<x> -out .`；每包一个 M 编号；判为"留 kit"的只在 kit README 写明理由。
  各包 Redis key / errcode 段 / RPC 方法名不变；每包各需 core + kit 一次发版（可以攒一批）。
- **复现 / 验收草稿**：`go list -deps ./service/<x> | grep roost-kit` 在 core 为空；kit 全套；codegen 生成工程（framework services 引用的是 kit 别名）编译。
- **来源**：2026-09-16 ARCH-01～04 收尾时的遗留项（`docs/history/POST_RELEASE_PLAN_2026-09-08.md` §0.6）。

## 09-17 第三轮已分流：Wanted-02 / 03 / 04

Wanted-02 → RR-20260917-05（嵌套通知），Wanted-03 → RR-20260917-06（attribute 契约），Wanted-04 → RR-20260917-07（原生 Saga 完成订阅）。全部未修复；[确认问题与实施交接](REVIEW-2026-09-17-03.md) · [完整复现](REPRO-2026-09-17-03.md)。以下仅归档原始候选，不再属于待审表。

### W-2026-09-17-02：dao 嵌套里的嵌套（map / slice / struct 字段的元素）从存储解码后没有 dirty 传播接线

- **位置**：roost-codegen `internal/dao/template_nested.go`——`Set<Field>` 对 Kind 2（map）只 `s.<f>.Set(key, val)`、Kind 3（struct）只 `s.<f> = v`，
  以及 U-0224 新增的 `set<Field>RawMap`，都没有对元素调用 `SetNotify`；对照 `template_dao.go` 的 DAO 层：`setXRawMap` / `SetX` 对每个嵌套值
  `val.SetNotify(func() { d.markXKeyDirty(key, val) })`（:266、:323、:342）。
- **现象**：`hero.GetEquips(1).GetGems(2).SetLevel(3)`——改的是嵌套里的嵌套，`GemInfo.Mark()` 的 notify 为 nil，`EquipInfo` 与 `HeroDao` 都不知道，
  这次变更不进 dirty、不进事务 patch。用 golden 的 `EquipInfo.gems: map[int32]*GemInfo` 就能写出会红的测试。
- **为什么可疑 / 为什么不顺手改**：嵌套 struct 模板从一开始就只把自己的字段变更 `Mark()` 给父级，第二层往下从未接线——是"承诺无实现"（C2）
  还是"嵌套只支持一层"的未写明限制，需要 review 定；接线要决定 notify 闭包捕获什么（`s.Mark` 即可，父链自然递归），以及 slice 元素的处理。
- **候选修法**：嵌套模板对 Kind 3 字段与 Kind 2 / Kind 1 的元素在 Set 与 RawMap 恢复时 `SetNotify(s.Mark)`；DAO 层 `Init` 已对第一层做了同样的事。
- **来源**：U-0224 修 BSON 表示时发现（`docs/bugfix/U-0224-dao-nested-bson.md` 未做一节）。


### W-2026-09-17-03：attribute 生成器的输出依赖"所在包需提供"的七个类型，而 attribute feature 的脚手架不提供它们

- **位置**：roost-codegen `internal/attribute/gen.go`（生成物引用 `AttrID` / `AttrValue` / `AttributeMeta` / `AttributeProfile`，容器访问器还引用
  `Snapshot` / `Container` / `Selector`，都不带包名）；`internal/roost/render.go:138-160`（feature `attribute` 的脚手架只写 `package attribute` 一行的 `doc.go`）；
  `docs/CODEGEN_REFERENCE.zh-CN.md` §11 只说"所在包需提供框架约定的 … 类型"。roost-core / roost-kit 里没有任何包定义这些类型。
- **现象**：`features` 加 `attribute` → 写一个 `//roost:attribute` profile → `make generate` 生成 `gen_*_attribute.go` → 编译失败（`undefined: AttrID` 等）。
  没有一个可以 import 的权威定义，也没有一份写在文档里的接口签名可以照抄；attribute 生成器在全部三仓里零消费者（codegen 自己的测试只看生成文本）。
- **为什么可疑 / 为什么不顺手改**：这是"生成器承诺了一份契约，但契约在哪里没人写"（C4 跨包契约不一致），修法要先定：这些类型是进 roost-core
  （新包 `attribute`，生成物 import 它）、还是由脚手架的 `doc.go` 生成一份默认定义（每工程一份，可改）、还是生成器自己在 `gen_*_attribute.go` 里带上。
  三种选择对 core 的 API 面和生成工程的自由度影响不同，需要 review 定；实施侧本轮做 demo 时因此**没有**接 attribute（原计划 B8）。
- **候选修法**：A. core 新包 `attribute` 放这七个类型 + `AttributeProfile` 接口，生成物 import；B. 脚手架在 `game/gameplay/attribute/doc.go`
  生成默认定义并标"应用拥有"；C. 生成器每个 profile 文件自带私有别名（多 profile 会重复定义，需去重）。我倾向 A（与 dataengine.DirtyHook 同一模式）。
- **会红的测试草稿**：生成工程 `-features …,attribute`，写 `//roost:attribute index=1 max=4 type P struct{HP int64}`，`make generate && go build ./...`——现在红。
- **来源**：2026-09-17 实施 game-demo B8（attribute 演示）时发现。


### W-2026-09-17-04：原生 Nest saga 步骤的完成效果没有消费者

- **位置**：roost-core `saga/nest.go` `NewCompletionEffect`（Topic `saga.result.<sagaID>`，经 Nest 事务的 Data Engine outbox 发到
  `<dataengine.effects.subject_prefix>.saga.result.<id>`，即 `ROOST_EFFECTS` 流的 `roost.effect.saga.result.*`）；
  `saga/assembly.go` `Start` 只订阅 `SubscribeCompletions`（`ROOST_SAGA` 流、`<saga.subject_prefix>.result.>`）与
  `SubscribeNestStarts`（`ROOST_EFFECTS` 流、`<effect_prefix>.saga.start`）。`grep -rn CompletionEffectTopicPrefix` 只有发送方与回执解码。
- **现象**：按 `SubscribeDataEngineStep` 的文档做一个原生步骤（`inbox.Bind(command, reservation)` + `saga.EmitCompletion` 在 Nest 事务里），
  消费者 `waitReplay` 等到 Data Engine 回执后 ack，但协调器永远收不到完成——它订的是另一条流的另一个前缀。saga 停在 waiting，
  按超时重发，重发又被 inbox 判重放回执（不重跑），协调器仍收不到。
- **为何可疑**：`SubscribeDataEngineStep` 的注释说"acknowledges only after the authoritative Data Engine receipt is projected and replayable"，
  隐含"之后完成会到协调器"；`nest_atomic_test.go` 只断言 CommitRecord 里有那条 effect，没有端到端。start 效果有专门的
  `SubscribeNestStarts`，result 效果没有对称的 `SubscribeNestCompletions`。
- **会红的测试草稿**：在 `assembly_test.go` 的 fake JetStream 上 `Assemble` + `Start`，往 `ROOST_EFFECTS` 流投一条
  `roost.effect.saga.result.<id>` 的 completion 效果载荷（`NewCompletionEffect` 产出的 Payload），断言 `engine.Get(id).Status` 离开 waiting；
  当前没有订阅，断言不成立。
- **候选修法**：A. `Assembly.Start` 加第三个持久消费者：`Starts.Stream` 上过滤 `<EffectPrefix>.saga.result.>`，解 `completionEffectPayload`
  后走 `engine.Complete`（与 `SubscribeCompletions` 同一处理，只是解包不同）；B. 让 `EmitCompletion` 的效果 Topic 直接落到 saga 流——
  不可行，outbox 的 subject 前缀是全局的。A 更像 start 那一侧已经做的事。
- **来源**：game-demo 送礼 saga（第十批）选步骤实现方式时发现；demo 因此用了 `SubscribeMongoStep`，并把"Nest 提交与 inbox 回执不原子"写成边界。

## 09-18 已分流：Wanted-05

已完成候选分流：生成 sync=true 与 Core 不兼容 → RR-20260918-01；房间内部 coordinator 缺持久化水位接线 → RR-20260918-02，均 P2 未修复。[确定结论与实施方向](REVIEW-2026-09-18.md)。公开 API 手动组合八场景通过，因此不采纳“没有公开路径”的笼统前提；自动生成和生产端到端尚未完成。Kit Mod 仍属设计选择，应先修契约并提供可执行样例。下面保留原文及 09-17 当时观察，不再属于待审候选。

### W-2026-09-17-05：实体状态同步（room 广播 + entitysync 订阅）没有装配入口，框架里一个使用方都没有
**09-17 第三轮复核：继续观察。** `EntityBase.Sync()` 是公开入口，`RoomManager.Create` / `RoomBroadcaster.RegisterSubject` 可组合，且 broadcaster 已拥有自己的 SubscriptionCoordinator。因此下方候选的“没有任何公开路径”不是本轮结论，也不建议再建第二个 coordinator。仍缺真实生成实体→房间→会话→sink 的运行证据，先补例子及锁/持久化水位/卸载关闭验证，再定 Kit 装配。见 [审查及修正](REVIEW-2026-09-17-03.md) 与 [机制](../review/IMPLEMENTATION-GENERATED-FEATURE-CONTRACTS.md)。以下保留实现侧原始候选。

- **位置**：`roost-core/room`（`NewRoomManager` / `NewRoomBroadcaster` / `NewRoomTransportSink` / `RoomBroadcaster.RegisterSubject`）、
  `roost-core/entitysync`（`NewSubscriptionCoordinator`）、`roost-core/entity/subject_sync.go`（`SubjectSyncState`、`SubjectSyncPacker`）。
  `grep -rn "RoomManager\|RoomBroadcaster\|SubscriptionCoordinator" --include='*.go' roost-kit roost-codegen` 在两个仓里零命中（本轮 core HEAD）；
  `RegisterSubject` 的调用方只有 core 自己的测试。
- **现象**：这条链的两端都在：codegen 的实体生成器支持 `sync=true` + `subjectPacker`（`internal/entity/gen.go` 的 `SubjectPackerFactory`），
  codegen 也会在工程带 `nettransport-*` feature 时生成 `transport.NewRoomSink(async, resolve)`（`internal/roost/render.go`）。
  中间那段没有：没有谁建 `RoomBroadcaster` / `RoomManager`、把实体的 `SubjectSyncState` 注册进去、把订阅接到会话上。
  kit 的 `room.RoomMod` 只发布 `ISyncBus`（跨进程的房间总线），与广播栈无关。
- **为何可疑**：`RoomTransportSink` 的构造被 codegen 生成出来却没有任何东西能喂它（它要 `RoomBroadcaster` 当上游）；
  `RoomManager` 的预算 / 空闲清扫 / 优雅关闭是成套的产品级功能，却没有任何装配路径；实体侧的 `subjectPacker` 标记生成了工厂，
  但没有消费者会调用它。三处各自都有测试，合起来没有一条端到端路径——这正是 U-0224（dao 嵌套 BSON）那一类"每一段都对、连起来没人走过"的形状。
- **会红的测试草稿**：在生成工程里（或 core 的一个 example 里）：建 `RoomManager` → `Create(roomID)` → 对一个 `sync=true` 的实体
  `RegisterSubject(state)` → `Subscribe(sessionRef, subjectID, profile)` → 改实体并 `FlushSubject` → 断言 `RoomTransportSink` 的下游
  收到了该会话的一帧。现在写不出来，因为没有任何公开路径把"生成的实体"接到"房间"上——缺的就是这一段。
- **候选修法**：A. kit 出一个 `statesync` Mod：持 `RoomManager` + `SubscriptionCoordinator`，发布一个"把实体注册进房间 / 订阅 / 退订"的
  capability，codegen 在 `sync=true` 的实体生成注册代码（与 nest 的 syncsender 同形）。B. 先只补一个 core `examples/roomsync`，
  把端到端串起来当活文档，装配层等有真实需求再定。C. 判定这条路是留给具体游戏自己装配的，那就在 `room` / `entitysync` 的包注释里
  写明"这三段谁负责接"，并给 `NewRoomSink` 的生成加一句说明。
- **来源**：game-demo 第十一批做 B10（实时）时选型发现。demo 最终走了 `lockstep`（帧同步）那条路——它的服务端 `Room` 与客户端
  `robot.LockstepBot` 都是完整的，业务只需接线，两小时就跑通了；状态同步这条路相比之下没有入口。
