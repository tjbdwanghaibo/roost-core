# Wanted：实现侧提交的待审查候选

这里放**实现 / bugfix 一侧在干活时看到、但自己不该拍板的疑点**。它们不是 RR：没有编号、没有状态、不进覆盖矩阵。
review agent 每轮看一眼，对每条做三选一——登记为 RR（分配编号、写 REVIEW-*.md、移出本表）、判为"不是问题"（写一行结论后移出）、
或"再观察"（留着，写明还缺什么证据）。实现侧**不要**据此直接改码；这张表存在的意义就是把"发现"和"定契约"分开。

格式：一条一个二级标题，写清位置（仓 / 文件 / 行 / SHA）、现象、为什么觉得可疑、能怎么复现、候选修法（可选）、来源。



## W-2026-09-22-04 分流结论：→ ARCH-10（原 ARCH-08 A 项，09-22 并入；M-13 已实施：envelope sink 已删除）

- **位置**：roost-core `room/room_broadcast.go:85-93`（`roomSubscriberKey{roomID, subscriber}`、`roomFrameGroupKey{roomID, subscriber}`）、
  `:98-110`（`RoomEnvelopeSink{subjectRooms map[subject]roomID, roomFrames map[roomID]uint64, sessionSequences map[roomSubscriberKey]uint64}`）、
  `:449`（`NewRoomBroadcaster` 内 `NewRoomEnvelopeSink(downstream)`——全仓唯一构造点，每房一个）。基线 `5b95547`。
- **现象**：`RoomEnvelopeSink` 每个 `RoomBroadcaster` 私有一个，但它的全部表按 roomID 分片，分组键、序号键都带 roomID；
  在一个只服务一个房间的对象里这些 roomID 恒等于自身 id。对照 `room_transport_sink.go:137` 的 `roomSessionKey{roomID, session}`：
  那一层是进程级共享（`RoomManagerConfig.Downstream` 一个实例给所有房间），一个会话可同时在多个房间，ObjectRef 空间与序号必须按 (room, session) 分——**那里的 roomID 是必要的**。
- **为何可疑**：形状像"进程一个、多房共用"的 fan-in，实际用法是每房一个；同一个事实（哪个 subject 属于哪个房、房间帧号）在 broadcaster 与 envelope sink 各记一份。
  RR-20260915-04 / -05 / RR-20260922-01 都长在这种"三张表记一个事实"的缝里。
- **候选修法**：(a) envelope sink 收成每房私有的形状——`subscriber → 序号` 一张表 + 一个帧计数器，roomID 作为构造参数带下去；
  (b) 反过来把它做成 manager 级共享（一个 downstream 前面一个 envelope sink），让 roomID 键名副其实。二选一。
- **Review 结论（2026-09-22）**：不是缺陷——没有行为错误、没有触发路径；是结构冗余。不登 RR，转 ARCH-08 A 项，实施时占 M 编号。
- **来源**：维护者 09-22 复审 room sync 结构时提出（U-0277 / U-0278 之后）。

## W-2026-09-22-05 分流结论：→ ARCH-10（原 ARCH-08 B 项，09-22 并入；M-13 已实施：coordinator 已删除，订阅表在 subject 里）

- **位置**：roost-core `entitysync/subscription.go`（`SubscriptionCoordinator`，725 行）；消费者只有 `room/room_broadcast.go` 与
  `room/room_transport_sink.go`（`grep -rl roost-core/entitysync --include='*.go' | grep -v _test` 全仓仅此两处）。
  重复记账：`room_broadcast.go:698-713`（`r.subscribers` 计数给 budget）对 coordinator 的订阅表；`:1181/:1216` 与 coordinator `sortSubscriptions` 各排一次；
  `unregisterSubject` 跨层问 `coordinator.Subscribers`。基线 `5b95547`。
- **现象**：coordinator 承担订阅状态机（Pending/Active/Closing + Revision）、每 profile 一次 Prepare 的共享 payload、订阅快照与 ContentVersion、跨 subject 原子批、
  持久化水位门槛、Leave 信封——逻辑是实的。但它是一个只有一个消费者的独立包，room 在外面又记一层订阅者账，三层（coordinator / room / sink）各有一套"投递失败后状态怎么办"。
- **为何可疑**：RR-20260922-01 的根因正是这三套政策合起来才成立（coordinator 恢复 Active、room 每 tick 重试、sink/lane 前缀送达）。
  一个只有单消费者的边界没有换来复用，只换来了要翻译的错误面（`ErrLeaveNotDelivered` 这类）。
- **候选修法**：(a) 把 coordinator 并回 room 包作为内部类型，订阅表只留一份；(b) 给它第二个消费者——不带 room 概念的"实体直接对会话复制"调度器（见 W-06）。
- **Review 结论（2026-09-22）**：不是缺陷。不登 RR，转 ARCH-08 B 项；(a)/(b) 的选择与 W-06 绑定，要维护者拍板。
- **来源**：同 W-04。

## W-2026-09-22-06 分流结论：→ ARCH-10（原 ARCH-09，09-22 并入；M-13 已实施：Manager 不带 room 概念，任何政策都能订；`syncTopic` 文档待改）

- **位置**：roost-core `entity/subject_sync.go:132-160`（`EntitySyncCreateParam{Topic}` → `entity_base.go:160` 写成 `Namespace`）、
  `entity/subject_sync.go:106-108,319`（`SubjectSyncDirtyNotifier`，只有 `room.RegisterSubject` 安装）；
  codegen `internal/entity/gen.go` `syncTopic` 标记（RR-20260918-07 修过其解析）。`SubjectSyncState` 的消费者：`grep -rl` 全仓只有 entity / entitysync / room。
  `syncbus/`、`mirror/` 走 ISyncBus 做跨进程副本同步，不碰 `SubjectSyncState`。基线 `5b95547`。
- **现象**：`sync=true` 的实体只得到一个"内容侧状态机 + packer"；调度、订阅、传输全在 room。没注册进任何房间的 `sync=true` 实体：MarkDirty 只是或位、代际加一，
  永远没人 flush（有界，不泄漏，但什么都不发）。`syncTopic` 变成 `SubjectSyncUpdate.Namespace` 随帧下发，没有任何路由按它分发——目前只是一个名字。
- **为何可疑**：标记承诺了"按 topic 同步"，实现只在 room 里生效且不看 topic；"非 room 模式"在文档与标记上存在、在代码上不存在。
  定级低是因为没有触发路径（demo 全部实体都进房间），不是因为后果轻：一个只写 `sync=true` 不进房的项目会静默零复制、零告警。
- **候选修法**：(a) 补一个不带 room 概念的调度器（实体 → 若干会话，不经 AOI），复用 coordinator——这就是 W-05 的第二个消费者；
  (b) 明确"复制只经 room"，`syncTopic` 从标记里删掉或改成只作 Namespace 的显式命名；(c) 至少在 `EnableSync` 之后若干 tick 仍无 notifier 时打一条 Warn。
- **Review 结论（2026-09-22）**：不是缺陷（无触发路径）。不登 RR，转 ARCH-09；(a)/(b) 要维护者拍板，(c) 可作为独立小单元先做。
- **来源**：同 W-04。

## W-2026-09-22-03 已处理：→ U-0278（维护者 09-22 拍板直接修，无 RR）

生成工程 `sceneLane.AdmitBatch` 逐个推、遇错整批放弃；`pushPlayer` 无会话不计指标。按失败种类分流后关闭，记录见 [bugfix/U-0278](../bugfix/U-0278-scene-lane-per-session-push.md)。

## W-2026-09-22-02：v1.16.1 的进程正常停止后重启，WAL 回放 `mongo: duplicate key`，进程再也起不来

