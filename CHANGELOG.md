# Changelog

本文件从 v1.6.1 起维护；更早版本见 git 历史。

## [Unreleased]

### Changed

- **依赖 core v1.15.5；`Mail` / `Session` / `Matchmaker` RPC 接口与传输半改为 roost-core 的别名，装配半从 core 的接口生成**（M-11 kit 半，ARCH-01 / ARCH-04 收尾）。`service/mail` / `service/session` / `service/match` 删掉接口文件与 `*_rpc_gen.go`；`alias.go` 追加接口、`BusClient` / `NewBusClient` / `RegisterHandlers` / `Capability`、`ServiceType` / `CapabilityName` / `LocalCapabilityName` / `DefaultCallTimeout` / `Method*` / `Methods` 的别名；`*_rpc_assembly_gen.go` 由 `servicerpc -dir github.com/tjbdwanghaibo/roost-core/service/<x> -emit assembly -out .` 生成（go:generate 在 `alias.go`）。mail 的传输测试与 session 的 `Capability` 断言随传输半迁到 core，kit 的 `rpc_test.go` 只剩 Mod / Server 形状。subject、方法名、capability 名不变；生成工程引用的 `mail.Mail` / `svcmatch.Matchmaker` / `svcmatch.CapabilityName` 仍是 kit 路径。记录：`roost-core/docs/bugfix/M-11-rpc-interfaces-into-core.md`。

- **依赖 core v1.15.4；mail、session 领域实现与 manager 生命周期引擎改为 roost-core 的别名 / 包装**（M-07 / M-08 / M-09 kit 半，ARCH-01 第二、三批 + ARCH-02）。`service/mail` 只剩 `Mail` RPC 接口、生成传输、Mod、server run，`Envelope` / `Mailbox` / `Config` / `Service` / `Deliverer` / 三个 store 接口 / 错误码与 sentinel / `New` / `NewRedisStores` / `Error` / `Code` 全部是 `roost-core/service/mail` 的同名别名（`NewRedisEnvelopes` 的参数类型未导出，不再从 kit 暴露；kit 内无调用方）；`service/session` 同形，`Admin` 也是别名，"Admin 不上总线"的断言留在 kit。`manager.ManagerMod` 变成 `roost-core/manager.Engine` 的包装：公开方法集不变，`ErrManagerRegisterAfterStart` 是引擎 sentinel 的同一指针；生命周期测试随引擎迁 core，kit 只剩 Mod 形状的断言。领域测试随实现迁到 core（mail 的 `TestRedisStoresRefuseInvalidConfigAndEmptyIDs` 也一并搬过去）；持久化 key、JSON 字段、错误码、RPC 方法名不变。记录：`roost-core/docs/bugfix/M-07-mail-domain-into-core.md`、`M-08-session-domain-into-core.md`、`M-09-manager-engine-into-core.md`。

- **全部 `//roost:rpc` 传输用拆分后的 servicerpc 重生成**（M-10，ARCH-04；roost-codegen 未发版，走 go.work 本地生成器）。每个服务包的 `<iface>_rpc_gen.go` 现在只含 wire 类型、handler 表、`BusClient`、capability 包装与名字（只依赖 roost-core），`Server` / `OwnerCapabilities` / `ClientMod` 移到同包新文件 `<iface>_rpc_assembly_gen.go`（唯一 import `roost-kit/mods` 的生成文件）。符号集、subject、capability 名不变，调用方无感；为 M-06～M-08 之后把 RPC 接口连同传输半搬进 core 领域包铺路。记录：`roost-core/docs/bugfix/M-10-servicerpc-split.md`。

- **依赖 core v1.15.3；match 领域实现与 `servicemetrics` 改为 roost-core 的别名包**（M-06 kit 半，ARCH-01 第一批）。`service/match` 只剩 Mod、sweep 循环、`Matchmaker` RPC 接口与生成的传输文件；`Queue` / `Ticket` / `Store` / `Config` / `Grouping` / 错误码与 sentinel / `NewRedisStore` / `Error` / `Code` 全部是 `roost-core/service/match` 同名类型与同一指针的别名，`errors.Is` 与类型断言不受影响；新增 `NewMemoryStore(cfg)`。`NewStore` 不再导出（参数是 core 未导出的队列状态类型，包外本来就调不到）。`service/servicemetrics` 同理别名到 `roost-core/servicemetrics`。领域测试随实现迁到 core；kit 保留 sweep 循环与契约测试。记录 roost-core `docs/bugfix/M-06-match-domain-into-core.md`。

### Fixed

- **match：删掉没有执行者的 `Grouping` 注入参数**（U-0217，C2；RR-20260916-05，T-111，源自实现侧候选 W-2026-09-16-01）。**破坏性**：`NewMod(grouping, reporter)` → `NewMod(reporter)`，`Config.Grouping` 删除；`Grouping` 接口与 `FirstComeGrouping` / `ScoreWindowGrouping` 保留为调用方工具。此前该参数从构造函数一路存进 Config，store 里没有任何路径读它——Enqueue 只入队、Commit 提交指定票、Sweep 只清过期票，成组完全由调用方 Candidates → Grouping → Commit 驱动，项目注入的策略静默失效。审查明确这不是"Enqueue 必须自动成组"，是签名说了实现没做的话；把策略塞进可重试的存储 CAS 回调会带来重复执行与事务窗口延迟，故不采用。codegen 同版本不再生成 `Grouping()` collaborator。`grouping_contract_promises_test.go` 修前红；记录 roost-core `docs/bugfix/RR-20260916-05.md`。

- **activity 的窗口条目有 opening / 确认两段生命周期,派发从持久的 Delivering 索引重试**(U-0191、U-0192,C8;RR-20260914-02/03,T-85、T-86)。
  `OpenActivity` 先进 `Window.Opening`(带 AdmittedAt),`Activities.Create` 之后 `confirmWindow` 在一个 CAS 里无条件挪进 `Keys`;sweep 对 opening 条目:记录存在就代为确认,缺失只有超过新配置 `OpeningGrace`(默认 1 分钟)才回收,且回收只删仍在 opening 的——与 OpenActivity 交错不再把正在建的活动删出窗口。
  完成的活动退出 `Keys` 时同 CAS 进入 `Window.Delivering`(不占 `MaxPendingActivities`),`sweepGroup` 每轮从 `DeliveringActivities`(排序 + 每组游标轮转)出发 DueDispatches→AttemptDispatch,`RetireDelivered` 先 `ensureDispatches` 兜底、全部 dispatch 终态才退出;此前只对本轮刚到期完成的活动派发,notify 收齐完成、heal、退避重试都没有入口。
  新增计数 `sweep.delivering_read_failed` / `sweep.retire_failed`。Redis 记录新字段 `omitempty`,老记录零迁移。测试 `open_window_lifecycle_promises_test.go`、`dispatch_rediscovery_promises_test.go`;记录 `roost-core/docs/bugfix/RR-20260914-02.md`、`RR-20260914-03.md`。
- **mail 的 Update 回调先 clone 再改**(U-0182,C2;RR-20260911-05,T-76,P3)。回调拿到的 `Mailbox` 是结构体浅拷贝,`Entries` / `SettledClaims` 两张 map 仍指向存储对象;`Deliver` 先 insert、再 evict、最后才做容量拒绝,回调返回 `save=false` 加错误时 MemoryStore 不保存新结构体,但 map 上的修改已经发生 —— `Unread` 与 `Version` 维持旧值,条目和墓碑却多了一个。`match` 的每个 Update 回调第一行都是 `current = current.clone()`,mail 的五个回调现在一致。仅 MemoryStore 受影响,Redis 每次解码新对象。`atomic_refusal_promises_test.go` 修前红。修复记录见 roost-core `docs/bugfix/RR-20260911-05.md`。
- **mail 的领取身份按信封的可领取窗口保留,不再按条数**(U-0171,C8;RR-20260911-01,T-65)。U-0165 用条数给墓碑收界,并论证"`ReserveClaim` 本来就拒绝过期信封,所以墓碑只需活得比信封长"——**这个论证是错的**:计数上限保证不了任何时间期限,被挤掉的时刻只取决于后面来了多少条。信封还有一周有效期时,只要再有 `MaxSettledClaims` 条更新的已领取邮件被淘汰,原来那条就被挤掉,邮件重投后又能领出一个新 token。
  现在 `Entry` 记下 `ClaimEnvelopeExpiresAtUnix`(`ReserveClaim` 从信封抄,那里本来就持有它),墓碑带着它、按"这封邮件再也领不了"来老化;U-0165 写下的没有窗口的旧记录才按计数淘汰。保留之后仍然超界时,投递以新增的 `ErrClaimHistoryFull`(码 590115)被拒绝并计 `refused:deliver:claim_history_full`,而不是悄悄忘掉一条身份 —— 拒绝可见,遗忘不可见,这与 `full()` 对"腾不出位置"的处理是同一个态度。`settled_claim_retention_promises_test.go` 修前红。修复记录见 roost-core `docs/bugfix/RR-20260911-01.md`。
- **`Mailbox.clone` 复制 SettledClaims**(U-0170,C2;RR-20260911-02,T-64)。`out := m` 复制的是 map 的头而不是内容:Entries 后面被显式深拷贝了,U-0165 新增的 `SettledClaims` 没有,于是 `Service.Mailbox` 返回的"快照"与存储共享同一张 map,在它上面删一条领取记录就绕过 `Store.Update` 改到了权威状态,那封已领取的邮件下次投递会被当成首次投递。`settled_claim_retention_promises_test.go` 的 `TestMailboxSnapshotDoesNotShareTheSettledClaims` 修前红。修复记录见 roost-core `docs/bugfix/RR-20260911-02.md`。
- **mail 的容量淘汰不再让"这封邮件已被领取"消失**(U-0165,C8;RR-20260910-02,T-59)。`evict` 认为终态条目可以整条丢掉,`deliver` 认为"条目不在就是从未投递":领取并提交的邮件被淘汰后,异步 fanout 的重复消息把它重建为未读且无 token,`ReserveClaim` 铸出第二个 token 并返回同一个附件,按 token 去重的发奖侧就会再发一次。`CommitClaim` 自己的注释早就写明 token"是这封邮件永久的投递键",而淘汰把它删了。
  现在把展示保留与领取身份保留分成两个上限:淘汰带 token 的条目时留一条极小的 `SettledClaim` 墓碑(键在 token 不在 Status,所以覆盖"领取后又被玩家删除"这条路径),墓碑受独立的 `MaxSettledClaims` 约束、按最旧先挤;`deliver` 命中墓碑不复活,`ReserveClaim` 答 `ErrAlreadyClaimed`,`CommitClaim` 带原 token 的迟到重试仍然幂等。计数上限够用是因为 `ReserveClaim` 本来就拒绝过期信封,墓碑只需活得比信封长。`claim_identity_promises_test.go` 四条,修前红。修复记录见 roost-core `docs/bugfix/RR-20260910-02.md`。
