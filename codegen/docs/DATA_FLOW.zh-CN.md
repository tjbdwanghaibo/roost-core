# 生成工程里的数据流：六条路径、各自保证什么

其他文档按功能组织（怎么加一个 handler、怎么配一张表）。这一篇按**路径**组织：一次客户端请求
进来之后，字节往哪走、每一层守的是什么不变量、失败在哪一层被处理、重放时靠什么幂等。
读代码之前先读这一篇，能省掉"这个调用为什么不能放在锁里"这类反复踩的坑。

例子全部取自 `roost project new … -template game-demo` 生成的工程（demo 的说明见生成工程里的
`demo/README.md` 与 roost-core 的 `docs/feature/GAME_DEMO_TEMPLATE.md`），路径都是生成后的真实路径。

## 0. 六条路径速查

| # | 路径 | 用在什么上 | 谁保证原子性 | 幂等键 | 失败后谁重试 |
| --- | --- | --- | --- | --- | --- |
| A | 请求 → Nest 事务 → Data Engine | 改自己进程的实体（加道具、加经验） | Nest 一次 handler 一条 WAL 记录 | 无（调用方自带，如帧序号） | 客户端 |
| B | 事务 → effect outbox → 消费者 | 一次提交要引发的后续动作（升级 → 奖励邮件） | outbox 与实体变更同一条记录 | EffectID（消费者收据 + 下游 RequestID） | JetStream 重投 |
| C | 端点 → bus RPC → 托管服务 | 调另一个进程的服务（match / chat / mail / session） | 没有跨进程事务，只有各自的 | 调用方给的 RequestID | 调用方（或不重试） |
| D | Nest 事务 → saga → 步骤 / 补偿 | 跨两个事务域的一次业务操作（送礼：背包 + 邮件） | 每步自己原子，整体靠补偿 | saga id + 每步的 CommandID | saga 协调器 |
| E | lockstep 房间 → 帧广播 | 实时战斗（确定性模拟） | 不落库，没有事务 | 帧号 + 座位（重放窗口） | 包内冗余 / 补发历史 |
| F | 配置数据 → 请求快照 | 读表（道具、掉落） | 快照不可变 | 不适用 | 不适用 |

## 1. 路径 A：请求 → Nest 事务 → Data Engine

```
玩家 TCP 帧
  → internal/access/player/tcp        解帧、鉴权、按 msg id 分发（生成）
  → game/controllers/player/*.go      端点：错误边界（业务代码）
  → game/handler/syncsender/*_gen.go  Sender：类型化的 Nest 客户端（从 handler 签名生成）
  → Nest 锁住实体，按参数顺序          （运行时）
  → game/handler/*.go                 handler：唯一能改实体的地方（业务代码）
  → 组件方法 → 生成的 DAO mutator      dirty 追踪 / undo 日志 / 持久化 patch
  → Data Engine：WAL → 投影 → Mongo
```

**每一层的不变量**

- **端点不持有锁**。它只做两件事：把协议消息翻成 Sender 的参数，把错误翻成响应里的 `Code` / `Reason`。
  端点返回 `error` 会被生成的 TCP server 当成坏帧——**断连**。所以业务失败一律
  `errcode.ClientError(err)` 换成码，只有"框架真的坏了"才返回 error。
- **handler 里不能做任何会阻塞的外部调用**：它在实体锁内跑。bus 调用、HTTP、Mongo 直查都不行——
  它们要么放在端点里（进锁之前 / 出锁之后），要么放在 effect 消费者里（路径 B）。
- **只走生成的 mutator**（`dao.SetItems(...)`）。直接写字段的话，dirty 追踪、undo 和 patch 全部看不见，
  表现是"内存改了、库里没有"，而且 `rollback=undo` 回滚不掉。
- **`rollback=undo durability=strict`**：handler 返回 error，本次调用过的所有 mutator 回滚；
  提交成功则这条 WAL 记录里的实体变更、收据、effect 一起生效。

**跨实体**：一个 handler 可以有多个实体参数（demo 的 `AddExp` 是 Player + World），Nest 按锁档顺序
一次锁齐，两边的变更进同一条记录。跨进程的实体不能这样——那是路径 C 或 D。