- **位置**：`roost-core/dataengine/engine`（startup projection recovery）与 `dataengine/mongo` 的 remote projection；
  错误链 `startup projection recovery: dataengine projector: segment first_transaction=80ceb6c8c60c05af00000000000000ba
  records=1: transaction 80ceb6c8c60c05af00000000000000ba: dataengine mongo: remote projection: mongo: duplicate key`。
  基线 `00277bd`，生成工程钉 core v1.16.1。
- **现象**：09-22 两进程实跑的 sid 1000（第二个化身，10:44:42 起、10:51 由 `run.sh stop` **正常停止**，
  日志末尾 `mod stopped / server stopped`）在 11:17 重启时于 `mod dataengine start` 退出，每次重启都一样。
  WAL 目录已原样保存：`<scratch>/evidence/wal-dataengine-1000/`（`segment-…0001.wal` 631 KB，最后写入 10:50；
  `ack-0.chk` / `ack-1.chk` 最后写入 **10:44**——也就是第二个化身整整七分钟没有推进过 ack）。
- **为何可疑**：
  1. `game._dataengine_transactions` 与 `game._dataengine_receipts` 里**都没有** `80ceb6c8…ba`——这笔事务没有被记为已提交，
     回放它却撞了 `_id`：它要插入的文档已经被别的事务写进去了。回放对这种记录不是幂等的。
  2. RR-20260920-01 / U-0261 的结论是"新记录已不可能成为毒丸，只有旧记录没有隔离流程"（CARRYOVER A1）。
     这条记录是 v1.16.1 自己写出来的，并且是在**正常停止**之后。A1 的前提不成立了。
  3. `80ceb6c8c60c05af` 与该库里其他事务的前缀 `77750f86f3b3ab39` 不同——是第二个化身的前缀；`records=1` 说明
     它是这个化身在这一段里的**第一条**记录。第一条记录 + ack 七分钟不动，指向"化身切换时 WAL / ack / receipt 三者的
     起点没有对齐"，而不是某一笔业务写坏了。
- **会红的测试草稿**：起一个 dataengine 运行时 → 写若干事务 → 正常 Shutdown → 用同一 WAL 目录、不同化身前缀再起 →
  写一笔会插入新文档的事务 → 再正常 Shutdown → 第三次启动必须成功。若能复现，再把"ack 不推进"单独拆出来。
- **候选修法**：先定契约——回放一条没有 receipt 的插入事务撞 `_id` 时，是"文档已在、视为已应用"（按 digest 比对后跳过）
  还是"毒丸、隔离并继续"。两者都比"进程起不来"好。
- **来源**：09-22 RR-20260922-01 的判别实验准备阶段。这条**没有在本轮修**：它不是登记的三条 RR 之一，
  且需要先定契约。P1 分量（进程死、无出路）。

## W-2026-09-22-01：`redis/driver` 的 toxiproxy 用例 `TestToxicRedisDroppedAcquireReplyIsReconciledNotRetried` 在真实矩阵里 3/4 红

- **位置**：`roost-core/redis/driver/lock_toxic_integration_test.go:151` 与 `:161`；被测 `distLock.Acquire` / `Release`
  （`redis/driver/lock*.go`）。基线 `00277bd`。
- **现象**：U-0276 把故障矩阵脚本修好之后第一次完整跑（toxiproxy 在位，所以这条 Toxic 用例第一次真的跑了，
  而不是像今早手动跑 `./redis/driver` 那样因为没有 `ROOST_DATAENGINE_IT_REDIS_PROXIED_ADDR` 而 skip）：

  ```text
  --- FAIL: TestToxicRedisDroppedAcquireReplyIsReconciledNotRetried (0.11s)
      lock_toxic_integration_test.go:151: Acquire with a swallowed reply returned ok=false err=<nil>
  ```

  单独重跑三次：1 绿、2 红，红的是另一行：

  ```text
      lock_toxic_integration_test.go:161: key still held after reconciliation: exists=1 err=<nil>
  ```

- **为何可疑**：两种红很可能是**同一个缺陷加一把没清掉的钥匙**。`:161` 说的是：SETNX 的回复被 toxic 吞掉后
  `Acquire` 报错，随后 `/reset` 恢复网络，`Release` 应当按 token 把**可能已经写进 Redis 的**那把锁删掉——
  可 `exists=1`，钥匙还在。这把钥匙（TTL 5s）留到下一次运行，下一次的第一步 SETNX 撞上它，
  `Acquire` 返回 `ok=false err=nil`，就是 `:151` 那一行。所以主嫌疑是**收到不确定结果之后 `Release` 没有走
  "按 token 删"的和解路径**（也许 state 没被置成 uncertain，`Release` 直接回了 `ErrLockNotHeld`）。
  这正是这条用例存在的理由（"A lost SETNX reply … reconciliation through Release must free it"），
  而且 `Release` 那条镜像用例（`TestToxicRedisDroppedReleaseReply…`）是绿的。
- **会红的测试**：就是它自己，`source /tmp/roost-dataengine-it/env.sh && go test -tags=integration ./redis/driver
  -run TestToxicRedisDroppedAcquireReplyIsReconciledNotRetried -count=3`。用例本身还有一个次要问题：
  固定键名 `toxic:acquire` 让相邻两次运行互相污染，把一个缺陷显成两种红——加 `t.Cleanup` 里 `DEL` 或按运行随机键名。
- **候选修法**：`Acquire` 在 ctx 超时 / 回复丢失时把 `state` 置为 uncertain 并保留 token；`Release` 在 uncertain 态下
  执行值守护删除（Lua：`if GET==token then DEL`）。先确认现状是哪一步没做到再动手。
- **来源**：U-0276（RR-20260922-02）修复后的验收实跑。这条不是 RR-02 的一部分——RR-02 修的是"矩阵没跑"，
  这一条是"矩阵跑了之后发现的"。

## W-2026-09-20-06 已分流：→ RR-20260921-02

- **位置**：`.github/workflows/demo-publish.yml` 的 "Push the generated tree to demo-generated" 步骤
  （合仓前在 roost-codegen，同一份）。
- **现象**：`gh run list --workflow demo-publish --limit 30` 里 **0 次成功**，一直是：

  ```text
  ! [remote rejected] demo-generated -> demo-generated
    (refusing to allow a GitHub App to create or update workflow
     `.github/workflows/ci.yml` without `workflows` permission)
  ```

  生成工程自带一份 `.github/workflows/`，而 Actions 的 `GITHUB_TOKEN` 不被允许创建或修改
  workflow 文件。
- **为什么可疑**：这条流水线的用途是"把生成出来的 demo 推到一个可克隆可阅读的分支"，
  而它从来没有推成功过——也就是说那个分支上的内容（如果存在）是旧的或根本不存在，
  却没有任何人收到过信号。三仓合一仓 P5 把它前面的部分修好了（生成、编译、测试、
  `generate --check` 现在都通过），暴露出后面这一段一直是坏的。
- **候选修法**：(a) 发布时剔除 `.github/workflows/`，并在 GENERATED.md 里说明"CI 请看模板仓"；
  (b) 换成带 `workflow` scope 的 PAT；(c) 推到另一个仓库而不是本仓分支。
  选哪个取决于"发布出去的 demo 要不要带它自己的 CI"——这是产品决定，不是搬迁决定。
- **来源**：三仓合一仓 P5。触发已改回只手动，免得每次推 main 都红一条。

## W-2026-09-20-05 已分流：→ RR-20260921-01（根因更准：不是「窗口会滚」，而是贡献的窗口 id 没有回传通路）

- **位置**：生成工程 `internal/service/game/activity.go` 的 `Contribute` 与 `Standing`
  （模板 `demo/internal/service/game/activity.go.tmpl:309` 与 `:331`），
  窗口定义在 `game/activity/activity.go` —— `WindowSeconds = 300`，
  `ID(nowUnix) = "race-" + (nowUnix - nowUnix%300)`。