- **match 的 Enqueue 幂等重放先校验归属**(U-0164,C8;RR-20260909-05,T-58)。`Requests` 只按 requestID 查,命中就返回原票:同一队列里 player:2 用了 player:1 的 requestID,就拿到 player:1 的票与分数,而自己这次入队意图整个消失。读取侧的 `Ticket` 一直调 `validateOwnership` 并以 `ErrNotPermitted` 拒绝,写入侧的重放分支漏了同一道检查。现在重放分支复用同一个校验,不匹配计 `refused:enqueue:request_subject_mismatch` 并拒绝,同样不透露主人是谁;键空间不变,所以没有数据迁移。`enqueue_request_owner_promises_test.go` 修前红。修复记录见 roost-core `docs/bugfix/RR-20260909-05.md`。
- **session `Enter` 撞 RequestID 的撤回不再误删同 owner 重新取得的 claim**（U-0158，C8；RR-20260909-02，T-53）。U-0154 的撤回先删 run 再按版本删 claim：run 一没，同 owner 的新 Enter 把孤立 claim 清掉建了新 claim，而删后重建的键版本回到 1，落败者按版本删 claim 就把新的删了，owner 再进又成功、两个 run 同时 open。现在先用带 run-id 校验的 `releaseClaim` 释放 claim 再删 run——claim 指向的 run 还 open 时没人能合法替换它。`enter_collision_cleanup_promises_test.go` 修前红。修复记录见 roost-core `docs/bugfix/RR-20260909-02.md`。
- **session `Enter` 对同一 RequestID 的跨 owner 竞争不再两边都成功**（U-0154，C8；RR-20260908-01，T-50）。claim 只串行化"每个 owner 一个 live run"，串行化不了全局 RequestID；两个 owner 并发用同一个 RequestID 时都读到账本为空、各自建 run 拿 claim，最后的账本 `Create` 返回 `created=false` 却被丢弃，落败者被告知成功、重试才发现请求属于别人。
  现在账本写入用 `Update` 的比较交换：键不存在就写入；已存在且 owner 不同，落败者撤回自己刚建的 run 与 claim（都从未交给调用方）并以 `ErrRequestInvalid: request belongs to owner X` 拒绝，计 `refused:enter:request_owner_collision`；已存在且 owner 相同按重放处理。`enter_ledger_collision_promises_test.go`（审查附录的屏障账本收进仓库）修前红。修复记录见 roost-core `docs/bugfix/RR-20260908-01.md`。
- **mail 单读与批读对"键在但值为空"的信封答案一致**（U-0145，C8；T-48）。此前 `Get` 把空值折叠成"不存在"，`GetMany`（List 走的路）对同一个值报解码错误——同一条损坏数据，单读说邮件没了、翻页却失败。现在 `Get` 同样报解码错误。
  `get_consistency_promises_test.go` 对五种存储形状分别走两条路要求同一判决；redis 替身改用 `bytes.Clone` 拷贝，不再把空值混成 nil（此前正是这一点掩盖了差别）。
- **activity / match / chat 的后台 sweep / prune 失败现在会计数**（U-0120 / U-0121 / U-0122，C5；T-47）。此前失败只打日志、下个 tick 重试，指标上与"无事可做"完全一样。
  activity：`dropped:sweep.advance_failed`、`dropped:sweep.due_read_failed`、`dropped:dispatch.attempt_failed`；match：`dropped:sweep.failed`（成功仍报 `ticket.expired`）；chat 存储的 Prune：`dropped:prune.failed`（冲突仍走 `conflict:prune`）。
  三个 `*_promises_test.go` 各一条，去掉计数即红。
- **activity 服务的后台 sweep 现在真的会扫组**（U-0119，与 U-0022 同形态；T-46）。`Server.sweepGroups` 此前是返回 nil 的桩、没有任何配置入口，
  于是没有一个进程在后台调用过 `AdvanceExpired`——宽限窗口只在有人碰到活动时才被兑现。新增 `activity.sweep_groups`（`Config.SweepGroups`），
  循环遍历配置的组；未配置时启动时告警一次并什么都不扫。`sweep_loop_promises_test.go` 两条（配置了组在窗口过期后推进到 complete；无组不推进、干净退出），`TestModReadsSweepGroups`。
- **`scripts/gapmap.sh` 收尾不再 `git clean`**（与 roost-core 同一份拷贝）：采样后只还原被改动的已跟踪文件，未跟踪文件原样保留。

### Added

- **account：collaborator 可实现 `RegistryBound`，Mod 在 `Provide` 里把服务进程的 registry 交给它**。`Verifier` / `Allocator` / `NameRules` 由 bootstrap 在 app 存在之前构造，此前没有任何入口能拿到进程里的能力；而 `PlayerIDAllocator` 的契约要求"计数器必须持久且共享"，等于要求一件拿不到的东西。现在实现了 `BindRegistry(*app.Registry) error` 的 collaborator 在 Redis 查找之前被绑定，返回错误则进程不启动、错误点名是哪一个 collaborator；没实现的不受影响。codegen 的 `game-demo` 模板用它把玩家 id 计数器放进 account 服务自己的 Redis。`bind_registry_promises_test.go` 修前红。

- **`scripts/gapmap/classscan.py`**（与 roost-core 同一份拷贝）：C3 / C4 / C5 / C6 / C7 / C8 的启发式候选扫描；service 十包首轮扫过，无真洞。
- **service/session 的入口与"run 中途消失"守卫钉住**（U-0114，C2）。nightly gap map `service/session` 20 条采样 10 条无覆盖（另 10 条在 `*_gen.go`，模板处已钉）。
  owner 非正、ForceRelease 空 run id、Redis 存储缺前缀 / 请求 TTL 非正；幂等账本指向不存在的 run → `ErrConflict` 且不新开一局；Attach / Finish / Leave 不存在的 run → `ErrRunMissing`；
  run 在 Get 与 compare-and-set 之间消失（resolve、markReleased 各一处，用按次数"失踪"的 RunStore 包装构造）→ `ErrRunMissing`，resolve 失败不归还资源、markReleased 失败前恰好归还一次；
  Get 不存在的 run 是 (false, nil)。`guards_promises_test.go` 五条；回退 10 处守卫各红。
- **dataengine Mod 的能力查找与生命周期守卫钉住**（U-0113，C2）。nightly gap map kit `dataengine` 10 条采样 10 条无覆盖。
  nil 注册表、缺 Mongo / JetStream / Remote Entity / 原子存储各自点名拒绝且不发布能力；未 Provide 的 Start（含 nil Mod）拒绝；Start 之前（无论 Provide 前后）Commit / Enqueue → `ErrCommitterRequired`。
  `mod_promises_test.go` 两条；回退 10 处守卫 8 红，2 处不红：实体访问缺失与 core `engine.Assemble` 的同名拒绝冗余；健康注册表是 `app.NewRegistry` 的内建能力，缺失不可达。
- **gap map 采样器跳过 `*_gen.go`**（B-25）：生成文件是同一模板在每个包的实例，其守卫在模板所在处钉一次即可；采样器现在只统计不采样，并在包级与总计里报告跳过的守卫数。
- **dataengine 仓库装载路径的其余拒绝钉住**（U-0101，C2，B-24 第三项）。nightly gap map 里 `dataengine` 20 条采样 15 条无覆盖。
  未注册构建器的 kind、无持久 DAO 的 kind（`ErrEntityAggregateNotFound`）、DAO 不实现 `PersistedDaoLoader`、DAO 解码出别的 id
  （`ErrEntityAggregateCorrupt`）、存储 schema 与 DAO 不一致且无迁移器（`ErrMigrationUnsupported`）、构造缺 manager / store、nil 仓库
  `LoadEntity` → `ErrStoreRequired`；每条都不向 manager 发布。`entity_repository_load_promises_test.go` 一条；回退七处守卫各红。
  迁移三次仍冲突（`ErrMigrationConflict`）一条需要 SystemCommitter 替身，留待 MigrationRunner 单元。
- **actionflow 的重入检测与运行器守卫钉住**（U-0100，C2，B-24 第二项）。nightly gap map 里 `actionflow` 20 条采样 17 条无覆盖。
  动作从自己的 Start / Tick / Cancel / 过渡钩子里回调 `Start` 换掉自己，外层调用各自返回 `ErrReentrantMutation`，内层装上的动作是唯一
  当前项、被换掉的动作不再被 Start；任务在自己的 Start 里再 `StartMission` 被 `starting` 标志拒绝、不构建；配置缺 registry / 组解析器、
  组未知、id 耗尽（action 与 mission）、空闲时 Cancel、`PlanMission.Start` 缺上下文 / 动作表。`reentrancy_promises_test.go` 三条；
  回退 17 处守卫 13 红；4 处不红均为防御性重复：`Start` 里 finish 后的二次检查（finish 自己已报重入）、`start` 的 nil 检查（registry
  已拒绝 nil 构建）、`StartMission` 里 End / changed 钩子后的两处检查（`starting` 标志已挡住重入）。
- **room 同步总线与信封汇的参数守卫钉住**（U-0095，C2）：NATS / JetStream 两条同步总线对未初始化、nil 消息、空 topic、nil handler
  各自拒绝且不触网（用计数替身证明）；`RoomEnvelopeSink` 的注册 / 注销拒绝房间 0、主体 0、跨房间迁移、未注册注销；nil 汇 / nil 帧函数
  返回 `ErrRoomFrameSinkRequired` 而非解引用。`guards_promises_test.go` 两条；回退二十处守卫各红。
- **remote_entity 所有权标记 / 兴趣注册 / 后端的参数规则钉住**（U-0093，C2）：`ClaimOwnership` 的 id / ownerSid 为 0、`EnterSharedExpected`
  传入已共享租约、`LeaveSharedExpected` 传入本地租约或 epoch 0 各自本地拒绝且**不落 Redis 一次**；声明冲突与 CAS 失败按文案翻译；
  `renewIfNeeded` 对 consumer 0 / 非法 key / 已过期兴趣返回 `ErrRemoteRejected` 且不登记；`NewBackend` 缺任一半拒绝，非原子存储的事务提交
  返回 `ErrRemoteAtomicBatchUnsupported`。`guards_promises_test.go` 三条；回退八处守卫各红。
- **saga 步骤消费者的配置拒绝与 Replay 身份规则钉住**（U-0092，C2）：`SubscribeMongoStep` / `SubscribeDataEngineStep` 对空 stream /
  durable、带通配的 topic、超代理上限的 MaxDeliver / MaxAckPending / NAK 退避、DataEngine 租约不长于 AckWait、以及任一 nil 依赖
  各自拒绝且不订阅；`MongoCommandInbox.Replay` 对同 ID 异摘要的回执返回 `ErrIdentityConflict`，未知 ID 返回"无可重放"，nil 收件箱返回
  `ErrInvalidRecord`；两个收件箱构造器的必填项。`step_consumer_promises_test.go` 三条；回退九处守卫各红。
- **gap map 工具**（与 roost-core 同一份拷贝）：`scripts/gapmap/revertsample.py`、`scripts/gapmap.sh`、`nightly-gapmap` 工作流。
  每日对每个有测试的包做承诺回退采样，报告进 job summary，不阻塞。

