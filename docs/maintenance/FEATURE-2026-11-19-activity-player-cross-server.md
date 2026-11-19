# 方案：SLG 主堡跨服——Castle 跨服、ActivityPlayer 伴生共享、协议转发

状态：**只做方案，不实施**（2026-11-19，维护者：“以上讨论的是方案，可以不用实施，core 底层有能力解决就可以”）。结论：除第 5 节“尚未确认”的两项外，所需能力 core 都已具备，缺的是业务模块与生成器的胶水（见 3、4.6）；唯一需要动 core 的可选项是“B 直接推给客户端”（Gate 现在不允许，见 4.6、5.6），不做时经 A 转推即可；第 6 节的分批只在以后真要做时参考。

## 1. 场景

SLG 里 A 服玩家的主堡迁到 B 服地图，一段时间后再迁回 A。这**不是**换服（rehome）：玩家的归属、账号绑定、Player 实体都留在 A；只有主堡跨过去。

要求：

- 主堡在 B 期间，B 的玩法（主堡相关的战斗、采集等）产生的个人活动贡献，必须与 Castle 的修改**在同一笔事务里成交**；
- 个人活动始终按**原服 A 的节奏**推进（A 每周三开的活动，主堡在 B 也按周三）；主堡在 B 期间 A 新开的活动照常开给这名玩家；
- 客户端只连 A，主堡相关的协议要能到达 B。

## 2. 维护者决定（2026-11-19）

| # | 决定 |
| --- | --- |
| D1 | 拆成三个实体：**Player**（本地实体，留在 A）、**Castle**（Remote 托管实体，跨服时所有权 A → B，回来 B → A）、**ActivityPlayer**（Player 的伴生 Remote 实体，存个人活动数据） |
| D2 | **推进仍在 Player 上**：A 的 Player 用本地持久定时器、进入与查询时对账推进个人活动，节奏与开服时间都按 A；A 新开的活动由 A 激活，主堡在 B 期间照样拿得到 |
| D3 | ActivityPlayer 平时由 A 独占；**主堡跨到 B 后切成共享**（`EnterRemoteSharedMode`），回 A 后退回独占（`LeaveRemoteSharedMode`），共享期间 A、B 都能写 |
| D4 | B 的贡献与 Castle 的修改用 **multi（一条 Nest 消息同时锁 Castle 与 ActivityPlayer）在同一笔事务里成交，不用 Saga** |
| D5 | **协议转发是新功能**：客户端只连 A，A 把主堡相关的消息转给 Castle 当前的 owner |
| D6 | 不做“换节奏”：已激活的序列冻结 A 的规则，跨服不改变节奏 |

讨论过程中否掉的做法与原因：Player 整体做成 Remote（所有模块、enter、自身视图、换服都假定 Player 是本地实体，且 Player 是最热的实体，共享模式每次写取锁代价太高）；ActivityPlayer 整个转给 B（转走后只有 B 能推进，A 新开的活动拿不到）；推进放在 Remote 实体上（Remote 实体不能挂本地持久定时器，只能靠扫描推进，10 万在线时不准点）。

## 3. 框架已有的能力（逐条核对过）