- **现象**：`framework-compat` 的 `source-head / demo` 格在 2026-09-20 12:15:01 UTC 失败：

  ```text
  robot action activity_standing: window race-1789906500 counted score=0 progress=0
  for one dungeon clear (and its replay), want 1/1
  ```

  `1789906500` 正好是 12:15:00 UTC，也正好被 300 整除——机器人在上一个窗口
  （12:14:5x）清的本，在新窗口里读的榜。
- **为什么可疑**：`Contribute` 与 `Standing` **各自**用 `time.Now()` 现算窗口 id，两次调用之间没有任何
  东西把读锚定到写入的那个窗口。这不只是测试抖动：一个玩家 12:14:59 清本、12:15:01 打开排行榜，
  看到的就是 0——他的贡献并没有丢（它在上一个窗口里），但界面上没有任何解释。
- **会红的测试草稿**：把时钟固定在 `WindowStart(now)+299`，`Contribute` 之后推进 2 秒再 `Standing`，
  断言"要么读到上一个窗口的成绩，要么明确告诉调用方窗口已滚动"。
- **候选修法**：(a) `Contribute` 把它落进的 window id 返回给调用方，端点带着它查；
  (b) `Standing` 在当前窗口为空时回退看上一个窗口并标明；(c) 机器人动作按窗口边界重试。
  三选一是接口语义问题，不该由搬迁批次拍板。
- **来源**：三仓合一仓 P4 的 framework-compat 实跑。**与合仓无关**——合仓前 codegen main 上同一格
  在 10:52 UTC 通过，只是那一次没有跨过窗口边界。

## W-2026-09-20-04 已分流：→ RR-20260920-08（根因是 `OpTimeout` 不覆盖写闸排队，不是锁泄漏）

- **位置**：`roost-core/remoteentity`（`versioned_lock` / `Assembly` 的 `OpTimeout` 使用路径）与
  `roost-core/nest` 的 dispatch 等待。基线：core `v1.15.17`、codegen 第十八批（Guild）。
- **现象**：单进程。跑一轮机器人（建了公会）→ `run.sh stop game` → `run.sh start game` → 立刻再跑一轮：
  `found_guild` 失败，而服务端的 slow dispatch 记录显示这一次 `handlerFoundGuild` 的耗时是

  ```text
  slow dispatch  handler=handlerFoundGuild  cost_ms=79031
  ```

  而配置里 `remote_entity.lock_ttl = 15s`、`op_timeout = 3s`。**79 秒既不是租约也不是超时。**
- **为何可疑**：
  1. 冷启动（数据库、控制库、Redis 全空）时同一个 handler 稳定在 **~200ms** 完成；连跑三轮都是 200ms 量级。
  2. 只有“前一个化身刚死”这一种情形会出现这个等待；等足够久（实测 ~20s）之后一切正常。
  3. 给客户端加上重试（20 次、每次前等 1 秒）之后，**整个 dispatch 的耗时从 3s 变成 79s**——
     这说明重试并没有各自超时返回，而是**堆在同一把锁后面**；等待者越多，释放越晚。
  4. `op_timeout` 看上去没有约束住这段等待：3 秒的配置下出现了 79 秒的 dispatch。
- **为何不自己改**：三个候选根因的修法完全不同，而手上的证据不足以区分：
  （a）**锁泄漏**——超时返回的那一次事务没有释放它已经拿到的锁，后续等待者全部排队；
  （b）**等待不受 `op_timeout` 约束**——超时只盖在某一段操作上，锁等待在它之外；
  （c）**前任的锁没有被 fence 回收**——versioned lock 有 epoch，理论上新化身可以抢占而不是等到期。
- **会红的测试草稿**：core 侧，一个 remote-managed 实体：进程 A 拿锁并在提交中途被杀（或直接丢弃 Assembly），
  进程 B 启动后对同一实体发 N 个并发事务；断言（i）每一个都在 `op_timeout` 内返回（成功或可重试拒绝），
  （ii）总耗时不随 N 增长。现在第二条应该会红。
- **候选修法**：先定一条契约——“一次远端事务的**总**等待不得超过 `op_timeout`，包括锁等待”，
  然后按根因选：超时路径上无条件释锁（a）、把锁等待放进同一个 deadline（b）、
  或者允许新化身按 epoch 抢占死化身的锁（c）。
- **影响范围**：只影响 remote-managed 实体（demo 里就是 Guild），而且只在**进程刚重启**的窗口内。
  普通运行（冷启动、连跑三轮）全绿，所以第十八批照常合入，这一条单独交审查。
- **来源**：第十八批合入时的验收实跑。

## W-2026-09-20-03 已分流：→ RR-20260920-07

- **位置**：`roost-core/safemap/small.go:121-133`（`MarshalBSONValue` / `UnmarshalBSONValue`）。
  基线：core `v1.15.14`，mongo-driver `v2.6.0`。
- **现象**：驱动的接口是 `MarshalBSONValue() (byte, []byte, error)`，而这两个方法用的是
  `(bson.Type, []byte, error)`。`bson.Type` 是 `type Type byte`——**定义类型，不是别名**，
  所以签名不匹配，接口没有被实现，方法被**静默忽略**。类型于是走默认结构体编码器，
  而它的字段全是未导出的：

  ```text
  SmallSafeMap encoded as: bson.D{}
  round trip lost the contents: &safemap.SmallSafeMap[...]{entries:nil}
  ```

  （一个临时 probe 用例，放两个键进去、marshal、再读回来，稳定复现。）
- **为何可疑**：签名承诺了自定义编码，实现没有兑现，而失败形态是**静默写空**——
  没有错误、没有警告。codegen 的 DAO 生成器把**每一个 map 字段**都生成成
  `*fmap.SmallSafeMap[...]`（`internal/dao/gen.go:356`），所以这个类型离持久化很近。
- **为何现在没爆**：生成的 DAO 不把整个 map 交给 bson，它自己拼 patch（`items.1001` 那种路径），
  转换走 `daoMapDocs`。所以目前是**潜伏的**：只要有人把一个 `SmallSafeMap` 直接放进一个要写 Mongo
  的结构体（或快照载荷），它就会静默变成 `{}`。
- **会红的测试草稿**：上面那个 probe 就是（放进 `safemap` 包）；建议同时加一条
  `var _ bson.ValueMarshaler = (*SmallSafeMap[string, int64])(nil)` 类型断言，让签名写错在**编译期**就红。
- **候选修法**：A. 改成驱动的签名（`byte`）并加上接口断言——最小，但要先确认
  现在有没有人依赖“它序列化成空”这个事实（比如某个文档里真的不想带上这个字段）；
  B. 在仓里搜一遍所有 `MarshalBSONValue` 签名（目前只有这一处），并把接口断言当成规矩。
- **来源**：U-0261 的实现过程。我自己先把签名写成了 `bson.Type`，编译通过、测试照旧红，
  查到驱动接口才发现；回头一看，仓里原有的那一处是同样的写法。

## W-2026-09-20-02 已分流：→ RR-20260920-06（候选根因 a（LOD 筛字段）被排除，packer 忽略 profile）

> **09-22 更正**：分流到 RR-06 是误归因——RR-06 是查它时顺带发现的另一条真缺陷。U-0267 在位、零 subscribe 拒绝，
> 真实环境仍 3/16。本条现为 **RR-20260922-01**（P1），正文保留为现场证据；候选 (b) 仍是主嫌疑。

- **位置**：`roost-core/entitysync`（`SubscriptionCoordinator` 的 prepared batch / content version）、
  `roost-core/room`（`flushStateBatch` 的 dirty 取用与按订阅者分发）、
  `roost-core/entity` 的 `SubjectSyncState` / `SubjectSyncPacker`，以及生成工程的
  `game/scene/runtime/interest.go`（LOD band 与 `SyncProfile`）。基线：core `v1.15.14`（已含 U-0260）。