### Changed

- **依赖 core v1.15.2**（cache 读穿透等待名额在取消后归还，T-51）；kit v1.14.3。这次一并把 v1.14.2 漏掉的 go.mod pin 从 v1.15.0 升到 v1.15.2。
- **kit v1.14.2 的 go.mod 仍 pin core v1.15.0**：发版时 goproxy.cn 的 sumdb 暂时 404 导致升级未落地、tag 已推不重打；kit 的修复不依赖 core v1.15.1（T-49 是 core 内部投影时序），消费方按发布清单同时取 core v1.15.1 即可，下次 kit 发版再升 pin。

### Changed（测试质量）

- **nats Mod 的 Provide 对真实 NATS 钉住两条守卫**（U-0153，C2，`-tags integration`）：连上之后缺健康注册表拒绝；开了可靠总线却没有 redis Mod 拒绝。读 `ROOST_DATAENGINE_IT_NATS_URL`，随 `dataengine-env.sh test` 运行。
- **toxic JetStream RPC 测试的主题前缀按轮次唯一**：流名早已唯一，但两轮共用 `roost.rpc.>` 主题，在持久化的本机环境里第二轮被 JetStream 以 "subjects overlap" 拒绝（CI 每次新环境所以未暴露）。
- **`dataengine-env.sh test` 的 core 段改跑 `./redis/... ./etcd/driver ./mongo/driver`**：Redis 故障套件在 P2-② 搬到 `redis/driver` 后脚本仍只跑 `./redis`（无测试文件），故障矩阵的 Redis 切片实际上一直没在这条命令里跑；etcd / mongo 驱动的真机守卫测试一并接入（etcd 二进制缺失时明确 skip）。
- **U-0150 留待的四条守卫钉住**（U-0152，C2）。rank：Lua 脚本返回非两元素数组时单读 / 换分报 "unexpected ... result"（覆写 Eval 的替身按脚本返回错误形状）；split 示例：授予失败且取消预留也失败时两个错误都报出（示例里恒成功的 `grantToInventory` 改为包级变量以便替换）；manager：启动途中收到关停且回滚失败时同时说明"被关停中止"与回滚原因，后续管理器不再启动。
- **nil / 参数守卫收尾（kit 十二个包）：service/directory、nest、ops、mods、service/rank、manager、service/chat、service/global、service/global/activity、configdata、room、service/examples/split**（U-0150，C2）。各一条 `*_promises_test.go`，共 97 条守卫回退 84 红；
  Mod 的能力查找是链式同文本，空注册表只能钉住第一条（断言核对被点名的能力），其后各条记冗余（ops 84 / 87 / 90、configdata 53）；directory 160 与 Update 闭包同哨兵、mods 24 与 core `RegisterBatch` 同文本、manager 75 与 DFS 环检测同文本、configdata 77 与 `Store.Load` 的 nil 接收者同文本，均记冗余；rank 211 / 224（Lua 结果形状）、split 78（取消预留也失败）、manager 131（Start 途中被 Stop 抢先）需要更深的替身或并发编排，留待；nats Mod 的 Provide 需真连接。
- **platform 未记录订单的拒绝、回调 / 补发空输入与玩家解析器非正 id 的守卫钉住**（U-0143，C2）。nightly gap map kit `service/platform` 20 条 7 条无覆盖。
  `ReopenDelivery` / `SettleOutOfBand` / `AttemptDelivery` 对未记录订单报 `ErrOrderInvalid` 且不凭空建单、不触达发货器；回调空载荷与空订单 id 在解析 / 读存储前拒绝；解析器交出非正玩家 id 时 `AuthSession` 不签 token；`NewRedisOrders` 缺前缀不能构造。
  `order_guards_promises_test.go` 三条；回退 7 处全红，采样 20 条无一无覆盖。
- **account 角色写入的竞态拒绝与入口校验钉住**（U-0142，C2）。nightly gap map kit `service/account` 20 条 8 条无覆盖。
  `SelectRole` 在 Get 与 Update 之间角色消失 / 换了主人时报 `ErrRoleMissing` / `ErrNotPermitted` 而不是写回登录时间；`ValidateSession` 对签名有效但角色不存在的 token 报 `ErrRoleMissing`；`UpdateProfile` 对不存在的角色拒绝且不建角色；
  `CreateRole` 空账号 id、`UpsertServer` 零 id、`Identity` 空 open id、`NewRedisStores` 空前缀各以对应哨兵拒绝。
  `role_guards_promises_test.go` 三条；回退 8 处全红，采样 20 条无一无覆盖。
- **match 队列读取的未命中 / 错误传播、Cancel 与 Commit 的兜底拒绝钉住**（U-0141，C2）。nightly gap map kit `service/match` 20 条 8 条无覆盖。
  `Ticket` / `Candidates` / `Match` / `QueueLength` 对没有状态的队列是干净未命中、对状态存储错误原样上抛（不当空队列）；`Cancel` 对不存在的票报 `ErrTicketMissing` 且不动其他票；`Commit` 对"同一主体两张等待票"的损坏状态拒绝且不产生比赛；`NewRedisStore` 缺键前缀不能构造。
  `read_guards_promises_test.go` 四条；回退 8 处 7 红，`Cancel` 的队列无状态检查与其后的票查找同哨兵（`clone()` 给空表），记冗余保留。
- **mail 服务入口与 Redis 存储构造的守卫钉住**（U-0140，C2）。nightly gap map kit `service/mail` 20 条 9 条无覆盖。
  `Send` 的过期秒数必须为正；`Deliver` / `List` / `MarkRead` / `Delete` 的玩家 id 必须为正、邮件 id 不能为空，被拒的操作不建邮箱；`NewRedisStores` 的发送账本 TTL 必须为正。
  `entry_guards_promises_test.go` 两条；回退 9 处 7 红，`NewRedisStores` 的客户端 / 前缀两条与 `NewRedisEnvelopes` 的同文本检查互为双份，记冗余保留。
- **etcd 本地镜像的配置守卫钉住**（U-0086，C2）。空前缀（会 watch 整个键空间）、缺 Decode / Encode / Clone、重试窗口上限低于
  下限、nil 客户端各自在构造期拒绝。`local_mirror_promises_test.go` 一条；回退三处守卫各红。
- **mongotest 替身的拒绝契约钉住**（U-0084，C2）。这个替身替代了 kit / service 绝大多数单元测试里的 Mongo，它的拒绝就是那些
  测试实际锻炼的契约：find-and-modify 三族的未命中返回 `ErrNotFound`（upsert 除外且确实插入）、`FindOne` 未命中、更新不得
  把文档搬到另一个 `_id`（拒绝且原文档不变）、重复 `_id` 插入是 `ErrDuplicateKey`。`contract_promises_test.go` 一条；
  回退三处守卫各红。
- **ai 行为树注册表与文档解析器的每条规则按文本与 JSON 路径钉住**（U-0083，C2）。原 `TestParseTreeFailFast` 只断言错误类别。
  注册：空名 / 空工厂、重复条件、重复动作；解析：schema、缺 root、缺 `node`、未知节点、未知动作 / 条件（带路径）、guard
  缺 condition、repeat 缺 child、cooldown ticks 非正、parallel 未知策略、嵌套超 64 层。`tree_parser_promises_test.go` 两条；
  回退五处守卫各红。
- **spatial 兴趣管理器的配置与边界守卫钉住**（U-0081，C2）。空边界 / 块尺寸为零、进入半径非正、离开半径小于进入半径、
  离开半径会溢出、距离带非正 / 非升序七种配置各自拒绝；世界外的点与未注册的 id 在增 / 移 / 删六个入口各自拒绝，
  被拒绝的移动不改变位置。`interest_promises_test.go` 两条；回退三处守卫各红。
- **syncstream 订阅端的尺寸上限与分片规则钉住**（U-0082，C2）。用真实 Publisher 铸出信封再单点变异：信封超
  `MaxEnvelopeBytes`、分片数超 `MaxChunks`、分片下标越界、要求校验和却没有、校验和不符、解压后超 `MaxDecodedBytes`、
  报文载荷超 `MaxPayloadBytes`——订阅端是同步总线上的信任边界。`subscribe_promises_test.go` 一条；回退五处守卫各红。
- **nats 客户端的错误翻译与参数校验钉住**（U-0079，C2）。gonats 的 timeout / no responders / closed / draining 各翻成
  roost-core 的哨兵（翻错会让调用方走错分支：把连接关闭当超时重试，或把无响应者当终态放弃）；空 / 带空白的 subject 与
  queue、nil handler、非正 request 超时、无连接的客户端各自拒绝。`client_promises_test.go` 两条；回退五处守卫各红。
- **nettransport 会话注册与批量准入的上限钉住**（U-0080，C2）。零会话 id、重复注册、超 `MaxSessions`、关闭后注册各自的哨兵；
  `AdmitBatch` 逐帧校验：必须指定会话且恰有一条通道、可靠消息超 `MaxReliableBytes`、数据报批超数 / 空包 / 超长——全部在
  触碰任何队列之前拒绝，压线的帧放行。`admission_promises_test.go` 两条；回退四处守卫各红。
- **dataengine 聚合装载的五种损坏形态钉住**（U-0078，C2）。kit 本地 gap map `dataengine` 19/20 无覆盖。同一 id 两份文档、
  文档键指向别的实体、构建器把同一资源声明两次、nil DAO 构建器、远端托管实体缺版本包——每种都以 `ErrEntityAggregateCorrupt`
  点名资源拒绝，且聚合不发布到管理器。装载是"坏的存储答案变成活实体"的唯一入口。`entity_repository_promises_test.go`
  一条（5 子用例）；回退四处守卫各红。
- **actionflow 的冻结组与任务独占守卫钉住**（U-0077，C2）。kit 本地 gap map `actionflow` 20/20 无覆盖。冻结的动作组拒绝
  `Start`（不留状态，`Recover` 后恢复）；`StartMission` 对 kind 0 且无默认拒绝；正在运行的任务拒绝被替换时，替换者不被
  构建、当前任务不被结束。`promises_test.go` 两条；回退三处守卫各红。
- **nestwal 的目录布局与帧头损坏检测钉住**（U-0053，C2，B-20）。此前只有"载荷校验和被改"一条测试。现在：段号不连续
  （中间缺一段）与"日志从段 1 之后开始却没有任何确认"两种目录损坏在 Open 时以 `ErrCorrupt` 命名拒绝，而不是把空洞
  当作"什么都没发生"回放；帧头的魔数、头 CRC、长度三个字段各自一条拒绝——魔数和长度被头 CRC 覆盖，测试改字段后
  **重算 CRC**，让被测规则成为唯一能触发的规则（否则被 CRC 规则掩盖、回退时绿）。`corruption_promises_test.go` 两条；
  回退五处守卫各红。