## 2. 路径 B：事务 → effect outbox → 消费者

一次提交要引发的后续动作（发邮件、通知别的服务）不能在 handler 里直接做：handler 在锁内，而且
"提交成功"和"动作做了"必须同生共死。做法是把动作写成一条 **effect**，它与实体变更进同一条 WAL 记录，
Data Engine 的 outbox 提交后再投递。

```
handler 里 nest.Emit(effect)            意图，和实体变更同一条记录
  → Data Engine outbox（Mongo 集合）     提交后可见
  → JetStream（ROOST_EFFECTS 流）         投递，至少一次
  → 消费者：internal/service/<x>/*.go     Mongo 收据去重 → 做事
```

**两层幂等**（demo 的 `level_up_mail.go` 就是范例）：

1. 消费者在自己的 Mongo inbox 里按 EffectID 提交一条收据，重投的 effect 认得出来、跳过；
2. 下游服务自己按 `RequestID` 去重，而 `RequestID` 就是 EffectID——所以"收据提交了但进程在 ack 前死了"
   这一格，重投时下游返回的是它已经产出的那封邮件，不是第二封。

**为什么需要两层**：消费者的 Mongo 事务能保证"收据 + 同一个 Mongo 里的写"原子；而发邮件是 bus 调用，
不在那个事务里。凡是"事务里做了不可回滚的外部动作"，都要问第二层幂等在哪。

**积压是会熔断的**：outbox 有硬上限（`dataengine.outbox.max_oldest_age`，默认 30m）。没有消费者 ack 的
effect 堆到期限，进程 fail-stop——这是对的，积压不该静默增长。本机换工程重跑最容易撞上（同一个
`game` 库、不同的 nats 前缀），处置见 demo README 的"本地实跑"。

## 3. 路径 C：端点 → bus RPC → 托管服务

`services.<name>.uses` 声明的托管服务（account / chat / mail / match / session）在**别的进程**里。
生成的客户端是类型化的 servicerpc：`internal/service/<x>/framework_clients_gen.go` 给出访问器，
调用经 NATS 请求-响应。

```
端点（无锁）
  → svcmatch.Matchmaker.Enqueue(ctx, queue, subject, requestID)
  → NATS 请求 roost.rpc.match.…
  → match 服务进程：自己的存储与事务
  ← 响应（或 coded error）
```

- **没有跨进程事务**。调用成功 = 对面的事务成功；调用超时 = **不知道**对面成不成功。所以每个写调用
  都要带幂等键（demo 用"玩家 + 会话 + 帧序号"），重试才安全。
- **对面的 coded error 原样穿过**：`errcode.ClientError` 把它翻成响应里的码，不会坍缩成 internal。
- **绝不在实体锁里调**。demo 的 matchmaker 注释写得很直白：这类调用从普通 goroutine 发起，World 的
  计数是之后用它自己的 Nest handler 补的。

## 4. 路径 D：Nest 事务 → saga → 步骤 / 补偿

一次业务操作横跨两个事务域（送礼：扣发送方背包 = Nest 事务；给收件人发邮件 = mail 服务）时，
没有能覆盖两边的事务，只有"每步各自原子 + 失败补偿"。

```
handler 里 saga.EmitStart(...)          start 意图，与本次事务同一条 WAL 记录
  → outbox → ROOST_EFFECTS 的 saga.start
  → saga 协调器（saga Mod，进程里）       写 saga 记录，开始发步骤命令
  → ROOST_SAGA 流：<prefix>.command.<step>
  → 步骤消费者（SubscribeMongoStep）      Mongo inbox 占命令 id → 跑业务 → 发完成
  → <prefix>.result.<sagaID>              协调器收完成，推进 / 补偿
```

- **开始与业务同生共死**：start 是 effect，不是 RPC。所以"检查通过了但 saga 没开"不可能发生。
- **步骤的幂等是命令 id**，inbox 先占位再跑业务；业务里的外部动作仍要第二层幂等（demo 用命令的
  `IdempotencyKey` 当邮件 RequestID）。