- **现象**：干净进程、单进程、`-count 16`，每轮稳定 3/16 失败：

  ```text
  scene_expect: after 40s ... "pos_x" never arrived;
  own fields=[_id attr_base attr_final exp items level] own_updates=6 other_updates=104
  ```

  注意三件事放在一起才是线索：
  1. `own_updates=6~7`——这个客户端**确实**收到了关于自己 subject 的更新，订阅是活的；
  2. 收到的字段里始终没有 `pos_x` / `pos_y` / `scene_id`（有时也没有 `equipment`），
     而 `level` / `exp` / `attr_*` / `items` 都在——丢的是**成组的**，不是随机的；
  3. `other_updates≈100`，客户端看得见 16 个 subject——同一条会话、同一条通道一直在正常收别人的帧。
- **为何可疑**：
  - 服务端全程零 `WARN`/`ERROR`，`handlerEnterScene` / `handlerMovePlayer` / `handlerEquipItem`
    的 `result="ok"` 计数等于机器人数；事务都提交了。
  - 40 秒后仍未到达，帧数不再增长——不是慢。
  - U-0260 修掉的 latest-only 覆盖已经不在链路上（客户端 `deltas=0`，全走可靠通道），
    所以这是**另一条**。
  - "同一个 subject 的一组字段对某一个订阅者永久不出现，而其他订阅者正常" 有两个
    自然候选：（a）该订阅者的 profile / LOD band 把这组字段筛掉了；（b）该订阅者的
    baseline 在没有收到那一帧的情况下被推进了（prepared batch 已提交 / content version 已前进）。
    两者的修法完全不同，而我手上的证据不足以区分，所以没有自己动手。
- **会红的测试草稿**：core 侧：一个房间 16 个 subject，每个 subject 也是观察者（自己订阅自己）；
  先全量快照，再让**所有** subject 在同一个 tick 改一个字段并 flush；断言每个观察者都收到了
  **自己那个** subject 的这次改动。若不红，再把 LOD band 加进来（观察者与自己的 band）。
- **候选修法**：先定一条契约——"一个观察者对自己的 subject 永远是最高细节"（如果是 (a)）；
  或者"推进订阅 baseline 必须与该订阅者真的收到那一帧绑定"（如果是 (b)）。
- **来源**：U-0260 修完之后的验收实跑。它把原来混在一起的两条分开了：
  传输层覆盖（已修）与这一条。

## W-2026-09-20-01 已分流：→ RR-20260920-02

**Review 结论（2026-09-20）**：确认 P1。`RoomTransportSink` 把普通 delta 送入 datagram；`AsyncTransport` 对同 stream 采用 latest-only，现有阻塞测试明确证明中间帧会被覆盖；而 entitysync 已在 admission 成功时提交 dirty。后一 delta 不是前一 delta 的超集，被覆盖帧独有的 `pos_x/equipment` 不会重现，也没有 ACK/resync。这一跨层契约完整解释下方 16 客户端实跑，已登记 [RR-20260920-02](REVIEW-2026-09-20.md)，本项不再属于活动 Wanted。以下保留原始候选。

### 原始候选：16 个玩家同场景时，部分客户端永久收不到"自己"的某一次状态变化

- **位置**：`roost-core/statesync`（`Replicator` 的准备 / 提交与 `Reassembler`）、`roost-core/room`
  （`RoomBroadcaster.flush` / `flushStateBatch`、`RoomTransportSink` 的准入）、`roost-core/entitysync`
  （`SubscriptionCoordinator` 的帧准入），以及生成工程的 `internal/service/<game>/scene.go`（`sceneLane.AdmitBatch`）
  与 `cmd/loadtest`（`sceneWatcher.consume`）。基线：core `v1.15.12`、codegen `v1.15.21` 生成的 game-demo 工程，
  **单进程**即可复现（与多进程所有权无关）。
- **现象**：`make dev-run` 起一套，`go run ./cmd/loadtest -count 16`（同一个 scene，同一个出生点）。
  每轮 16 个机器人里有 **2～6 个**在 `scene_expect` 失败：

  ```
  scene_expect: after 10s the client knows 16 subjects (want 2) and 49 frames; "pos_x" never arrived
  scene_expect: after 10s the client knows 16 subjects (want 2) and 48 frames; "equipment" never arrived
  ```

  断言的是**这个客户端自己**的 subject。丢的永远是 `pos_x` 或 `equipment`——即这次场景里对自己状态的
  最后两次改动（`move` 与 `equip_item`）。`count=4` / `count=8` 稳定不复现，`count=16` 每轮复现。
- **为何可疑**（这几条合起来才是这条目的价值）：
  1. **服务端认为全部成功**：`nest_dispatch_total{handler="handlerMovePlayer",result="ok"}` 与
     `handlerEquipItem{result="ok"}` 都等于机器人数（66/66 三轮累计），`result="error"` 的 66 次正是场景里
     故意越界的那一次 move。事务都提交了。
  2. **没有任何告警**：整轮 game 进程日志里 `level=WARN|ERROR` 计数为 0，`scene: push to player` 0 次，
     `admission failed` 0 次。既没有推送失败，也没有准入拒绝。
  3. **不是慢，是丢**：把场景里的 `timeout: 10s` 改成 `40s` 重跑，同样 4/16 失败，
     文本变成 `after 40s ... "pos_x" never arrived`，帧数不变（~50）。**40 秒之后那次改动仍然没有到达，
     也没有任何机制重发它**——delta 不重复，于是这一个字段对这个客户端永久缺失。
  4. 客户端不是静默吞掉：`sceneWatcher.consume` 对 `Reassembler.PushFor` 和解码的错误都会写 `watcher.failure`，
     失败文本里会带出来；这几轮没有出现，说明不是重组报错，而是**这一帧根本没到**（或到了但不含该 subject）。
  5. 丢的总是"最后一次改动"这一点很有指向性：玩家此后就空闲了，没有下一次 delta 把它带上。
     也就是说，链路上任何一次"丢掉一帧就不再补"的地方都能造成这个结果，而 `RoomFrame` 明明带了
     `SessionSequence`（注释写着 "suitable for loss detection and replay"）——**没有任何一端在用它做丢失检测**。
- **会红的测试草稿**：core 侧，建一个房间，注册 N=16 个 subject，每个 subject 都有一个订阅者（同一个 sink），
  在同一个 tick 让全部 16 个 subject 变脏并 flush；断言每个订阅者都收到了**自己那个 subject**的这次变化。
  把 sink 做成"按订阅者分片、每片有界队列"以模拟负载，逐步加压到复现。若 core 侧不红，就在生成工程里
  用 16 个真客户端复现（上面的命令即可，脚本化在 `cmd/loadtest`）。
- **候选修法**（都需要先定契约，所以没有自己动手）：
  A. **丢失检测**：客户端按 `SessionSequence` 发现缺口就请求一次全量（resync），服务端提供该入口。
     这条最符合现有 wire 的设计意图（那个字段就是为它准备的），但要定"谁发现、向谁要、要多少"。
  B. **服务端不丢**：找到那个"丢一帧"的点（房间 flush 的批准入 / lane 的分片 / 订阅者的背压）并改成
     "保留 dirty、下一 tick 重试"。`sceneLane.AdmitBatch` 的注释已经宣称是这个语义
     （"the room keeps the subject dirty and retries"），需要核对它在每条出口上是否真的成立。
  C. **周期性对账**：每个订阅者每 N 秒收一次自己 subject 的全量。最省事，但把"正确性"换成了"最终一致的延迟"，
     而且 N 就是丢字段的可见时长。