- **saga 消费者的配置拒绝与两条入站解码路径钉住**（U-0051，C2）。脚本回退采样 40 条守卫 37 条全绿。`SubscribeNestStarts`
  的七种不安全配置（空 stream / durable、带通配的前缀、处理超时不小于 AckWait、超 broker 上限的 MaxDeliver /
  MaxAckPending、超一天的 NAK 退避）各按错误文本拒绝且不订阅；`decodeStepCommand` 对 nil / 超 8MiB / 异版本 / 校验失败
  的拒绝；`handleNestStart` 对超大帧、缺 effect id、异 topic、解不开的 start 载荷全部 **permanent** 拒绝且不到达 starter
  ——用能独立通过的 start 载荷做夹具，让"缺 id / 异 topic"只能由信封规则拦下（否则被载荷解码失败掩盖，首版测试正是如此）；
  收据 TTL 不能表示为 int32 秒时 `EnsureInfrastructure` 拒绝。超大帧那条守卫是纵深防御：去掉后 JSON 解码仍会拒绝，
  记为"冗余但保留"。`promises_test.go` 四条；回退六处守卫各红。
- **remoteentity 写批次的步骤顺序、准入上限、围栏与 Mod 的 sid 要求钉住**（U-0049，C2）。脚本回退采样 40 条守卫 37 条全绿。
  现在钉住：prepare → finalize → commit 之外的每一步都返回 `ErrRemoteCommitNotFinalized`（提交 / 不确定早于 finalize、
  失败或零事务的 outcome、二次 finalize、abort 后再标不确定——二次 finalize 若被放行会在 WAL 已拿到提交后改写它）；
  超过 `MaxWriteBatch` 报 `batch=N max=M`；记录过释放失败的管理器对新写入围栏并带原因；`RemoteEntityMod` sid 为 0 拒绝
  初始化。`mongo_committer` 的"事务 id 复用但内容不同"在采样里显示无覆盖，核对后是被 `ApplyRemoteCommitsInTransaction`
  的同类检查先拦下（冗余互掩，见 U-0035 同型），已有测试。`promises_test.go` 三条；回退五处守卫各红。
- **nestwal 的选项校验、编解码拒绝、确认围栏、健康阈值与检查点解码逐条钉住**（U-0048，C2）。脚本回退采样 40 条守卫
  37 条全绿。现在按错误文本断言：空目录 / 未知 writer 版本 / 负保留数 / 段小于最大记录四条选项规则；空记录、非法
  durability、条目过多、记录过大、未知 writer 版本五条编码拒绝；`Ack` 对零围栏与"越过日志尾"的拒绝（否则下次重开会跳过
  从未投影的记录）；磁盘预算在准入处拒绝且不把日志标成不健康、最老未确认年龄超限由 `Healthy` 报出；检查点文件截断 /
  异魔数 / 校验和翻转三种拒绝。`promises_test.go` 五条；回退七处守卫各红。

### Added

- **toxiproxy 故障矩阵第五切片：JetStream RPC 半开**。`nats` 包新增集成测试：用 NatsMod 按生产配置起一条 JetStream 传输的
  RPC（`nats.rpc.transport=jetstream`），基线调用成功后把三个 NATS 代理的下行黑洞化，带 500ms 截止期的 `CallReliable` 在
  502ms 返回（publish 阻塞在确认上、被 ctx 截止期打断），网络恢复后同一条 bus 立刻恢复服务。RPC 路径尊重调用方截止期——
  与 Redis 客户端（U-0061）不同，这条路径本来就对。
- **toxiproxy 故障矩阵第四切片：Redis 延迟**。`latency` toxic 3s 下 `Acquire` 必须在调用方截止期内返回（首跑抓到上面那条缺陷），
  超时的 SETNX 按 uncertain 处理、恢复后经 `Release` 协调复用。Redis 半开（`timeout` toxic）与"回复被吞"形态相同，第二切片的
  两条测试已覆盖，不另开。
- **toxiproxy 故障矩阵第三切片：NATS 半开**（B-15）。`timeout` toxic（timeout=0）把三个 NATS 代理的下行黑洞化——连接不断、
  字节不回，客户端拿不到 ack 也拿不到错误。测试钉住：提交 + 投影 2.5s 内完成（总线不在持久路径）；outbox 的发布在有界时间内
  失败而不是永远挂住（`PublishFailures ≥ 1`，来自 ping 超时 → EOF）；效果保留在 outbox；网络恢复后**恰好一次**送达——
  broker 可能已经存下那条没 ack 的发布，靠 `Msg-Id = effect ID` 去重。首跑发现客户端从 INFO gossip 学到成员真实端口、
  重连时**绕开了代理**（`reconnected url=…:14222`），于是有了下一条。
- **`nats.ignore_discovered_servers`**（kit 层配置，默认 false）：让客户端只走配置的 URL，不跟随集群 gossip 重连到成员
  广告地址。代理、NAT、故障注入这三种部署下没有它，客户端会静默逃出运维配置的路径。集成夹具在代理模式下自动置 true；
  `options_discovered_test.go` 钉住默认关、开了到达连接选项。
- **不变量 ③ 删除防复活的真实 Mongo 测试**（remoteentity）：删除提交落库后，旧 fence 在正确 base version 上的迟到写入被
  `ErrRemoteVersionConflict` 拒绝、meta 仍是 tombstone、数据文档不复现；当前 fence 的写入允许且是新版本（显式重建，不是复活）。
  故障矩阵四个不变量至此各有至少一条真实依赖上的测试。
- **toxiproxy 故障矩阵（第二切片：Redis）**。隔离环境新增一个 Redis 节点（16379，`--set-proc-title no` 让脚本能按命令行认领自己的进程）并由 toxiproxy
  代理（26379）。两条 `integration` 锁测试：`Release` 的回复被网络吞掉 → 锁进入 uncertain，同一对象拒绝再 `Acquire`，网络恢复后
  `Release` 以值守卫删除收敛、锁可复用，且不会误删另一持有者；`SETNX` 的回复被吞掉 → 不重试、按 uncertain 处理，收敛后 key 已释放。
  这是 U-0012 的契约第一次被真实丢包驱动而非脚本化客户端。CI 与 nightly 安装 `redis-server`；集成脚本包列表加 `./redis`。
- **toxiproxy 故障矩阵（第一切片）**。隔离环境脚本在装了 `toxiproxy-server` 时为三个 NATS 节点各起一个代理并导出
  `ROOST_DATAENGINE_IT_TOXIPROXY_URL` / `ROOST_DATAENGINE_IT_NATS_PROXIED_URL`；`heal` 同时清空 toxic。两条
  `integration` 标签的网络故障测试：① 对全部 NATS 节点加 3s 延迟，提交与投影仍在 2.5s 内完成（提交点是 WAL +
  Mongo，总线不在同步路径上），延迟清除后效果恰好投递一次；② 提交期间对全部 NATS 连接做 reset_peer，提交仍
  被接纳、outbox 保留效果、网络恢复后恰好投递一次。没装 toxiproxy 时这两条 skip 并说明；`ROOST_IT_TOXIPROXY=1`
  时缺 toxiproxy 直接失败。新增 `nightly-fault-matrix` 工作流（每日 03:00 Asia/Shanghai，可手动触发）以
  `ROOST_IT_TOXIPROXY=1` 跑整套集成测试。Mongo 不走代理：副本集发现会把驱动引到成员各自的地址，代理会被绕开。
- **运行时观察进指标与 ops**（方向一）。statslog 每次采集把 `runtime.goroutines`、`runtime.heap_alloc_bytes`、
  `runtime.heap_sys_bytes`、`runtime.sys_bytes`、`runtime.num_gc`、`entity.count`、
  `entity.count_by_category{category}`、`entity.count_by_kind{kind}` 写成 gauge，ops 的 `/metrics` 与 Grafana 直接
  可见；ops 新增 `GET /statsz`，返回 statslog 当前一次观察的 JSON（进程未装配 statslog 时 404 并说明，而不是空 200）。
  内存以进程堆为观察量：实体自身的占用没有分配追踪无法归属，实体侧给数量。`StatsLogMod.CollectStats()` 导出。

### Fixed

- **Redis 客户端不把调用方的 ctx 截止期带到网络上**（U-0061，C8，故障矩阵第四切片发现）。go-redis 默认 `ContextTimeoutEnabled=false`：
  命令等待回复只看 `ReadTimeout`，不看 ctx。对 Redis 注入 3s 延迟时，带 500ms 预算的 `Acquire` 等了整整 2s（读超时）才返回——
  锁之后的每个处理器都跟着停 2s；原有两条丢回复测试的 700ms 预算实际也等了 2s。单机与集群客户端都改为 `ContextTimeoutEnabled: true`
  （没给截止期的调用方仍由 `ReadTimeout` 兜底）；toxic 夹具改用 kit 自己的构造器，不再手写一份近似的客户端选项。
  `client_deadline_test.go` 钉住两种客户端的选项；`TestToxicRedisLatencyKeepsAcquireWithinItsDeadline` 对真实延迟断言
  501ms 返回、恢复后可协调可复用。
- **v1.12.4 的 `integration` 构建编译不过**：U-0037 的单元测试文件声明了 `waitFor`，与 `failover_integration_test.go`
  （`//go:build integration`）里同名的辅助函数重复。`go test ./...`、`go vet ./...`、pretag 都不带 tag，全绿；只有 CI 的
  `go vet -tags integration` 与 integration job 红——而当时盯的是 `codeql` 工作流的结果（按"main 上最新一次运行"取的，
  不是按工作流名）。改名 `waitUntil`；pretag 新增 `go vet -tags integration ./...`；教训记入 T-41。
- **nats：JetStream 的 `Ack` / `Nak` / `Term` 失败被静默丢弃**（U-0036，C5）。处理器成功后 `Ack` 失败（连接已关、消费者被删）
  只会让 broker 在 AckWait 后重投——at-least-once 允许——但没有任何计数或日志，运维看到的是"处理器反复收到同一条"，
  和"处理器一直失败"分不开。结算路径抽成 `settleJetStreamDelivery`：失败时计数
  `nats.jetstream.settle_failures.total{op=ack|nak|nak_delay|term}` 并打一条 Warn（带 subject / stream / consumer / 序号）。
  `jetstream_settle_test.go` 用可注入失败的 `gojs.Msg` 替身钉住三种 op 各计一次、成功路径不碰计数器。
- **dataengine：outbox 认领循环对 store 失败只加计数、不出声**（U-0037，C5 / C8）。`RunOnce` 的错误在 `run` 里被 `_, _ =`
  吞掉；Mongo 停一小时，日志里一小时什么都没有，health 行也只报 `publish_failures`（saga 的 health 行两侧都报）。现在
  失败**转折**各打一条：连败开始 Warn 一次、恢复 Info 一次（不是每次轮询一条——100ms 间隔下那是每秒十行同样的错），
  被自身 ctx 取消的轮询不算失败；health 行加 `store_failures=`。三条测试：连败 5 次只有 1 条 Warn、恢复恰好 1 条 Info、
  停机不留"failing"尾巴；`dataEngineHealthMessage` 两侧都在。
