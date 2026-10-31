# 房间 / 组同步的正式入口：与 self_sync 共用 Manager 与连接（方案与实施记录）

来源：派对房间纵切（`8f3dd69b`，[PARTY-ROOM-INTEGRATION §8](../framework/PARTY-ROOM-INTEGRATION.md#8-框架没有什么)）发现“房间内同步”没有装配入口，夹具自建 `entitysync.Manager` 做 Nest 的 `EntitySync` 观察者；与 `access.player.self_sync` 同用时会怎样登记为“尚未确认”。维护者 2026-10-31“继续框架改进”。本页是方案与取舍；现行契约在 [ENTITY_SYNC“按组同步与 self_sync 共用连接”](../framework/ENTITY_SYNC.md#按组同步与-self_sync-共用连接groups)。状态：已实施（未发版）。

## 1. 先确认：同一进程同时开 self_sync 与房间组会怎样（2026-10-31 源码，v1.33.2）

| 接法 | 结果 | 证据 |
| --- | --- | --- |
| 业务自建 Manager，经 `NestOptionWithEntitySync` 交给 `wiring/nest.NewModWithEntitySync` 的 Nest | **静默覆盖**：Mod 把自己的 Manager 追加在业务选项之后，Nest 只有一个观察者位置；业务 Manager 不被绑成提交生产者，收不到捕获与回滚撤回，periodic 下无报错（on_change 下 Start 才报错） | 红测试 `wiring/nest` `TestEntitySyncModRefusesASecondEntitySyncObserver`：修前 `Provide with a second EntitySync observer = <nil> (business observer bound: false)` |
| `policy.Group` 挂到 self_sync 的 Manager（`Mod.EntitySync()`） | Manager 与线格式层面**能共存**：同一会话、同一帧两个对象，按命名空间 + 视图键分流。但会话归 `SelfView`：登录后第一个政策阶段之前 `Join` 被拒（`entitysync: session is not open`）；断线重连后组订阅随旧会话删除、`Group` 仍记着成员，**房间静默断供** | 探针（真实 TCP，未入库）：重连后第一帧 `map[player/self:gold=10]`、`group members=1`，下一周期无推送 |

结论：两者都是缺陷。第一条已修（`Provide` 返回 `ErrEntitySyncObserverConflict`）；第二条不是 `Group` 的错（它的契约就是“会话由调用方开关、Manager 丢会话后调用方再 Join”），而是缺一个“会话归自身视图”的组政策——本方案。

## 2. 方案

- **`policy.GroupView`**（框架，`framework/sync/entitysync/policy/group_view.go`）：与 `SelfView` 同一写法、装在同一个 Manager 上、政策阶段排在 `SelfView` 之后。成员关系是事实（`Join / Leave / Disband / EntityLoaded` 只记事实、不阻塞）；政策阶段登记组实体、给会话已开的成员订阅、没开的留在 waiting 每阶段复查；`Released` / `Resubmit` 回调置回未订阅，会话重开的那个阶段全量重发。`GroupView` 拥有组实体的登记，`Disband` 退订并注销。
- **`wiring/selfsync.Groups`**（装配）：`selfsync.Config.Groups`；Nest Mod Init 时随自身视图装上；组实体加载按 kind 转交；`Join / Leave / Disband` 在 Nest 事务里推迟到提交之后（`nest.AfterCommit`），回滚即不发生。组 ID 是组实体 ID，成员是玩家 ID。
- **生成器**：`access.player.sync_groups: [party_room]`（需要 self_sync）。`project sync` 在实体包里创建一次业务文件 `sync_group.go`（`SyncGroup` 变量，视图键由业务改），bootstrap 把它们交给 `PlayerSelfSync`。不生成成员规则。
- **线上区分两路**：同一连接、同一推送编号（`msgid.MsgSelfSync`）、同一会话与 epoch，按每个更新的 `Namespace` + `Profile.Key` 分流；客户端只要一个 `frame.Receiver`。

## 3. 取舍

| 选项 | 结论 | 原因 |
| --- | --- | --- |
| A. wiring 层组同步（`selfsync.Groups`）+ 清单声明（`sync_groups`） | **采用** | 与 self_sync 共用 Manager、连接与会话；装配可见（bootstrap 一行）；声明归项目、视图键由业务定 |
| B. 实体标记 `syncGroup=` | 不采用 | bootstrap 由清单渲染（`project sync`），从标记反推要在渲染时扫源码；声明变量放在实体包里才能被 handler 引用（handler 不能 import bootstrap），清单 + 一次性业务文件更直接 |
| 给 `Group` 加会话钩子 | 不做 | 自管会话的调用方（自建 Manager、机器人、测试）仍需要现在的 `Group`；两种会话所有权分成两个类型更清楚 |
| 允许第二个 Manager | 不做 | 观察者位置只有一个；两个 Manager 推同一连接要两套推送编号与 epoch |
| 成员关系持久化 | 不做 | 进程内事实；重启后业务在玩家重连的 handler 里再 `Join`（与写连接代同处） |

## 4. 验证（2026-10-31）

- `policy` `TestGroupView*`（6 条，含“禁用 Released 回调时重连断供”的变异检查）、`wiring/selfsync` `TestGroups*`（真实 TCP 两路、Nest 提交边界、装配拒绝）、`wiring/nest` 冲突红绿、codegen `TestSyncGroups*`。
- `scripts/test-party-room-generated.sh` 13 场景（新增 `TestPartyGameSelfViewAndRoomShareOneConnection`）。
- 未验证：真实依赖模式（`ROOST_PARTY_ROOM_REAL=1`，本轮未跑）；独立 Gate 下的组同步（只测了 `OnActive / OnClosed` 的嵌入替身与嵌入 TCP）；慢消费者与容量（离线成员每阶段复查的成本、多房间规模）。