- **来源**：第二十批之后复盘"16 机器人为什么一直有红"时查出来的。此前两轮我把这条失败误读成
  "场景断言要求视野里正好 2 个主体"——`subjects: 2` 其实是下限，真正的失败在错误文本的后半句
  （`"pos_x" never arrived`）。记在这里也是为了这条教训：**这两轮的验收都因此漏掉了一个真实的丢数据缺陷**。

## W-2026-09-19-01 已分流：→ RR-20260920-01（根因更正）

**Review 结论（2026-09-20）**：确认 P1，但“负 state version 转 uint64”不是根因；`batch.go` 和 `SetEntityVersion` 已把负值归零。用原错误值放入 `snapshot.Checksum uint64` 可稳定得到完全相同的 Mongo driver 溢出，且 checksum 的高位为 1 是正常散列空间。已登记 [RR-20260920-01](REVIEW-2026-09-20.md)，要求 checksum 与需要数值比较的 version/fence 分开编码，并在 WAL admission 前校验。以下保留原始候选供追溯。

### 原始候选：远端实体第一次真被使用就写不进去

- **位置**：roost-core `remoteentity/mongo_committer.go:184-210`（`applyCommit`，把 `commit.BaseVersion` /
  `NextVersion` / `MarkerEpoch` / `RouteEpoch` / `LockFence` 直接放进 `bson.M`）；
  产生 version 的一侧在 `remoteentity/batch.go:254`（`BaseVersion: uint64(stateVersion)`）。
  基线 core `v1.15.11`。
- **现象**：给 game-demo 加一个 `remote=managed` 的 Guild（第一个真实使用方），端点创建聚合后跑一次
  两实体事务（guild + player），提交阶段报：

  ```
  dataengine mongo: remote projection: cannot marshal type bson.M to a BSON Document:
  12630717602282170696 overflows int64
  ```

  `18446744073709551616 - 12630717602282170696 = 5816026471427380920`，也就是说这个值是
  **`uint64(负的 int64)`**。此后进程再也起不来：启动时的投影恢复会重放这条 WAL 记录，每次都失败退出
  （`mod dataengine start: ... startup projection recovery: ... overflows int64`），只能删 WAL 目录。
- **为什么可疑**：
  1. `uint64(stateVersion)` 没有对负数设防。一个"还没持久化过"的聚合如果用负数当哨兵，
     转成 uint64 就是 1.2e19，Mongo 驱动拒绝，而**拒绝发生在事务里、事后每次恢复都重演**——
     可恢复性比那一次失败更要命。
  2. 三个 fence 字段（`MarkerEpoch` / `RouteEpoch` / `LockFence`）都是 `uint64` 并且都直接进 `bson.M`。
     epoch 与 fence 目前来自 Redis 的递增计数，短期不会越界；但类型上它们**都能**越界，
     而越界一次就是同样的"进程再也起不来"。
  3. `remoteentity`、`ownerroute`、`mirror` 三个包在 09-19 之前没有任何生成工程的使用方
     （见 `docs/feature/GAME_DEMO_TEMPLATE.md` §9.1 第 5 条），这条正是"零覆盖的地方就是缺陷能长期活着的地方"。
- **复现**：`roost project new … -template game-demo` 之上加一个 `remote=managed` 的实体
  （本轮的实现已存好，见下），用生成的 lifecycle `Create` 造聚合，再发一次带它的 Nest 事务。
  单进程即可复现，不需要两个 game 进程。
- **候选修法（都需要先定契约，故未动手）**：
  - 版本这一侧：`stateVersion` 为负时应当**在进入提交路径之前**拒绝并说清原因，而不是转成 uint64；
    或者明确"新建的远端聚合 BaseVersion = 0"的语义，并让 `Create` 走一条远端感知的路径。
  - 存储这一侧：`applyCommit` 把 uint64 写进 BSON 之前要么转成 int64 位模式（并在读回时转回），
    要么用 Decimal128 / 十进制字符串；无论选哪个，**写出去的编码必须和读回来的解码是同一个决定**，
    而且要覆盖"历史文档已经是另一种编码"的迁移。
  - 恢复这一侧：一条投影恢复时必然失败的记录会让进程永远起不来。是否该有"毒丸记录"隔离
    （记下来、跳过、报警）是一个独立的可用性决定。
- **本轮为什么没有自己修**：三处都要先定契约（新建远端聚合的语义、uint64 在 BSON 里的编码、毒丸记录的处置），
  而且都在零覆盖的核心路径上——赶出来的修法下一轮多半会被打回。demo 侧的 Guild 实现（DAO / 实体 / 组件 /
  三个 handler / 端点 / 机器人动作 / 第二个 game 进程的启动脚本）已写完并验证过"生成、编译、单测全绿"，
  暂存在会话 scratchpad 的 `guild-batch18-*`，等这条有结论后接着做。
- **来源**：实施 §9.1 第 5 条（多 game 进程）第一批时自查。

## W-2026-09-18-11 分流结论：→ ARCH-07 配置所有权

- **位置**：roost-core `fctx/context.go:163`（`SetRuntimeConfig(config any)` / `RuntimeConfig() any`）；
  写入者有两个——`app/app.go:133`（进程的 `*viper.Viper`）与 `configdata/configdata.go:858,868`（发布/回滚时写入
  `*configdata.Snapshot`）。
- **现象**：名字与签名都暗示"进程的运行期配置"，实际语义是"最后一个写入者留下的东西"。消费者做类型断言，
  断言失败就静默拿到零值。U-0244 就是这样：demo 的 spawner 从这里读 sid，configdata 发布快照之后断言失败、
  sid 为 0，而 U-0242 的启动校验正确地拒绝了它——**整个 game 进程起不来**，而三仓全套单测、生成工程全包测试与
  doctor 全部是绿的。
- **Review 结论（2026-09-19）**：U-0244 的错误消费者已改走 `Registry.Config()`；全仓当前没有第二个把该槽位断言为
  viper 的生产消费者，因此不新增功能 RR。槽位实际承担 configdata 活动快照与 Nest/fctx 请求捕获，app 写 viper 是不同所有权的混入。
  转 **ARCH-07**：configdata 成为唯一写入者，app 停止写入；旧 API 先改名/弃用，再迁到类型化快照契约。
- **复现**：起一个带 configdata 的进程，在服务 `Init` 里对 `fctx.RuntimeConfig()` 断言 `*viper.Viper`——失败。
- **来源**：发版验证时自查（`docs/bugfix/U-0244-spawner-sid-source.md`）。
- **实施交接**：[ARCH-07 运行期配置所有权](../review/IMPLEMENTATION-RUNTIME-CONFIG-OWNERSHIP.md)。本项已分流，不再属于活动 Wanted。

## 09-18 第三轮分流结果

本轮 10 条候选已全部判定，当前没有未分流的 09-18 Wanted。完整依据见[问题与实施方向](REVIEW-2026-09-18-03.md)、[独立复现](REPRO-2026-09-18-03.md)和[运行记录](../review/REVIEW-2026-09-18-03.md)。

| Wanted | 分流 |
| --- | --- |
| W-01 邮件账本寿命 | → RR-20260918-05，P1 |
| W-02 私有同步字段 mask | → ARCH-06，实现稳定字段元数据；不是默认 packer 的功能 bug |
| W-03 会话关闭回调 | → RR-20260918-06，P2 |
| W-04 syncTopic 裸标识符 | → RR-20260918-07，P2 |
| W-05 单观察者 AOI 预算 | → RR-20260918-08，P2 |
| W-06 ID 空间 | 用户决定的完整 entity ID 与当前 bridge 一致；补契约测试，不改 Core 泛型 SubscriberRef |
| W-07 subject/Point 语义 | 文档契约，不改破坏性 API；已写入 scene 机制文档 |
| W-08 多来源兴趣 | 接受 demo 的 source-refcount + 最高保真策略；关系 feed 留在游戏侧，暂不提升 Core API |
| W-09 全 nopersist Monster DAO | 接受；统一同步权威的收益高于冗余 collection 元数据，不新增无集合语法 |
| W-10 进程本地怪物 ID | → RR-20260918-09，P1 |