- **nestwal：`TestWALCloseDrainsAdmittedAppends` 在慢机器上偶发 `append 0: nestwal: closed`**（v1.12.2 tag 的
  Windows 首跑）。测试的前置条件"全部 append 已被接纳"只等了"队列里有一条"，Windows 上 Close 抢在 31 个
  goroutine 到达 `Append` 之前，它们得到的 `ErrClosed` 是合法的。`Stats` 新增 `Admitted`（接纳计数；
  `Admitted − Appended` 即在途量，`Queued` 在写入协程取走一批后就看不见了），测试等到 `Admitted == 32` 再
  Close。收敛单元 U-0027。
- **dataengine / saga / remoteentity 三个 Mod 在真实进程里装配不起来**（U-0025，C4）。它们的 `DependsOn`
  写的是 `mods.ModHealth`（app.Registry 的内建项，不是 Mod）和 `mods.ModNatsJetStream`（nats Mod 发布的
  capability，不是 Mod），而 app 按 Mod **名字**解析依赖：任何带数据引擎的进程启动即
  `mod dataengine depends on health: unknown mod dependency "health"`。本仓的集成测试都是手工 Init/Provide、
  不经 app 的依赖排序，所以从未发现；roost-codegen 生成的工程第一次真的启动 game 进程时暴露（默认 mods 含
  nest → dataengine，也就是**默认生成的工程一个都起不来**）。现在依赖只写 Mod 名（dataengine 无硬依赖、
  可选 mongo / nats / remote_entity；saga 依赖 mongo / nats；remote_entity 依赖 redis / room[/ mongo]）。
  根目录 `mod_dependencies_test.go` 构造全部 14 个 kit Mod，钉住"每个依赖名都是某个 kit Mod 的 Name()"。

### Added

- **remoteentity：真实 Mongo 上的 fence 竞争测试**（`mongo_committer_integration_test.go`，`integration` 标签，
  B-10 / FEATURE_LOGIC §4.2 第五条）。同一实体同一 base version 上高低两个 lock fence 并发提交：恰好一个
  落库，败方得到 `ErrRemoteVersionConflict` 进入既有隔离流程；随后"版本对、fence 低于已存"的提交同样是
  版本冲突，"版本对、fence 相同"的提交通过。此前这条不变量只有 mongotest 假客户端上的证据。
  `scripts/integration/dataengine-env.sh test` 的包列表加入 `./remoteentity`，CI 的 integration job 随之覆盖。
  收敛单元 U-0023。

### Added

- **CI `integration` job：真跑集成套件**。此前 `//go:build integration` 的八个测试文件只被
  `go vet -tags integration` 编译、从不执行（2026-09-02 审计 F8）。新 job 在 ubuntu runner 上装
  mongod 8.0 / mongosh / nats-server v2.14.5 / jq / nc，先跑环境脚本自检，再跑与本地完全相同的
  `scripts/integration/dataengine-env.sh test`，失败时打印节点状态，无论成败都 `down`。版本与
  本地开发环境一致并以 env 变量钉死。**尚未在 GitHub Actions 上跑过第一次**：actionlint 通过、
  同一条命令本地三个包全绿（U-0003 之后），首次远端运行的结果要单独确认。收敛单元 U-0010。

### Fixed

- **nestwal：重放循环与 `Flush` 重叠时同一条记录被 apply 两次**。一次重放 pass 是"从 ack fence 读
  → apply → publish → ack"，WAL 只串行化了"读"：另一条 pass 在前者读完、ack 未落地的窗口里进来，
  从旧 fence 再读一遍、再 apply 一遍。`MutationApplier` 契约允许（at-least-once、幂等），所以
  不丢不坏，但两条 pass 扫同一段纯属浪费，也正是 `TestCommitterEnqueueHoldsReplayUntilReleased`
  在慢 runner 上报 `apply calls=2` 的原因。现在 `replayPass` 整体持 `replayMu`，运行循环与
  `Flush` 串行；新增 `testBetweenReplayAndAck` 构造期 seam，测试把第一条 pass 停在读与 ack 之间、
  放第二条进去，修复前确定性地看到 ≥2 次 apply，修复后恰好 1 次。`-race` 与 `-count=3` 绿。
  收敛单元 U-0013。
- **B-05 回退验证补齐的测试（U-0012）**。对 FEATURE_LOGIC M2/M3/M5–M9 逐项临时回退实现、看哪条
  测试变红：9 处原本就有守卫，6 处回退后无一变红，现在都有了——
  `redis`：distLock 的 TTL < 1ms 拒绝、SETNX 回复丢失进入 uncertain 态并阻止重获直到 Release
  对账（服务端已应用/未应用两种结局）、Release 回复丢失同样 uncertain、owner token 每次获取
  不同；`nats`：`validateSubscription` 拒绝 nil handler；`mongo`：`BulkWrite` 未知模型类型带
  index 报错且不触碰 driver。另把 `TestInvokeNatsHandlerContainsPanic` 从"调用后置一个永远为
  true 的标志"改为断言 `nats.subscription.handler_panic.total` 计数 +1——原测试对"panic 被吞
  掉但没上报"完全看不见。redis 侧新增 `scriptedRedis`：只实现 SETNX 与释放脚本、其余方法保持
  nil-panic 的求值型替身。
- **集成环境脚本只能在 macOS 上跑**。`common.sh` 把唯一允许的测试根目录硬编码为 `/private/tmp/…`，
  `dataengine-env.sh` 的 `reset` 安全检查和 `GOCACHE` 默认值、`perf/dataengine.sh` 的缓存目录
  也各写了一份；Linux 上 `/private` 不可创建，`integration` job 首次远端运行在起环境前就退出。
  现在根目录取 `$(cd /tmp && pwd -P)`（macOS 解析到 `/private/tmp`，Linux 保持 `/tmp`），其余路径
  都从它派生；"根目录固定、拒绝任意路径"的安全属性不变。
- **tag 上的 release-hygiene 被第三方间接伪版本卡住**。"tagged release uses tagged dependencies"
  对 go.mod 里任何伪版本都报错，而 viper → locafero → `sourcegraph/conc` 的间接依赖只有伪版本，
  从 v1.11.3 起每个 kit tag 的这一步都是红的。该步骤存在的目的是"kit tag 不得依赖未发布的
  roost-core / roost-skill / roost-service"，现在正则限定到 `tjbdwanghaibo/roost-*`。
- **govulncheck：GO-2026-6061**（google.golang.org/grpc v1.79.3，经 etcd 客户端间接引入）。
  升到 v1.83.2，govulncheck 全绿。v1.11.3 与 v1.12.0 的 released-core (ubuntu) 都因此失败。
- **`TestStatsLogStopWithContextReturnsWhenFlushIsBlocked` 让 flush goroutine 活过测试**。
  `defer close(release)` 在测试结束时才放开被阻塞的 provider，flush 随后往 TempDir 写文件，与
  runner 的清理竞争——慢机器上报 `TempDir RemoveAll cleanup: directory not empty`。现在测试
  自己放开 provider 并等第二次 `StopWithContext` 真正结束。F13 一类：goroutine 是谁起的谁收。
- **CI 基准步骤指向已改名的包**。`ci.yml` 里房间同步的三条基准仍写 `./sync`，而该包在
  v1.10.0 已改名为 `room`；上面的 `go test ./...` 一直绿，这一步却每次都失败。现在改为
  `./room`，并新增根目录测试 `TestCIWorkflowPackagePathsExist`：工作流里每个 `./pkg`
  字面量都必须是本模块里真实存在的目录，下次改名忘了工作流会在普通 `go test` 里变红，
  而不是在 CI 里。这是 2026-09-02 审计"跨包字面量耦合"一类的同族缺陷——只是这次
  字面量写在 YAML 里。收敛单元 U-0001，见 roost-core `docs/history/ledger.md`。
- **集成环境的 NATS 就绪判定没等 JetStream 元 leader**。`scripts/integration/lib/nats.sh`
  的 `nats_cluster_ready` 只看 JetStream 已启用和路由数，而建流要等元集群选出 leader；
  冷启动后包内第一个执行的 `TestRealMongoPrimaryFailoverContinuesProjection` 在
  `ensure effect stream` 上等满 30s 超时，在注入任何故障之前就失败，且两轮完整运行都
  确定性复现。现在每个节点的 `/jsz .meta_cluster.leader` 非空才算就绪，`status` 也打印
  它；修复后完整套件三个包全绿。这是 F10 一类的同族缺陷——就绪检查是对真实条件的
  宽容替身。收敛单元 U-0003。
- **`syncstream.Publisher` 的帧没有投递身份**。每一帧的 `SyncMsg` 只带 `(topic, key, sequence,
  sid, part)`，JetStream 以此元组去重：同一进程重启后从头重发同一序号，在 broker 去重窗口内
  与重启前的帧撞键，新帧被当重复丢掉，客户端无声地少收一帧。现在每帧经 core
  `syncbus.DeliveryIDs` 取进程唯一的身份；新增测试用两个同 sid 的发布者重发同一序号，断言所有
  帧身份两两不同。收敛单元 U-0009。
- **Remote Entity：无 fence 的锁工厂现在在构造和 Provide 时被拒绝**。`batch.go` 此前用鸭子类型
  `interface{ Fence() uint64 }` 探测锁能否给出 fence，拿不到就在每次共享操作时以 `ErrRemoteFenced`
  拒绝——fail-closed，但太晚、太吵，且在指标上与真正的 fence 冲突无法区分。现在改用 core 的公开
  契约 `redis.IFencedVersionedLock`；`newRemoteEntityManager` 构造时用一把探针锁（无 I/O）检查
  工厂，不合格则记录错误、`getOrCreate` 不再创建任何 wrapper，`RemoteEntityMod.Provide` 直接
  返回该错误让进程在启动时停下。FEATURE_LOGIC §4.2 第 6、7 项。
  同时补两条 §4.2 要求的确定性测试：第一代持有者租约过期、第二代取得锁后，第一代的迟到 unlock
  不能删掉第二代 owner、不能改版本、只能拿到 `ErrVersionedLockNotOwned`（机制原本就正确，缺的
  是测试）；无 fence 工厂在构造与创建两处都被拒（修复前该测试红）。收敛单元 U-0011。

### Changed

- **P3b：Mod 瘦身**——nats / etcd / redis / dataengine / saga / remoteentity 六个 Mod 改为持有 core 的 `Assembly`：`Init` 不变，`Provide` = Lookup + `Assemble` + 注册能力 + 健康注册，
  `Start` / `Stop` 转交；构造顺序、失败回滚与驱动内部（`Raw()`）全部在 core（core ≥ v1.15.0）。Mod 构造器签名与能力名不变；错误可见性方向的三处差异见 core `docs/history/P3b_mods.md` §3.1。
  新护栏 `assembly_boundary_test.go`：kit 非测试代码不得调用 `.Raw()`。`saga/mod_test.go` 随 `drainSubscriptions` 搬到 core。
  `scripts/perf/dataengine.sh` 删除（指向的 `./nestwal` 已随收敛搬走），现位于 roost-core `scripts/perf/`。