| 能力 | 接口 / 依据 |
| --- | --- |
| 伴生 Remote 实体 | facebook 模块（每名玩家一个伴生 Facebook 实体）、team 模块；`Ensure` 在事务外装进内存 |
| 独占 → 共享 → 独占 | `ClaimRemoteOwnership`、`EnterRemoteSharedMode`、`LeaveRemoteSharedMode`（推进 `MarkerEpoch`，旧写被 fence）；见 [REMOTE_ENTITY](../framework/REMOTE_ENTITY.md#独占写准入) |
| 所有权转移 | `TransferRemoteOwnership(id, newOwnerSID)` |
| 一条消息写多个 Remote 实体，原子提交 | `IRemoteAtomicBatchCommitter`：同一个 Mongo transaction 里做全部实体的 CAS、DAO、快照与 outbox，不支持时 fail-fast，不退化成逐实体提交；每个实体按自己的归属做写准入（独占不取锁，共享取分布式锁与写许可） |
| 本地 Player 与 Remote 实体同一笔事务 | Remote 写与本地写进同一条 WAL 提交记录（team 模块的用法） |
| 调用 Remote 实体当前的 owner | 路由 RPC：`WithRouteKinds` + `RouteSource`，服务端 `RPCRouteCheck` |
| 非 owner 读 | Mirror 只读快照 |
| 定向发给某个服的 game | `Bus.Send(toSid, module, msg)`、`Bus.CallTo(svcType, sid, …)` |

更正（2026-11-19 核对）：`login.presence` / account 的在线登记是**区服级**的（“本服连这个地址、这么忙”，给登录入口列清单用），不是“玩家连在哪个 game”。本场景不需要它：玩家只连 A，Castle 上记着主人的原服 sid，B 直接按 sid 发给 A。

约束：handler 只能持久写本条消息声明的 Remote 托管实体（`refuseUndeclaredRemoteTargets`），所以 multi 消息必须把 Castle 与 ActivityPlayer 都列为目标。

## 4. 设计

### 4.1 实体、归属与写入方

| 实体 | 平时 | 主堡在 B 期间 | 谁写 |
| --- | --- | --- | --- |
| Player（本地） | A | A | 只有 A |
| Castle（Remote，独占） | A 独占 | B 独占（转移） | 当前 owner |
| ActivityPlayer（Remote，伴生） | A 独占 | **共享** | A：节奏与贡献；B：只写贡献 |

### 4.2 事务

| 发起方 | 事务 | 锁定目标 |
| --- | --- | --- |
| A：Player 的活动定时器到期 / 进入 / 查询对账 | 推进阶段、激活新活动、结算 | Player（本地）+ ActivityPlayer |
| A：玩家在 A 上的操作产生活动事件 | 推进进度 | Player（本地）+ ActivityPlayer |
| B：主堡相关操作 | Castle 修改 + 活动贡献，同成同败 | Castle + ActivityPlayer（multi） |

### 4.3 硬规则：节奏状态只由 A 写

激活、阶段、期限、`NextDue` 等“节奏状态”只能由 A 的 Player 侧推进；B 只能写贡献字段（进度、积分）。原因：到期定时器挂在 A 的 Player 上，B 改了期限或激活了活动，A 的定时器不知道，会错过或提前到点。

落实方式（实施时选一种，倾向第一种）：

- ActivityPlayer 的 DAO 分成“节奏”与“贡献”两部分，B 能调的 handler 只写贡献 DAO（由生成器 / 守卫检查）；
- 或者活动组件对 B 只暴露贡献类 API（不暴露 `Activate` / `Advance` / `Reopen`）。

贡献写入必须先按冻结的期限检查（`CheckOpen`），过期的贡献拒绝。

### 4.4 跨服与回迁流程

主堡跨到 B：

1. A：`Ensure` ActivityPlayer 并 `EnterRemoteSharedMode`（A 是 owner，才能切共享）；
2. A → B：`TransferRemoteOwnership(Castle, B)`；
3. A 记下“主堡在 B”（Player 上的跨服状态），协议转发据此路由。

主堡回 A：

1. B → A：`TransferRemoteOwnership(Castle, A)`；
2. A：`LeaveRemoteSharedMode(ActivityPlayer)`，B 之后的写被 fence；
3. A 清掉跨服状态。

先后顺序保证“Castle 在 B 时 ActivityPlayer 一定是共享的”。两步之间崩溃时，靠 Player 上持久的跨服状态在重启 / 下次访问时续做（每一步都可重做：已共享再切共享、已在目标 owner 再转移都应答成功）。**待定**：续做由谁驱动（A 的 Player 定时器 / 进入对账），以及 B 侧仍在途的贡献在第 2 步之后被 fence 时怎样给客户端回结果。

### 4.5 共享期间的代价

- A、B 每次写 ActivityPlayer 都要取分布式锁与写许可（`_remote_entity_meta` 上一次多数派 `findAndModify`），A 的定时器推进与玩家事件也一样；
- 一边写了，另一边内存里的实例落后，下次写之前先仅内存卸载再从权威重载（`ErrRemoteEntityReloading` 可重试）；
- 只在主堡跨服期间付这个代价；平时 ActivityPlayer 由 A 独占，稳定持有不碰 Mongo。

### 4.6 协议转发（新功能，D5）

客户端只连 A（Gate 一条连接只绑定一个 game，`gate_forward.go` 按 `binding.Game` 转发，没有按消息号分发的能力）。两种做法：

| | **game 层转发（推荐，维护者 2026-11-19 同意先推荐这层）** | Gate 层分发 |
| --- | --- | --- |
| 做法 | 客户端消息照常到 A；A 发现目标 Castle 的 owner 不是自己，经路由 RPC 转给 B，应答带回客户端 | Gate 按消息号 / 目标实体把一条连接的部分消息直接送到 B |
| core 已有 | 路由 RPC 全套（Redis 登记 → 所有权记录查找、改投、no responders 重查、按实体 ID 的接收端顺序键，见 [guide/09 §2A](../framework/guide/09-services.md#2a-路由-rpc-与改投)）；`RPCRouteCheck` / `RPCWriteAnswer`；`Bus.Send` / `CallTo` 定向发 | 没有。Gate 只有一个 `binding.Game`，票据只签一个 game |
| 要新做 | 业务胶水：A 上主堡消息的转发 handler、B 上对应的路由 RPC handler；生成器目前只给 `service=` 的 Remote kind 接 `WithRouteKinds`，game 进程上的 Castle 要手工接（或以后让生成器支持） | Gate 多目标绑定、account 为 B 签票据、Gate 按消息分发表、两个 game 往同一连接推送的顺序、任一 game 断开 / 重连 / 换服时绑定的收拾 |
| 代价 | 主堡消息多一跳（A → B 一次内网 RPC），A 承担转发 | 少一跳；但改的是接入层与登录链，影响所有工程 |
| 顺序 | 同一 Castle 的请求在 B 上按实体 ID 排队（路由 RPC 的 `OrderKey`）；同一玩家发往 Player 与发往 Castle 的消息之间不保证顺序（两个实体本来就不同序） | 同左，另加 Gate 侧两路的顺序问题 |

game 层的完整链路：

1. **请求**：A 收到主堡消息（消息声明目标是 Castle）。Castle owner 是 A：本地派发。不是：`CallRoutedChecked(Castle ID)` 发给 owner；B 的 handler 先 `RPCRouteCheck`，再进 Nest（multi：Castle + ActivityPlayer），错误经 `RPCWriteAnswer` 转成改投 / 可重试；A 把应答原样回客户端。转移进行中答可重试，A 不重试写、由客户端重发（与现有路由 RPC 语义一致）。
2. **B 的变化推给客户端**：
   - Castle 的变化：A 持 Castle 的 Mirror 只读快照，经 Sync 视图推给自己的客户端（Sync 视图可以挂 Mirror 实体）；或者 B 提交后 `Bus.Send(A 的 sid, …)` 定向通知 A 推送。
   - ActivityPlayer 的变化：B 提交后 `Bus.Send(A 的 sid, …)` 定向通知；A 在下次写之前本来就会按共享模式仅内存卸载、从权威重载，收到通知时先重载再经自身视图下发。通知丢了（总线可丢）只是推送晚到，下一次 A 侧写或查询对账时补上，不丢数据。
   - **B 直接推给客户端**（维护者 2026-11-19：不需要 A 服数据的推送，理论上可以从 B 直接推）：**core 现在不支持**，Gate 有意挡住了：
     - 有序出站（`gate_outbound.go` `receiveOutbound`）只收连接绑定的那个 game 发来的包：subject 里是 `binding.Game`，包里的 `Binding` 要与连接当前绑定逐字段相同，`OutSeq` 必须是这条连接的下一个序号。B 不知道 A 的绑定和序号，就算知道也会和 A 抢同一个序号流。
     - 广播（`broadcast.go`）的 `Players` 受众可以点名玩家，但 `enqueueBroadcastLocked` 对 game 角色只投给 `connection.binding.Game == request.Source` 的连接，B 点名 A 的玩家匹配数为 0；只有 `system` 角色不受这条限制（全服系统广播用的 singleton 身份，不适合拿来做业务推送）。
     - 要支持，改动在 Gate：连接多一个“附加推送方”集合（由绑定的 game A 在主堡跨出时授权 B、回迁时撤销，或由 account 签进票据），Gate 对集合里的 game 按广播那样做**无序推送**（不占 `OutSeq`）。比整套 Gate 层分发小得多（请求仍走 A，不动票据的请求路径），但 B 的推送与 A 的推送之间没有顺序，客户端要按消息自带的版本处理，只适合 B 独有、不依赖 A 状态的推送（战报、地图事件等）。
     - 不做时：B 经 `Bus.Send` 交给 A 转推，多一跳，但与 A 的其他推送在同一个有序流里。
3. **回迁**：Castle 的路由登记随 `TransferRemoteOwnership` 换成 A，A 上的旧路由缓存靠改投 / no responders 重查自愈，不需要额外清理。

不推荐 Gate 层：它解决的只是“少一跳”，代价是改接入层、票据与推送顺序，影响所有工程；只有当实测 A 的转发成为瓶颈时再议。

### 4.7 活动模块改造

现在个人活动的数据（`ActivityBook`）与定时器都在 Player 上（`PlayerDao.Activity`、`ActivityDeadline` 定时器）。改为：

- `ActivityBook` 放在 ActivityPlayer 上；定时器与推进留在 Player 上；推进 handler 同时锁 Player 与 ActivityPlayer；
- 对 B 暴露只写贡献的入口（见 4.3）；
- 不装跨服的工程：ActivityPlayer 一直由 A 独占，行为与现在相同，只多一次 Remote 写的开销（稳定持有不碰 Mongo）。是否让“不跨服的工程”继续把数据放在 Player 上（两种布局并存），实施前再定。

## 5. 待验证 / 待定

1. **一笔事务里一个独占、一个共享的 Remote 实体**：原子批次本身已支持，但没找到“独占 + 共享混在一个批次”的专门用例；实施第 4 批时先补这个用例（含 B 写时 A 正在写的竞争）。
2. 跨服流程两步之间的崩溃续做与在途贡献被 fence 后的应答（4.4）。
3. ~~B 写后通知 A 刷新与推送的通道~~：定为 `Bus.Send` 定向通知 + A 重载（4.6），通知可丢、靠下次对账补。
4. Castle 到 B 地图之后与 B 的地形、建筑、视野的关系（地形按地图归属分库，D11），本方案不涉及，另行设计。
5. 是否保留“数据在 Player 上”的旧布局给不跨服的工程（4.7）。
6. **B 直接推给 A 的客户端**：core 不支持（Gate 只收绑定 game 的推送），要做需在 Gate 加“附加推送方”与无序推送（4.6）；不做就经 A 转推。是否要做待维护者定。

## 6. 分批（不排期，真要做时参考）

1. **活动模块拆宿主**：`ActivityBook` 可放在伴生 Remote 实体上，定时器留在 Player；节奏 / 贡献分开（4.3）；现有个人活动用例全部改到新布局并通过。
2. **ActivityPlayer 伴生实体与共享切换**：`Ensure`、跨服 / 回迁两步流程与续做（4.4）。
3. **协议转发**：消息声明目标实体种类、game 层按 owner 转发与应答回传、定向推送（4.6）。
4. **B 侧 multi 事务**：Castle + ActivityPlayer 同成同败；补“独占 + 共享同批次”与并发写用例。
5. **真实依赖演练**：两个 game（A、B）+ account + Remote，主堡跨过去、A 新开活动、B 贡献、回迁；每个阶段 SIGKILL 后核对活动数据、Castle 与贡献不丢不重。

## 7. 验收

- 主堡在 B 期间，A 每周三开的活动照常开给这名玩家，按 A 的节奏结算；
- B 的一次主堡操作失败时，Castle 与活动贡献都不落地；成功时两者同时可见；
- 回迁之后，B 的任何迟到写入被 fence，不改动 ActivityPlayer；
- 平时（不跨服）个人活动的推进不访问 Mongo 确认归属（稳定持有）；
- 跨服期间每次写的额外代价（锁与写许可）有测量记录。