以下保留原始候选供追溯，均不再属于活动待判项。

### W-2026-09-18-09 原始候选：怪物没有 DAO 的话"位置权威在 DAO"这条就不成立——demo 的解法是给它一个全 nopersist 的 DAO

- **位置**：roost-codegen `demo/db/def/monster.go`（`MonsterDao` 的四个字段全是 `dao:"nopersist,sync"`）、
  `demo/game/entities/monster/entity.go`（`noPersist=true lifetime=ephemeral`）。
- **与已有约束的冲突**：用户定的规则是"位置权威在 DAO 里，其他都不做缓存，改位置和读位置最终都走到 entity 的 component"。
  一个刷出来的怪没有任何要持久化的东西，按字面理解它不该有 DAO——那样它的位置就只能放在组件的普通字段里，
  于是 demo 里会出现**两种位置权威**，而第一段要同时处理玩家和怪的代码就会挑错一种。
- **实现侧的解法**：给它一个 DAO，但每个字段都是 `nopersist,sync`——不存，但复制。于是"位置住在 DAO 里、经组件读写"
  对所有实体一致，packer 也还是同一个形状（`MarshalSync(mask)` 进出）。代价是生成器会为它产出一个 `monsters` 集合名
  与一整套持久化代码路径，而那条路径永远不会写任何东西。
- **要 review 判的**：(a) 这个解法是否是想要的（还是宁可承认"无持久化实体的位置放组件里"是另一种合法形状）；
  (b) 如果是，`//roost:dao` 是否该支持一个"无集合"的声明，让全 nopersist 的 DAO 不必编造一个 Mongo 集合名。
- **来源**：game-demo 第十三批第三批实施时撞上（§9.4.3）。

### W-2026-09-18-10 原始候选：刷出来的实体 id 由进程本地计数器生成，第二个进程会撞

- **位置**：roost-codegen `demo/internal/service/game/spawner.go`（`monsterUniqueIDBase` + `mintID()`，一个进程内自增）。
- **现象**：怪物是 `noPersist` 的运行期实体，没有账号服务那样的 id 分配器给它发号。demo 用"基数 + 进程内自增"，
  单进程正确、两个进程就会铸出同样的 id——而 entity id 是全局身份，撞了之后 room 的 subject、AOI 的 subject 与
  Nest 的实体寻址会同时指错。
- **为什么不自己拍板**：这与"多 game 进程"那条（§9.1 第 5 条）是同一个问题的两面。候选做法至少三种：
  按 sid 给运行期实体划分 id 段（最省，但要定段宽）、用 account 已有的 Redis `INCR` 分配器（多一次跨服务调用，
  而刷怪在热路径上）、或者让运行期实体的 id 里带上进程标识（改 id 布局，破坏性最大）。
- **来源**：game-demo 第十三批第三批（§9.4.3）。实现侧已在文件头注明这是单进程限制。

### W-2026-09-18-08 原始候选：多来源的兴趣（空间 + 社会关系）合并成一份订阅，合并规则与关系数据来源没有定

- **位置**：roost-core `spatial/interest.go`（空间来源，事件形如 `{Observer, Subject, Enter/Leave/BandChanged, Band}`）；
  `entitysync/subscription.go:178`（`Subscribe` 是幂等的"设定"语义：同键同 profile 直接返回，不同 profile 做切换并重发快照）；
  消费侧 roost-codegen `demo/internal/service/game/scene.go`。
- **背景**：用户建议把"一个玩家永远订阅自己"从 AOI 的特例改成**一类关系**（自己 / 好友 / 同联盟），底层做成通用的
  "兴趣来源"：空间是一个来源，社会关系是另一个来源，都产出同样形状的 Enter/Leave，汇总后才落到 `room.Subscribe`。
  实现侧同意这个方向，它正好绕开了 `spatial` 拒绝自观察（`evaluatePair` 第一行 `observer.id == subject` 直接返回）
  与"必须订阅自己"之间的冲突。
- **现象 / 它强制的东西**：一旦有两个来源，**同一对 (observer, subject) 可能同时被多个来源命中**（我的队友正好站在我旁边）。
  于是汇总层必须按来源计数：第一个来源命中才 `Subscribe`，**最后一个**来源撤销才 `Unsubscribe`。少了这一步，
  队友走远时空间来源发 Leave，会把关系来源仍然需要的订阅退掉——而且这种 bug 只在"两个来源重叠又分开"的时序里出现。
- **要 review 定的两件事**：
  1. **档位合并规则**。来源对 profile 不一致时（空间说"远处，低档"，关系说"队友，全量"）取哪个？
     实现侧默认"取最高保真"（band 最小者胜），但这是产品决定——它意味着一个同盟成员在地图另一头也会收到全量状态，带宽照付。
     与 W-2026-09-18-02（字段掩码常量包私有）连着：档位不能裁字段之前，这条规则实际上没有可观察的差别。
  2. **关系数据从哪来**。好友 / 同联盟几乎一定是别的服务的数据，而 kit 现在没有好友服务；demo 打算先用**已有的**两种关系
     （自己、匹配成队的队友，后者由 matchmaker 填、散场清），把 feed 的形状留出来。要不要为此加一个 kit 服务、
     或者约定"关系来源由游戏自己喂、框架只认事件"，归 review。
- **是否该进 core**：汇总层（多来源 → 一份订阅 + 引用计数 + 档位合并）本身与游戏无关，可能属于 `entitysync`。
  实现侧先在 demo 里落一版，跑通了再提；不想在没有第二个使用方之前就把它定成框架 API。
- **来源**：用户在第十三批第二批设计讨论中提出（§9.4.2）。

### W-2026-09-18-05 原始候选：`spatial.InterestConfig` 对"一个观察者订阅多少格"没有任何上界

- **位置**：roost-core `spatial/interest.go:60`（`InterestConfig.validate`：只校验 `EnterRadius > 0`、`LeaveRadius >= EnterRadius`、
  `LeaveRadius < 2^62`、`Bands` 递增）、`:276`（`resubscribe` 用 `±LeaveRadius` 的盒子取格）、
  `spatial/block_index.go:29-41`（`NewBlockIndex` 只拦**整张地图**的格数 `MaxBlockCount = 1<<20`，且**预分配**全部 `*indexBlock`）。
- **现象**：一个观察者订阅的格数是 `(⌈2·LeaveRadius/BlockSize⌉+1)²`，没有任何一处检查这个比值。
  `LeaveRadius=150, BlockSize=150` 是 9 格（教科书九宫格）；`LeaveRadius=1000, BlockSize=100` 是 441 格；
  `LeaveRadius=5000, BlockSize=10` 是约 10⁶ 格——等于每个观察者订阅整张图，配置合法、构造成功、不报错。
  每格是**两处 map 条目**（`observer.blocks` 与 `blockObservers[block]`），而且观察者每移动一次 `resubscribe` 就要对这个集合做一次差分。
- **为什么可疑 / 为什么不自己拍板**：这是"配错了不报错、只是慢慢变慢"的那类参数——增量 AOI 的全部收益（主体移动只惊动一格的订阅者）
  在比值失衡时被观察者侧的差分成本吃光，而没有任何信号告诉部署方。对照：cube 的 `BlockAOI` 拦的是**单个观察者的窗口面积**
  （`view.SceneConfig.MaxAOIArea`，默认 `BlockSize² × 1024`，`validateObserverRect` 超了直接 `ErrAOIRectTooLarge`），
  roost 拦的是整图格数——两道闸防的不是同一件事。修法有几种取舍：硬上限（拒绝构造）、软上限（构造时告警）、
  或按 `LeaveRadius` 推荐/推导 `BlockSize`（等于把九宫格的惯例写进框架）。选哪条是契约，不是缺陷修复。