- **go 指令 1.25.0 → 1.27.0**，与 roost-core / roost-codegen / roost-service 和
  `go.work` 统一。取 1.27.0 而不是最新的 1.27.1：一个补丁级的 go 指令什么都买不到，
  还会让停在 1.27.0 的工具链去下载一个新工具链。

  **代价写在明处**：这是每个消费方都要满足的工具链下限。roost-codegen 的
  `ci/framework-release.yaml` 里声明的 consumer lane 因此从 `[1.25.x, 1.26.x]` 变成
  `[1.27.x]` —— Go 1.25/1.26 的工具链构建不了本仓，那两条 lane 不是"没测"而是
  "不可能通过"。

### Added
- **`redis` 客户端实现 `IRedis.MGet`**（接口在 `roost-core` 新增，**需要与 core 同批
  发布**）：一次往返读多个 key，结果按位置返回，缺失的 key 是 `nil` 元素。

  实现里处理了两处调用方本不该操心的细节：零个 key 直接返回、**不发往服务端**
  （`MGET` 不带参数在 Redis 里是错误，透传会让一个普通的空页失败）；以及 go-redis
  的 `[]any` 回复里每个缺失 key 的 `nil` **原样保留为 nil 元素**而不是被跳过——跳过
  会让结果变短，而变短的结果和被截断的读无法区分。

  三条契约测试对着真实 Redis 跑（`ROOST_REDIS_TEST_ADDR`），因为"回复恒与 key 等长"、
  "缺失是 nil 而非省略"、"空列表不发往服务端"都是 Redis 与驱动的性质，单测建立不了。
  其中一条专门验二进制保真：驱动把 bulk 回复交回来是 string，按文本转换会破坏二进制
  载荷——而邮件附件正是二进制。

- `versionstore.RetryBackoff`：把带全抖动的指数退避策略导出。不是每个
  compare-and-set 都能用这个包的信封模型——同时维护 sorted set 与 hash 的存储需要
  自己的脚本——但那些循环需要同一套策略。立即重试会让竞争看起来像故障：N 个写者
  一起输、一起重试、同时耗尽预算，调用方得到一个与"后端不可达"无法区分的错误。
  导出它是为了让这套策略只有一份实现，而不是每个 CAS 循环一份。

### Fixed
- **`versionstore` 的并发测试不再断言一个吞吐量声明。** `TestConcurrentUpdatesLoseNothing`
  此前要求 8 个写者在同一个 key 上做 200 次更新时**没有任何一个触到重试上界**。那是
  吞吐量声明，却挂着正确性声明的名字：Update 里的尝试预算是**上界不是保证**，用尽后
  返回的 `ErrConflict` 是一个**被报告的、可区分的结果**，不是丢失的更新——「耗尽即报告」
  这条性质本来就有它自己的测试（`TestUpdateReportsConflictWhenTheBudgetIsExhausted`）。

  这个声明在 `-race` 下不成立：每个 goroutine 都慢到 8 个写者能在一个 key 上连输 8 次。
  它 **8 次里失败 6 次，并且已经带着这个失败发进了 v1.11.0**（非 `-race` 下稳定通过，
  所以全量套件是绿的，只有开 `-race` 才暴露）。

  改成调用方真实的做法：遇到 `ErrConflict` 就重试（有上界，因此一个永远冲突的实现仍会
  让测试失败），然后断言真正的性质——总数与版本号**精确**。丢一次更新或重复计一次都
  过不了，与时序无关。三条变异验证：内存实现把读改写移出锁（total = 59，want 200）、
  版本不自增、Redis 实现把 CAS 换成无条件写，全部变红。

### Changed
- **`versionstore.Store.Create` 的契约写清了冲突时的返回值**：返回的是**零值**，
  不是它撞上的那个值；需要看既有值的调用方必须 `Get`。两个实现本来就一致，缺的是
  文档——而误信它会让"每 owner 独占"退化成完全不独占（零值的 id 是 `""`，看起来像
  一个指向不存在记录的孤儿声明，于是每个竞争者都"解决"掉一个活着的声明并接手）。
  已用测试对两个实现钉住。
- `redis.NewClient`：在 Mod 生命周期之外按配置构造客户端。生产接线仍走 `RedisMod`
  （它负责配置解析、capability 注册与关闭）；这个入口给没有 registry 的场景：需要
  真正执行 Lua、pipeline、WATCH 这类语义的集成测试（内存替身只能重新实现而非执行
  它们），以及一次性运维工具。调用方负责 Close。

### Added
- `scripts/pretag.sh`：打 tag 之前的发布预检（tag major 与 module 路径后缀一致、
  tag 未存在、无 replace、工作区干净、`GOWORK=off` 下 build/vet/test 通过）。
  由 tag push 触发的 CI 运行在 tag 已存在之后，能报告但阻止不了。

### Added（roost-service 的三个前置能力）

- **`versionstore`：版本化状态的契约 + Redis 与内存实现。** 契约里**没有无条件写**：
  `Update` 是改变值的唯一途径、自己完成 CAS 重试，调用方无法表达"无论如何都写"。
  这是刻意的——只要契约同时提供无条件 `Set`，就会有实现满足接口而跳过检查，
  且编译器不会发现（业务仓里两个互不相关的服务各自这样丢过更新）。三条硬规则各自
  对应一个已确认的缺陷：**比较版本不比较值**（比较重新序列化的值会在滚动发布改变
  结构体序列化的那一刻卡住所有写入）；**版本由存储侧分配、按 key 单调**，跨副本与
  跨重启可比（进程内计数器不是版本）；**版本 0 表示"不存在"而非"跳过检查"**，
  已存储的值版本恒 ≥ 1，因此没有代码路径能靠留零值绕过比较。
  `Create` 是独立的仅插入路径，因为"不得覆盖"与"由当前值计算"是不同意图，用
  `Update` 表达它要依赖调用方检查 `found`——而那正是会被忘掉的检查。
  重试带**指数退避 + 全抖动**：立即重试会让竞争看起来像故障（N 个写者一起输、
  一起重试、一起耗尽预算），而把它报成冲突就得到一个与"后端不可达"无法区分的错误——
  正是这个原语要消除、而不是复现的缺陷。
- **`servicerpc`：服务间 RPC 客户端**（从业务仓提升）。总线上的类型化调用、etcd
  实例选择、lightweight/JetStream 传输选择，以及服务应答用的稳定响应状态约定。
  它是基建而非服务，业务仓与 roost-service 不应各持一份。
  新增 `KeyAffinityPicker`：同一 key 的调用固定路由到同一实例。round-robin 对无状态
  读是对的默认值，对"按 key 持锁改共享状态"是错的——它把同一逻辑 key 的连续操作发到
  不同副本，于是进程内互斥等于没有互斥（业务仓的匹配队列因此出现两个副本各自提交
  一场匹配）。亲和本身不是正确性机制（实例会增减），它消除的是让该故障变成常态的
  稳态竞争；共享状态仍需 CAS。映射按 sid 排序后计算，因此不依赖 discovery 的返回顺序。
- **`mongo/mongotest`：求值 filter 的内存 MongoDB**（原 `internal/mongofake`，导出）。
  不求值的测试替身比没有替身更糟：它让所有版本 CAS、租约谓词、唯一索引冲突和 tombstone
  守卫无条件通过，于是这些构造要保障的语义全部不受测试保护。业务仓与 roost-service
  需要与 kit 自身测试同一份替身；手写副本可靠地会出错（用 `fmt.Sprint` 比较、用
  `encoding/json` 而非 bson 编码文档、或让 `Pipeline` 返回 nil 使生产读路径根本不执行）。
  kit 内 8 个使用它的包已迁移，`go test -race` 全绿。

### Changed（破坏性：Go 模块路径改为 roost-kit，协议前缀默认值改为 roost）

- 模块路径 `github.com/tjbdwanghaibo/cube-kit` → `github.com/tjbdwanghaibo/roost-kit`，版本延续。
- NATS 主题前缀默认值：`cube.sync`/`cube.room` → `roost.sync`/`roost.room`；`nats` mod 默认前缀
  `cube` → `roost`；`statslog` 默认服务名 `cube` → `roost`。这些都是可配置默认值。
  **滚动升级**期间新旧节点若都用默认值会互相听不见，请在升级前显式配置同一前缀。
- JetStream 同步流默认名 `CUBE_SYNC` → `ROOST_SYNC`，与 `ROOST_EFFECTS`/`ROOST_SAGA` 对齐。
  流名是 broker 侧持久状态，见 roost-core CHANGELOG 同条说明。
- `syncstream` 指标名 `cube_sync_*` → `roost_sync_*`，仪表盘与告警规则需同步更新。
- `ai` 行为树 JSON schema 标识 `cube.ai/v1` → `roost.ai/v1`，不做兼容；已有行为树定义把 `"schema"` 字段改名即可。
- QUIC ALPN `cube-replication-v1` → `roost-nettransport-v1`；JetStream 去重 MessageID 前缀 `sync:` → `room:`
  （跟随包名）。两者都是握手/去重时双端必须一致的值，**不做兼容**：滚动升级期间未升级的客户端会被
  已升级服务端拒绝握手，新旧节点对同一条消息算出不同 MessageID 会各投递一次。请同批升级两端，
  或在 TLS 配置里显式固定 ALPN。

### Added
- **`manager`：`ManagerMod`**，一个 Service 的内存单例 manager（场景注册表、路由表、缓存这类有 Start/Stop 但没有自己持久状态的逻辑）的生命周期拥有者。cube-core 早就声明了契约——`app.IManager`、`app.ManagerDependencyProvider`、`app.IManagerStopperWithContext` 的注释都写着"managed by ManagerMod"——但实现一直缺失，各业务仓各写一份。行为：
  - 按 `DependsOn` 拓扑序启动、**严格逆序**停止（manager 绝不比它依赖的东西活得久）；
  - 无依赖关系的 manager 之间保持**注册序，且每次进程一致**。这里没有复用 core 的 `container.TopologicalSortCache`：它的队列由 map 播种，独立节点的顺序每进程不同，而启动顺序是可观察行为——顺序漂移会把一个必现的顺序 bug 变成偶发的；它报告环的方式也是打日志返回 nil，而启动门禁必须说出是哪个 manager 成环；
  - 启动失败回滚已启动的，**失败的那个不 Stop**——它没有启动完，Stop 就得处理半构造对象；清理是 `Start` 自己的责任。这条契约写在代码注释里并由测试钉住；
  - **启动中收到 shutdown 会中止启动**，而不是和它赛跑。否则 `Stop` 排空已启动的之后，`Start` 会继续把后面的 manager 启起来，于是 shutdown 报告成功而仍有 manager 在跑；
  - `Start` 之后 `Register` 返回 `ErrManagerRegisterAfterStart`。接受它等于加进一个永不启动也永不停止的 manager，唯一症状是很远处的一个 nil；
  - `Start` 之前没有 `Provide` 直接报错，而不是把 nil registry 发给每个 manager；
  - `Stop` 幂等，逐个 manager 都给停止机会并**汇总全部失败**（首个失败就中断会漏掉其余的）；优先使用 `IManagerStopperWithContext` 并把调用方 ctx 透传进去。
  - 指标：`manager.start.duration{manager}`（histogram）、`manager.started`（gauge）。
  - 20 条测试，含 `-race`；确定性排序、启动中止、失败不回滚失败者、`Register` 拦截四条都做过变异验证（破坏实现 → 断言精确打红）。