- **拒绝要分类**：业务拒绝（对方没进过游戏、邮箱满）是 `Success:false, Retryable:false`，协调器随即
  补偿已完成的步骤；基础设施错误返回 error，让投递退避重试。这两者混淆的代价是"该补偿的没补偿"
  或者"该重试的被当成业务失败"。
- **补偿也会失败**：那时 saga 进 `manual_required`，等人处理（demo 的 `gm.saga.list` 就是给这个用的）。
- **Nest 提交与 inbox 收据不原子**：步骤里的 Nest 事务提交后、inbox 事务提交前进程死掉，重投会再跑一次。
  saga 包的原生路径（`SubscribeDataEngineStep` + `inbox.Bind` 进 Nest 事务）把收据和变更一起提交，
  能关掉这个窟窿。

## 5. 路径 E：lockstep 房间 → 帧广播

确定性模拟：服务器不发状态，只把所有人的输入排成编号的帧广播出去。

```
客户端输入 → BattleInput 请求 → 端点 → 房间的串行 goroutine
  → lockstep.Room.SubmitInput（提交窗口内）
  → Tick 切帧 → 编码（带最近几帧冗余）→ 广播给每个座位
客户端 ← BattleFrame 推送 → 组装器按序释放 → 应用到本地模拟 → 下一帧输入 + 关键帧哈希
```

- **不落库、没有事务**。帧不是状态，是"所有人将要重放的输入"。
- **房间是单所有者状态**：`lockstep.Room` 内部没有锁，所有调用必须在一条 goroutine 上；端点把命令
  投进 channel。
- **丢包靠冗余自愈**：每个广播包带最近 N 帧，丢一个由后续包补上，不重传、不加延迟；缺口太大才请求
  补发历史（走可靠通道分页）。
- **desync 是判出来的**：客户端在关键帧报模拟哈希，房间按法定人数判定，少数派就是 outlier。
- **房间要留收尾窗口**：客户端只有应用了关键帧才报得出那一帧的哈希，所以最后一个关键帧的报告必然
  晚于模拟结束——切完最后一帧就关的房间会正好拒掉裁决需要的那批报告。

## 6. 路径 F：配置数据 → 请求快照

`//roost:table`（或 cfggen 的 meta）→ 生成 loader 与类型化访问器 → `roost-core/configdata` 在运行时
持有一份不可变快照，**一次请求钉一份**。所以热更新落在请求中间也不会让同一次处理读到两份配置。
坏数据死在 `make generate`（`required` / `unique` 在转换时校验），不会死在玩家请求里。

## 7. 一次请求会经过的"边界"清单

按顺序，每个边界都有一个"过了就不能回头"的动作：

| 边界 | 过了之后 | 失败怎么表现 |
| --- | --- | --- |
| 解帧 / 鉴权 | 会话上的 principal 确定 | 断连 |
| 端点入口 | 参数已是类型化的请求 | 响应里的 coded error |
| Nest 锁 | 实体归你，别人等着 | handler 返回 error → 全部回滚 |
| 事务提交 | WAL 记录落盘，effect 已入 outbox | 提交失败 = 什么都没发生 |
| outbox 投递 | 下游会收到，至少一次 | 消费者去重 / 积压熔断 |
| bus 调用 | 对面可能已经做了 | 超时 = 不确定，靠幂等键重试 |
| saga 步骤完成 | 协调器会推进或补偿 | 拒绝 → 补偿；补偿再拒 → manual_required |

## 8. 排错从哪看

- **请求没反应**：先看是不是端点返回了 error（断连）而不是 coded error——生成的 server 日志里有。
- **改了内存、库里没有**：多半是绕过了生成的 mutator，或者嵌套结构没重新生成。
- **effect 不动**：看消费者的 durable 名、Mongo inbox 集合，以及 outbox 是否在积压（`/metrics`）。
- **跨服务调用超时**：看对面进程的 `/readyz` 与 bus 的 subject 前缀（`nats.prefix` 每个环境要独立）。
- **saga 卡住**：`gm.saga.get` 看 `status` / `last_error` / `attempt`；协调器日志里 `kind=completion` 的错误
  说明完成消息被拒。
- **战斗卡住**：看房间是否还活着（进程内状态，重启即结束），以及客户端是不是在会话读循环里做了阻塞调用。