- **复现**：`NewInterestManager(InterestConfig{Bounds: 1e6×1e6, BlockSize: 10, EnterRadius: 4900, LeaveRadius: 5000})` 成功返回；
  `AddObserver` 一个观察者后数 `len(observer.blocks)`。
- **来源**：game-demo 第十三批准备 AOI 接线时对照 cube `BlockAOI` 发现（`docs/feature/GAME_DEMO_TEMPLATE.md` §9.4.2）。

### W-2026-09-18-06 原始候选：AOI 的 id 空间与 entitysync/room 的 id 空间没有契约（**id 空间部分用户已定**）

- **位置**：roost-core `spatial/interest.go:392`（`evaluatePair` 第一行 `if observer.id == subject { return }`——自观察靠**同一个 id 空间**判定）；
  `entitysync/subscription.go:42`（`SubscriberRef{Kind, ID, Sid, Key}`）与 `:178`（`Subscribe(ctx, subscriber, state, profile)`，
  主体由 `state.SubjectID()` 给出，那是**完整 entity id**）；消费侧 roost-codegen `demo/internal/service/game/scene.go`
  （`SubscriberRef{ID: playerID}` 用的是 **unique id**，因为 `RoomSessionResolver` 要把它映成 SessionID）。
- **现象**：`InterestEvent{Observer, Subject}` 要直接喂进 `room.Subscribe`，但两端的 id 空间在现有代码里**已经不一致**：
  订阅者用 unique id（如 100971），主体用完整 entity id（如 103402500）。原样接线的话 `observer.id == subject` 永远不成立，
  于是玩家会"观察自己"——今天这恰好是 demo 想要的结果，但它是靠两个 id 空间不一致这个**巧合**成立的，改一次 id 生成或
  换一次 SubscriberRef 的取值就会翻转，而且翻转时没有任何测试会红。
- **为什么可疑 / 为什么不自己拍板**：`spatial` 用一个 id 空间同时表示 observer 与 subject 并据此判自观察；`entitysync` 用两个
  不同性质的 id。两层都没写下"接线时谁负责换算"。而且"一个玩家要不要订阅自己的主体"其实**不是 AOI 问题**
  （永远看得见自己，跟距离无关，也不该被 `MaxVisible` 挤掉），却因为共用 id 空间而被卷进了 AOI 的判定里。
  候选修法至少三条：(a) 约定 AOI 内部一律用 entity id，发事件时由桥接层换算成 `SubscriberRef`，自观察由 `spatial` 正确排除，
  "订阅自己"在 AOI 之外显式做；(b) 让 `SubscriberRef` 也用 entity id，由 resolver 负责换 SessionID；
  (c) 给 `InterestManager` 加一个显式的"自观察策略"配置，不再靠 id 相等推断。(a) 是实现侧倾向的那条，但它把一条约定放在
  谁都没有强制的位置上，值得 review 决定要不要落成框架里的类型或断言。
- **复现**：按当前 demo 的取值构造一次桥接，断言玩家不会收到自己的 Enter——会失败。
- **用户已定（2026-09-18）**：**全程用 entity id**，不转 unique id——entity id 跨 kind 全局唯一（它把 unique id、kind、
  category 打包进一个 int64），而 unique id 只在 kind 内唯一，按它做索引会让 Player 42 与 Monster 42 相撞。
  转换塌进 `RoomSessionResolver` 一处（那正是这个 collaborator 存在的理由）。**仍需 review 判的是**：要不要把这条约定
  落成框架里的类型或断言，而不是只写在注释里——现在没有任何东西阻止下一个人把 unique id 塞进 `SubscriberRef.ID`。
- **"订阅自己"另有去向**：用户建议把它做成一类**关系**（自己 / 好友 / 同联盟），而不是 AOI 的特例，见
  W-2026-09-18-08。
- **来源**：game-demo 第十三批设计 AOI → 订阅桥接时发现（§9.4.2）。

### W-2026-09-18-07 原始候选："subject" 跨两层同名不同物，且 AOI 的点不是实体的 pos——两处都只在实现者脑子里

- **位置**：roost-core `spatial/interest.go`（subject = 一个 `int64` + 一个 `Point`，包对"实体"一无所知；
  observer/subject 是**角色**不是类型，同一个 id 可以两者都是、都不是——`:184` 的注释只写了
  "An id may be both an observer and a subject; it never observes itself"）；对照 `entity/subject_sync.go`
  与 `entitysync`/`room`（那里的 subject 是 `SubjectSyncState`，由 `EntityBase.EnableSync` 建、挂在实体上，
  带 Namespace / SubjectKind / packer）。
- **现象**：两件事没有任何文档写下来：
  (1) **同名不同物**——`spatial` 的 subject 是"某个 id 扮演的可被看见的角色"（可以是刷新点、音源、触发区，完全不必是实体），
  `entitysync` 的 subject 是"实体的可复制面"。接线的人很容易以为是一回事。
  (2) **AOI 的点不是实体的 pos**——观察者的点是**视点**（可以是摄像机、载具、被观战的目标、滞后的插值点），
  主体的点是**被看见的位置**（可以量化、可以降频、隐身单位甚至可以是假点）。框架从不要求它们等于任何 DAO 字段；
  demo 把两者都喂成 DAO 的 pos 是一个**决定**，不是约束。
- **为什么可疑**：这类"只在实现者脑子里的契约"的代价是滞后的——第一个做观战 / 载具 / 摄像机分离的人会先把
  `MoveObserver` 接到角色位置上，然后发现改不动；而 `subject` 的双关会让人以为 AOI 里只能放实体。
  两处都不是缺陷，是**文档与命名的契约**，所以不自己改。
- **候选修法**：给 `spatial` 的包注释加一段"subject / observer 是角色，点是 AOI 的输入而不是某个权威字段的镜像"；
  或者更强一点，把 `spatial` 里的 `subject` 改名（`target` / `visible`）以免与 entitysync 的 subject 混淆——
  改名是破坏性的，取舍归 review。
- **来源**：game-demo 第十三批（§9.4.2）。与 W-2026-09-18-06 同源，可一并分流。

### W-2026-09-18-02 原始候选：生成的同步字段掩码常量是 DAO 包私有的，别的包里的 packer 没法按字段裁剪

- **位置**：roost-codegen `internal/dao/template_dao.go`（`{{fieldMaskName $.Dao.Name .Name}}` 生成 `varietyDaoFieldSyncOnly`
  一类**未导出**常量，见 `internal/dao/testdata/golden/gen_variety_dao.go:59-63`）；消费侧的形状见
  `entity.SubjectSyncPacker.PackSubjectDelta(profile, mask)`（roost-core `entity/subject_sync.go:95`）。
- **现象**：`PackSubjectDelta` 拿到的是 DAO 攒出来的脏掩码，而 packer 通常写在实体包（`game/entities/<x>`）而不是 DAO 包。
  它没法写 `if mask & PlayerDaoFieldLevel != 0`——那个常量在 `db` 包里未导出。game-demo 的 packer 因此只能整体转交给
  `dao.MarshalSync(mask)`（掩码不透明地进、不透明地出），这恰好可行；但凡项目要把状态打成自己的客户端协议（多数项目都要），
  就必须知道哪个 bit 是哪个字段，而现在拿不到。
- **为什么可疑 / 为什么不自己拍板**：这是"跨包契约缺一半"（C4 一类）——框架把掩码交出来，却不交出读懂它的词汇表。
  但补法有好几种取舍：导出常量（污染 DAO 包的公开面、字段改名即破坏兼容）、生成一个 `FieldMaskByName(string) uint64`
  的查表函数（多一层、但可演进）、或生成一个 `<Dao>SyncFields() []FieldMeta`（和 attribute 的 Meta 同形，最贵也最完整）。
  选哪条要 review 定。