### Changed（测试质量）
- `spatial` 与 `ai` 的两条并发测试此前丢弃全部返回值，只靠 race 检测器——一个
  拒绝每次写入的 terrain 或一个丢写的 Blackboard 都能通过。现断言并发写后每格/
  每键保留最后一次写入（期望值由测试自己的写入计划推导，不硬编码），并断言
  `Blackboard.Snapshot()` 返回**拷贝**而非活状态别名。两条都用变异测试验证过。
- `etcd/local_mirror_test.go` 的 11 处裸通道接收改为有界的 `awaitWatchStarted`；
  `dataengine` 与 `remoteentity` 的裸接收改为 `awaitChan(t, ch, what)`。此前
  watcher 不启动会表现为挂 10 分钟后一份堆栈，现在是 5 秒内一句
  "mirror never started its etcd watch"。

### Changed（破坏性：包与标识符重命名，无行为变化）

跟随 cube-core 的命名整理，把只描述"机制"的名字换成描述"职责"的名字：

| 旧 | 新 | 说明 |
| --- | --- | --- |
| 包 `sync` | 包 `room` | 它是房间状态同步的房间侧，不是同步原语 |
| 包 `replication` | 包 `nettransport` | 它是 KCP/QUIC/UDP 网络传输，不是复制 |
| 包 `remote_entity` | 包 `remoteentity` | Go 包名不用下划线 |
| 包 `taskflow` | 包 `actionflow` | 与 core 对齐 |
| `SyncMod` | `RoomMod` | |
| `RoomReplication`（及 `New*`/`*Config`/`*Stats`/`Default*Interval`/`Err*Stopped`） | `RoomBroadcaster`（`DefaultRoomBroadcastInterval`、`ErrRoomBroadcasterStopped`） | 它每 50ms 把脏 subject 聚合成帧广播给订阅者 |
| `mods.ModSync` = `"sync"` | `mods.ModRoom` = `"room"` | |
| `mods.ModObs` = `"obs"` | `mods.ModMetrics` = `"metrics"` | 随 core |

文件重命名：`room/jetstream_sync.go` → `jetstream_syncbus.go`、`room/nats_sync.go` →
`nats_syncbus.go`、`room/room_replication.go` → `room_broadcast.go`、
`nettransport/async_transport.go` → `channel.go`、`nettransport/transport.go` →
`sender.go`、`nettransport/control.go` → `control_plane.go`、`syncstream/adapter.go` →
`publisher.go`、`ai/wire.go` → `tree_parser.go`。

**配置段兼容**：room mod 优先读 `room.*` 配置段，读不到才回退到旧的 `sync.*` 并打印
一条弃用告警，因此既有部署配置无需在升级同一时刻修改。JetStream 去重 MessageID 前缀随包名改为 `room:`（见下）。


### Fixed（独立复审 F1/F2/F3/F4/F6/F8，均带"无修复即红"验证过的回归测试）
- **`dataengine` outbox backlog 探针不再随积压等比变慢**：补 `created_at` 索引（原先 oldest 查询按未索引字段排序，探针会随它所要度量的积压一起变慢），并把探针从"每次 `RunOnce` 末尾无条件执行"（默认 2 workers × 100ms = 每秒 20 次全量计数）改为按 `dataengine.outbox.backlog_interval`（默认 1s，跨 worker 用 CAS 抢样）限流；新增 `OutboxWorker.RefreshBacklog` 供健康检查即时取样，`Mod.checkHealth` 已改用它，避免读到最多落后一个间隔的陈旧 gauge。
- **`nats`：删除 `nats.rpc.duplicate_completion` 指标与那条永不失败的断言**。实施中发现它在原理上无法有意义——`worker.Worker.safeHandle` 是 `handler(task)` **加** `defer task.OnRelease()`，所以每个任务必然两次到达 `complete()`，"二次到达"是普通路径而非异常（改成计数器后，10 万 pending 取消测试立刻报出 10 万次"重复完成"）。改为把 `sync.Once` 换成到达计数（同一保证、更直白），并用直接测试覆盖真正的属性：handler→release、仅 release（拒绝准入的路径）、16 goroutine 并发到达、nil 回调。pool 的"handler + OnRelease"所有权协议已写入注释。
- **CI 新增 `go vet -tags integration ./...`**：4 个 `//go:build integration` 文件（约 1000 行崩溃/故障切换测试）被 build tag 同时排除在 `vet`/`build`/`test ./...` 之外，可以静默腐烂而无人察觉。真正运行仍需 Mongo/NATS，入口是既有的 `scripts/integration/dataengine-env.sh`。
- **lease fence 改用 core 的共享谓词，并为未命中加上可见计数**：`leaseFencesMatch` 不再手拼 filter，改用 `coredata.LeaseFence.Predicate(now)`；`saga` 的 `claimStatusPending` 取自 `coredata.LeaseFenceStatusPending`。原先同一份 claim schema 被 `dataengine` 与 `saga` 两侧各自拼写、无任何编译期耦合，任一侧改名都会让**每个被 fence 的事务静默变成 no-op**（谓词不可满足与"租约确实过期"从内部无法区分，两者都被当作 skipped 正常提交）。新增跨包耦合测试：在 `saga` 包内用 `Reserve`/`Bind` 真实写出 claim 与 fence，再拿生产谓词查生产文档，并逐字段偏移验证谓词确实在拒绝——已验证把 `bson:"lease_token"` 改名即变红。fence 未命中现在计 `dataengine.fence.skipped.total{resource}`。
- **`dataengine` 投影批路径不再把良性重放判成致命冲突**（原缺陷会导致服务永久起不来）：单变更记录走快路径时**有意不写** transaction marker（省一次往返正是快路径的意义），但一旦 WAL ACK 丢失（Mongo 提交与 checkpoint fsync 之间崩溃，或任何 `Ack` 报错），该记录会在多记录批中重放——此时"无 marker + 精确版本 CAS 打空"与真实冲突无法区分，而 `ProjectBatch` 原先直接返回致命的 `ErrProjectionConflict`，令 projector 停摆；由于同一批每次重启都会重放，服务从此无法启动。现在批路径改为返回新的 `ErrProjectionBatchNeedsPerRecord`**延后判定**，由 `Projector.replayPass` 回退到逐条投影——单记录路径比对存储版本与 `_last_tx`，能区分"已应用的幂等重放"与"真实冲突"。真实冲突仍然致命（语义不变，只是改由有能力判定的一方来判）。
- **`dataengine` 实体聚合加载在事务重试下不再误报"数据损坏"**：`readAggregate` 的 `loaded` 累加器声明在 `ReadConsistent` 回调**之外**（`missing`/`tombstones` 却正确地在回调内重置）。`ISession.WithTransaction` 的契约明示会自动重试回调（副本集切主、snapshot 不可用、网络抖动），第二次进入时重名守卫立刻命中上一轮条目，健康数据被报成 `ErrEntityAggregateCorrupt: duplicate DAO resource`——恰好发生在最需要平稳降级的故障切换时刻。现已将 `loaded`/`remoteVector` 的重置移入回调首行。

### Changed
- 测试基建重构：新增 `internal/mongofake`——**真正求值** filter/update/唯一索引/事务回滚的内存 Mongo，取代此前 5 份各自手写的 `ICollection` 桩（约 80 个方法）。旧桩把存储建模成"返回测试预置的东西"，无法分辨正确查询与错误查询，因此投影版本 CAS、saga step 租约 CAS、command 收据去重、remote_entity 版本 CAS、effect inbox 幂等这些**靠查询条件承载正确性**的机制全部不在测试覆盖内（F1 正是其中从未被走到的一条路径）。迁移过程即刻暴露两处真实差异（`uint8` 字段的数值加宽、bson 日期的毫秒精度）。不支持的构造一律显式报错而非静默匹配。

### Added — Data Engine
- 新增统一 Data Engine runtime：WAL recovery barrier、Mongo Put/Patch/Delete 版本 CAS、transaction receipt/effect outbox、聚合 snapshot load、system-transaction migration、tombstone、健康/积压硬限制和有界 shutdown。
- Saga 新增 Data Engine step inbox（claim lease + 权威 receipt replay），Remote mutation 与普通 mutation 可在同一 Mongo transaction 投影；NATS publisher 与 WAL ACK 解耦。
- 新增 WAL/投影/Saga benchmark 矩阵及 NATS outage backlog 恢复测试，脚本位于 `scripts/perf/dataengine.sh`。

### Changed — Data Engine
- `persistence.engine` 只接受 `dataengine`；Nest 通过 Data Engine lazy proxy 获取 committer，recovery 完成后才接流量。
- WAL reader 同时支持 v1/v2；新配置默认写 v2，只有显式 `dataengine.wal.writer_version=1` 才为历史 reader 保留兼容写入。
- 旧 Checkpoint Mod 与 standalone NestWAL Mod 已物理删除；`nestwal` 仅作为 Data Engine 内部 WAL 库存在，不再参与业务 Mod 装配。

### Fixed
- Data Engine Runtime 现在为 EntityManager 注册唯一 delete admitter。本地删除用隔离 strict transaction 或当前 Nest transaction 写 tombstone；Remote 删除先准备完整 RemoteWriteBatch 并携带显式 delete intent，只有 durable admission 后才从内存移除，rollback 保持实体可用，结果不确定时 fail-stop。
- Saga Data Engine step 的 reservation 必须显式传给 `inbox.Bind(command, reservation)`。Mongo 投影在应用任何 mutation/effect 前原子校验 claim owner、lease token、command digest、`pending` 状态与未过期租约；陈旧记录只写 skipped transaction marker 后 ACK，防止旧 worker 晚到提交，也避免 poison WAL。skipped 状态在重放时同样阻断事务外 Remote publish。
- Data Engine 默认 WAL writer 修正为支持 Patch/Receipt 的 v2；聚合加载先绑定 DAO ID，并把“部分缺失/部分 tombstone”判为损坏而不是整个实体不存在；绝对 `expires_at` 索引不再重复叠加一轮 TTL。
- Data Engine：修复部分重放时的 segment ACK 正确性；重放按 WAL 顺序切分，失败 segment
  不会被后续 ACK 跨越。Mongo marker 与 WAL 存储格式保持不变。
- NATS async RPC 增加 started/completed/pending、callback latency、duplicate completion 和 callback queue rejected 指标，并加入 10 万 pending 取消守恒验收，确保 exactly-once completion 不仅被实现，也能被监控和规模验证。
- NATS async RPC 将 reply、timeout、publish error 和 Stop 收敛到唯一 `LoadAndDelete` 终态入口；callback worker 队列关闭/满载时同步兜底，并以 once 防止正常执行与释放路径重复回调，实现 exactly-once completion。
- NATS 同步 RPC 使用 `RequestWithContext`，调用方取消可立即中断正在等待的请求；默认重试策略随 core 收敛为单次发送，避免未知幂等性的业务调用被框架静默重复执行。
- Mongo、Redis、ConfigData、Lock 四个内置 Mod 补齐 `StopWithContext`，统一接受 App 的 shutdown budget；关闭错误不再被 Redis/Mongo 的无返回 Stop 路径静默吞掉。
- 多 capability Mod 在注册前统一预检名称、重复项和现有冲突，避免 Provide 失败留下半装配 registry；Etcd 服务注册复用有界启动 context，不再可能无限阻塞 Start。

### Changed
- NATS Mod 通过 core `OptionalDependsOn` 声明 Redis 可选排序依赖，Nest Mod 同样声明 Remote Entity；reliable bus/远程事务集成不再依赖业务传入 Mods 的偶然顺序。
- README 接入 Roost 三级文档导航，并修正停机说明：App 会把统一 shutdown deadline 传给 Saga/Nest/WAL 的 `StopWithContext`。

### Added
- **`robot/` 机器人框架的 kit 侧**（配合 cube-core 新增的 `robot` 框架）：
  - `RegisterKCPDialer`/`RegisterQUICDialer` 经 core `transport.RegisterDialer` 挂载 KCP（AES-GCM + FEC，复用 `replication.DialKCP`，KCP 流上直接跑统一包协议）与 QUIC（`replication.DialQUIC` + 单双向 stream 承载，对端关流归一化为 EOF）客户端拨号——runner 配置里 `Transport.Type` 选中即用。
  - `LockstepBot`：lockstep 客户端半场——基于 core `lockstep.FrameAssembler`（去重 + 严格顺序）应用帧、按帧生成并提交输入、关键帧哈希上报（缺省 `FrameHasher` 输入链 FNV 折叠：确定性模拟下输入同则状态同）、缓冲越界时每 gap 恰好一次追帧请求；出站走业务注入的 `LockstepSink`（提交/上报/追帧的线格式由业务定义）。回归基线：3 bot × 600 帧 × 各自独立 30% 丢包，冗余 + 追帧后全帧应用、`DesyncDetector` 全程零误报（16 次追帧、30 次裁决）。

### Changed
- 移除根模块的本地 `replace`。当前未发布的 robot 功能依赖 core HEAD 新增 API，因此开发主线固定到可由 Go proxy 解析的精确 pseudo-version `cube-core v1.8.1-0.20260826111010-16f057d5e22f`；正式 tag 必须先切回已发布的 core tag，release-hygiene 会拒绝 pseudo-version。

## [1.8.0] - 2026-08

### Added
- `redis`：客户端实现 `fredis.ListTrimmer`/`ListRemover`（LTRIM/LREM）——core `failurelog` 的 trim/delete 回退由此走就地操作，消除 DEL+RPUSH 两步间崩溃丢整表的窗口。（go.mod 已升级到 cube-core v1.7.1，临时 replace 已删除。）

### Fixed（lockstep v1.7.1 发布后复审，9 缺陷 + 6 权衡全部实施，均带回归测试）
- `lockstep`：**构造期传输预算校验**——`NewRoom` 拒绝"冗余深度 × 座位数 × 单输入上限 + 线格式开销 > MaxDatagramBytes（默认 1232）"的配置（原先单个合法客户端满载 payload 可让整房间每个广播包超过 UDP 上限、全部发送失败且冗余无从修复）；`RedundancyDepth` 按解码端 `MaxBroadcastFrames` 收口（原先配 100 会让所有客户端整包拒收而服务端零告警）。
- `lockstep`：**追帧与会话绑定加固**——session 独占（被其他座位/观战者占用的 session 拒绝 Attach，消除双发与追帧互踩）；同 session 重复 Attach 幂等且保留追帧游标（原先防御性重复 Attach 会静默取消追帧留下永久帧洞）；`StartCatchup` 拒绝超过下一帧的起点（原先白丢一帧实时广播）、追帧中重复调用取 min 游标；可靠通道连续失败超预算（`CatchupMaxFailures`，默认 8）自动放弃并显式报错（原先永久钉死在"不收实时也追不上"态）；历史被 Trim 出缺口时以 `ErrCatchupUnservable` 放弃。
- `lockstep`：**裁决语义重做**——quorum 改为"同意组大小"（默认由座位数派生过半），串谋少数抢先上报不再能对诚实玩家触发 `OnDesync`；`OnDesync` 按离群**集合**变化触发（原先只比基数，等基数翻转被吞）；`ReportHash` 校验座位与帧号上界并返回错误（原先可用伪造座位/未来帧制造 Trim 清不掉的无界内存）；Trim 过的帧墓碑化。
- `lockstep`：迟到折叠不再遮蔽真实输入（显式当前帧输入覆盖折入的过期 payload）；新增 `lockstep.input.rejected.total{reason}`；广播/追帧迭代确定性排序；新增观战者通道（`AttachSpectator`/`SpectatorCatchup`）与 `Room.Close()`；**文档修正：datagram 通道必须直连裸 transport，不得经 `AsyncTransport` 的 latest-only 合帧队列**。

### Fixed
- `configdata` Mod：Rollback 回调把 `configdata.version` gauge 复位到 `Old.Version`——此前 apply 后失败的 reload 会让 gauge 永久停在一个已被回滚掉的版本号上（监控盘误报 reload 成功）。

### Changed
- go.mod：`cube-core` 升至 v1.8.0（lockstep 复审修复 + configdata 自查修复 + ListTrimmer/ListRemover）。
- README 按全量能力审计重写扩充：组件总览逐包补齐（taskflow 独立成行、sync/syncstream 拆分、replication 补 AsyncTransport/三 transport 矩阵/CompositeTransport、remote_entity 补 Mod 装配与所有权状态机）；新增 §3.2 配置命名空间与跨键不变量、§3.3 capability 常量→注册者→实际类型对照表、§3.4 停机语义表；§4 新增 replication/sync 四角色/syncstream/remote_entity/saga（`SaveWithOutbox` 描述修正为 `Apply`）/基础设施细节/并发定位一览各节；修正装配顺序描述（仅 6 个 Mod 声明 DependsOn，nats(reliable)→redis 与 nest→remote_entity 是未声明的顺序依赖）与版本号引用。

## [1.7.1] - 2026-08

### Fixed（v1.7.0 发布后独立复审的 8 项发现，均带回归测试）
- `spatial`：MaxVisible 从每房间独立执行收口为 **cluster 级全局 top-N**（缝边 observer 的预算曾被相交房间数放大）；`AddRoom` 回填既有 observer 的边界镜像（静止 observer 对后加房间曾有永久可见性盲区）；拒绝 id 0（曾半跟踪导致可见性对不同 observer 分裂）；`LeaveRadius` 加上界 + 订阅盒算术全饱和（极值曾回绕致 observer 全盲）；observer 全量评估改最近优先 admission（曾产生瞬态 Enter+Leave 对）；补 gofmt。
- `ai`：guard 谓词在装配期限定为无状态形态（condition/sequence/selector/inverter，action 作谓词曾每次检查都发起动作）；nil-root 策略丢弃动作完成事件（曾无界堆积）；`Cooldown.Reset` 保留计时的语义与 `ParseTree` 结果单策略专属（禁共享）写入文档。

### Added（v1.8.0）
- `lockstep` 包：帧同步（输入帧）房间层，依赖 cube-core v1.7.0 的 `lockstep` 包（go.mod 已升级到正式版本）。`Room` 把 Sequencer/帧历史/冗余编码/裁决器绑定到传输：`Tick` 切帧并经 datagram 通道冗余广播（丢包靠后继报文冗余修复，不重传）；`StartCatchup` 重连追帧经可靠通道（KCP/QUIC 皆可，`ReliableSender` 接口互换）按 tick 限速分页，追上后自动切回实时；`ReportHash` 关键帧哈希多数派裁决，`OnDesync` 仅在裁决出现或离群集扩大时回调；掉线座位保留（缺席即空输入），重连 = `Attach` 换 session + 追帧。指标：`lockstep.frame.total`、`input.late.total`、`catchup.frames.total`、`desync.total`（非零即事故），见 cube-core OBSERVABILITY.md。

### Added（v1.7.1）
- `nestwal`：durability 管线指标——`nestwal.batch.total`/`append.total`（合批放大率）、`bytes.total`、`fsync.duration`、`pending.tickets`、`disk.bytes`、`reject.total{reason}`。面板与告警基线见 cube-core 的 OBSERVABILITY.md。

## [1.7.0] - 2026-08

### Added
- `spatial`：增量兴趣管理。`InterestManager`（九宫格订阅、双半径滞回、距离带 LOD、MaxVisible 风暴闸门、确定性 Flush）与 `InterestCluster`（共享坐标平面多房间无缝：边界 observer 镜像、净变化 Flush、跨界迁移 make-before-break 零闪断；单锁并发安全）。`BlockIndex` 新增 `QueryBlockIndex`。跨进程 handover 不在此层（见包注释）。
- `ai`：行为树二期。节点库（Parallel/Repeat/UntilSuccess/Succeeder/Condition/Guard/Cooldown/TimeLimit/RandomSelector，计时读注入 tick 时钟、随机用注入掷点）；`BehaviorStrategy`（树 → cube-core Strategy 桥，动作完成事件缓冲进下一 tick）；`TaskflowAction` 叶子（发起并等待 taskflow 动作，含打断收尾钩子）；`ParseTree`/`Registry`（严格 JSON 树装配，fail-fast + JSON path 诊断，schema `cube.ai/v1`）。

## [1.6.3] - 2026-08

### Fixed
- `redis/AutoExtendLock`：单次瞬时 Extend 错误不再永久停摆（TTL 预算内重试）；每次续期带独立超时，Redis 挂死不再拖死续期协程。
- `checkpoint`：`Stop` 在 WAL fence 场景下增加 30s 上界，不再无限悬挂。
- `nestwal`：`Stats()/Healthy()` 缓存最早未 ack 时间戳（5s TTL，Ack 时失效），周期健康检查不再全量 CRC 重扫段文件。

### Added
- CI 增加 `release-hygiene` 门禁（module 路径可解析 + tag 与 major 匹配）。

### Changed
- 锁双轨契约边界正式化：`redis.IDistLock` 包注释与 README 写明"无栅栏、仅限可容忍双执行的场景"，正确性互斥指向 `remoteentity` versionedLock / `etcd.IFencedElection`（含二选一判据表）。
- `spatial`/`ai`/`gateway` 各自的包注释补 non-goals 定位声明（无 Z 轴/navmesh/兴趣管理；行为树骨架而非 AI 中间件；中间件集合而非网关服务器）。

## [1.6.1] - 2026-08

- 对接 cube-core v1.6.x `DurabilityPipelined`：nestwal Enqueue ticket、durable watermark 先于唤醒发布、从 ack fence 起扫描重放、`NestOptions` 配置接线（`nest.pipelined.allowlist/async`）与 checkpoint 外化闸门（`SetDurableWatermark`）。
- `remoteentity` versionedLock 幂等 unlock；`etcd` 选主暴露 fence（`IFencedElection`）；`spatial` 寻路预算错误与半径查询饱和防护。