- **复现**：在生成工程里新建一个包，写 `if mask & db.PlayerDaoFieldLevel != 0`——编译不过（未导出）。
- **来源**：第十二批给 game-demo 接实体同步时发现（`docs/feature/GAME_DEMO_TEMPLATE.md` §9.3.1）。

### W-2026-09-18-03 原始候选：生成的接入层没有会话关闭回调，所有"谁还在线"的东西只能靠推送失败懒清理

- **位置**：roost-codegen 生成的 `internal/access/player/tcp/server_gen.go`（`Runtime` 只导出 `PushPlayer` / `PushSession` /
  `ActiveSessions`，`internal/roost/render_player_tcp.go` 是模板）；使用方 game-demo 的 `game/chatroom` presence
  与新加的 `internal/service/<game>/scene.go`。
- **现象**：连接断开时没有任何通知。chat 的 presence 只在"推送失败"时把玩家摘掉；scene 同样，且额外要在玩家入场时按
  `ActiveSessions` 扫一遍陈旧成员——否则新玩家订阅到一个早就断线的主体上，失败日志出现在**别人**的入场路径里
  （实跑日志里确实是这样：`scene: peer did not subscribe to the newcomer ... session not found`）。
- **为什么可疑**：凡是维护"在线集合"的东西都要各自发明一遍懒清理，而懒清理的触发点在错误路径上——正确性还在，
  但错误归属和可观测性都是错的。框架侧已经知道会话什么时候关（`server_gen.go` 里 `session.Close(gateway.ErrSessionClosed)`
  就在那儿），只是没往外发。
- **候选修法**：生成的 Runtime 加一个 `OnSessionClosed(func(playerID int64, sessionID string))` 注册点（多播、在关闭路径上调用），
  或发一条 app 事件让感兴趣的服务订阅。要定的是"多播回调"还是"事件"，以及回调在哪个 goroutine 上跑（关闭路径上直接调
  会让一个慢回调拖住连接清理）。
- **复现**：起 demo，杀掉一个机器人进程，观察 `scene` / presence 什么时候才把它摘掉——直到下一次有人向它推送为止。
- **来源**：第十二批（§9.3.1）。

### W-2026-09-18-04 原始候选：`//roost:entity` 的 syncTopic 只认带包名的常量，裸标识符被静默当成字面量

- **位置**：roost-codegen `internal/entity/gen.go:225` `syncTopicExpr` → `isConstExpr`（要求含 `.` 且点后首字母大写）。
- **现象**：`syncTopic=SyncTopicPlayer`（同包常量）生成出来的是 `Topic: "SyncTopicPlayer"`——**常量的名字**，不是它的值。
  写 `syncTopic=clientsync.PlayerTopic` 才会被当成常量引用。codegen 自己的 fixture
  （`internal/entity/testdata/player.go:103`）就是前一种写法，所以生成物里也是 `Topic: "SyncTopicPlayer"`。
- **为什么可疑**：标记的值看起来是个 Go 标识符，实现却按"不带点就是字面量"分流，两种意图在语法上无法区分，
  且错的那种不会报错、只会安静地把 topic 写成一个没人想要的字符串。demo 已改用字面量 `syncTopic=player` 绕开。
- **候选修法**：要么让裸的首字母大写标识符也算常量引用（可能破坏把 `Foo` 当字面量用的既有工程），
  要么在解析时拒绝"看起来像标识符但不是限定名"的值并要求显式引号，要么保持现状但在标记文档里写死"只接受字面量或限定常量"。
  三条取舍不同，交 review 定。
- **复现**：`roost add entity X -sync` 类路径上给 syncTopic 传一个同包常量名，看生成物里的 `Topic:`。
- **来源**：第十二批（§9.3.1）。

### W-2026-09-18-01 原始候选：邮件附件账本的界仍然押在"别的服务会忘掉"上，要不要改成与副本同形

- **位置**：roost-codegen `demo/game/rewards/rewards.go.tmpl`（`ClaimRetentionSeconds = 31 天`、`ClaimExpired(claimedAt, nowUnix)`）
  与 `demo/game/entities/player/bag_component.go.tmpl:104-120`（`ClaimMailReward` 按领取时刻清理）。
- **现象**：保留期的论证是"31 天 > `mail.send_ttl`(720h)，信封过期后拿不到预留、没有发放路径"。本轮核对过，这条**成立**——
  与 RR-20260918-04 的关键区别是它依赖的那条 TTL 真实存在（`roost-kit/service/mail/server_run.go` 明写
  "envelopes expire by Redis key ttl"），而 session 的 Runs/Claims 根本没有 TTL。
- **为什么仍然可疑 / 为什么不自己拍板**：它仍属于"把自己的正确性押在别的服务的存储策略上"这一类论证。
  运营把 `mail.send_ttl` 调到 31 天以上，或者 mail 换一种保留实现，这条链就静默断开，而没有任何测试会红——
  U-0226 就是这样被打回的（那次是论证的前提压根不存在，这次是前提存在但可被配置改掉）。是否要改成与副本同形，
  是契约选择，不是缺陷修复。
- **候选修法**：账本存邮件自己的发送时刻（`mail.Envelope` 的时间，已经在领取路径上拿得到），准入与清理共用
  `rewards.ClaimWindowClosed(sentAt, now)`，于是"记录被清掉 ⟺ 该邮件的领取被拒"，不再引用 `mail.send_ttl`。
  代价：窗口过期的首次领取要变成一条有名字的拒绝（副本那边是 `dungeon_claim_window`），运营口径要跟着定。
- **会红的测试草稿**：在生成工程的 `game/handler/claim_mail_reward_test.go` 里，先记一条 `sentAt = now - 窗口 - 1` 的领取，
  再用一条新邮件触发清理，然后重放第一封——当前实现会再发一叠，新契约下应拒。
- **来源**：修 RR-20260918-04 时对照检查两条同形账本发现（`docs/bugfix/RR-20260918-04.md` 的"未做 / 边界"）。

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

#> 09-18 分流结果：W-2026-09-17-02 → RR-20260917-05（已修，U-0232）、W-03 → RR-20260917-06（已修，U-0230，attribute 运行时进 core）、
> W-04 → RR-20260917-07（已修，U-0231）、W-05 → RR-20260918-01/02（均已修，U-0229 / U-0233）。W-01 转 ARCH-05 实施交接。
> 以下条目保留原文供追溯，不再是待判定项。

> 09-18 晚交付待审：game-demo 把两条写在注释里的"恰好一次"边界实施掉了（codegen `697ae18` 邮件领取账本、
> `0731db9` 送礼 saga 原生步骤 + `add saga` 生成 topic 常量）。**这两条不是疑点，是已实施的改动，登记在此只为交给 review 审查**：
> 做法、边界、实跑证据见 `docs/feature/GAME_DEMO_TEMPLATE.md` §9.2。值得重点看的三处：
> (1) 邮件账本按时间清理，保留期 31 天 > `mail.send_ttl` 720h 的论证是否成立；
> (2) 原生步骤在"业务拒绝"时提交一个只含回执与失败完成结果的事务（无变更），这是否是 saga 契约期望的形状；
> (3) `LeaseDuration`(2m) 与消费者 `AckWait`(30s) 的关系，以及原生 inbox 用 Data Engine 的库而非 saga 库是否正确。

## W-2026-09-17-01 已分流：→ ARCH-05（原始候选正文保留）：其余六个 kit RPC 服务（account / chat / global / activity / platform / rank）是否按 M-06～M-11 的形状下沉 core

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
