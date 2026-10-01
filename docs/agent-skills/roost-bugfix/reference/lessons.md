# roost-bugfix 反复用到的事实

踩坑记录，按包 / 主题分组，逐条保留。路径按单仓写法：kit 的包在 `kit/` 下、生成器在 `codegen/` 下、同步块在 `sync/` 下（M-17）；
核心 service 实现在根目录 `service/`（session / mail / match），`kit/service/<name>` 是 Mod 装配层。harness 名称以当前源码为准，
标"（当前源码未找到，以源码为准）"的名字 2026-09-30 用 `rg` 没搜到。

## core nest / nestwal

- **"已关闭"是两个事件**：nestwal 的 `closed`（Close 发起、拒新写）和 `doneCh`（writer 排空并 sync 完）。U-0185 混用被 RR-20260914-01 打回。
  任何"关闭后可以直接返回"的推理先问：靠的是哪一个？要钉住 writer 时别用它路径上的锁（旧代码自己也要拿，红会变成"阻塞"），
  在 `Options` 加未导出的 `beforeProcessBatch` 缝（`nestwal/wal.go`，写 nestwal 测试可直接用）。
- **要改状态机 / 要可取消等待的四条也收得了**，手法可复用：可取消的互斥 = 一格 channel 信号量 + `select` 上 ctx（`acquireSlot`，`nestwal/committer.go`）；
  队列的"之前的都写完了" = 往同一条 FIFO 放一个 barrier 请求，收批即答、答后再做后续动作（比 LSN 水位轮询干净）；
  "外部 CAS 结果未知" = 失效本地缓存 + 用独立有界 ctx 重查权威、让权威三选一（未变 / 已生效 / 别人），查不到就进一个现有的冻结态
  （`Recovering`）并让所有出口在下一次权威读成功后解冻——先查状态表里有没有现成的态，别新加。
- **门槛要跟着"被判定的东西"一起捕获**：entitysync 的水位门槛原来在一个入口上查可变的 `LastCommitLSN` 再放锁 Prepare，既漏入口又有检查-捕获窗口。
  正解是让捕获出来的内容自带判据（`SubjectSyncUpdate.CommitLSN`，在同一把实体锁内读——先确认写入方也在这把锁内盖章，
  `nest/rollback.go` "stamp entity LSNs in-lock"），然后所有准入点只看内容自带的那个数。"推迟"要有可辨认的可重试错误，
  并且不改变任何已有状态（原订阅保留、批次 abort 保持 dirty）。harness：`SetDurableWatermark(func)` 现在在 `nestwal/runtime.go`；
  `newSubscriptionTestState(t, id, &packs)`、`recordingEnvelopeSink`（当前源码未找到，以源码为准）。

## core cache / remoteentity / entity

- core `cache`：`ReadThroughStore` 的 `waiters` 是"正在等"，取消要减回并核对 `s.calls[key] == call`。
- **"所有写入口共用一条规则"要落在最底层的锁下**：U-0187 第一版只在 Publish 查墓碑，ReadThrough 回填这条 L1 写入口就漏了；
  和 U-0181 的 `Conflict` 一样，做成 `StoreConfig` 钩子在 `AtomicLocalStore` 分片锁下判（`Superseded`，键不存在也判）。
  凡是"L1 上的准入"，先数清 L1 有几条写入口：publish、loader fill、L2 回填。
- **共享层（L2）的比较只能在共享层做**：本机 L1 为空说明不了 L2 有什么；删除、CAS 都要是 L2 侧的一个脚本。
- **预检查不是并发屏障**：Get-then-Set 之间的窗口只能靠真正的原子操作（L2 CAS）的返回值处理，所以"哪些错误不可降级"必须有分类钩子
  （`FatalRemoteError`，`cache/read_through.go` / `entity/remote_snapshot.go`），而不是再加一次查询。
- **比被测实现更"正确"的测试替身会把缺陷藏起来**：`snapshotRedisFake` 用 Go 的 `ParseUint` 精确比较，而脚本用 Lua 的 `tonumber`（float64）；
  `markerEvalStub` 用 Go 精确格式化，而 Lua 用 `%.14g`。两个跨语言缺陷因此在单测里长期不可见（替身都在 `remoteentity/`）。
  碰到"脚本 / 外部运行时"的逻辑，先问替身是否忠实于它，必要时**先把替身改忠实让测试变红**，再改实现，最后让替身镜像新实现。

## core dataengine/engine

- `Assembly.Shutdown` 只有 `Runtime.Shutdown` 返回 nil 才释放引用；`Runtime.Shutdown` 记住 projector / outbox 是否已停，可重入。
  测试替身：`projectorOutboxFake`、`failingOutboxPublisher`（`outbox_lifecycle_test.go` 等）。

## core sync（entitysync / syncstream / syncbus / lockstep / frame）

- **"上限"先问约束的是结果还是过程**：一条有序合并出来的 delta，过程中的暂存集合可以合法地超过最终存量上限（先建后删）。
  三个阶段（构造校验 / 编解码 / 应用）要用同一个不变量；应用时对过程用一个能从"最终合法"推出来的更宽上界（base + creates ≤ 2×Max），
  对最终集合用真正的上限。别去改发送侧的操作顺序——老发送方的帧还在路上。
- **"靠幂等 / 对端报错收敛"这种话写进记录前先找反例**：U-0200 我写"并发准备的分叉靠 delta 幂等 / resync 收敛"，
  审查一个"下一 tick 投影回到第一份视图 → 零变化 delta"就推翻了。凡是"两份不同状态、之后自然一致"的论断，要枚举"之后什么都不变"的情形。
  修复也要往前提：U-0200 在提交时冻结不够，窗口在"先发后 Commit"的运输契约里，所以钉视图要在第一次投影时做（该路径已被 sendMu 串行）。
  记录被推翻的部分要在原记录末尾写"更正"，不要删。（涉及的 statesync 包已删除，见末尾"历史"。）
- **不可撤销的副作用，其通知不能绑在"本次调用是否成功"上**：room sink 在一次准入里先做了剔除（已 RemoveSession），剩余部分再失败就把剔除通知随 error 丢掉。
  凡是"先做了改不回来的事、再做可能失败的事"的函数，返回值要把已发生的副作用与失败结果**同时**带回（`return events, err`），
  调用方在两条出口都消费副作用。（room 广播器已退场，harness 见末尾"历史"。）
- **"Set 某个协作者"要和"New 时装配这个协作者"做同样的事**：room 构造时向 sink 注册回调，`SetDownstream` 只换指针就漏了。
  写替换类 API 先列出构造函数对该协作者做的每一步（注册 / 订阅 / 持有 unregister），替换时按"新的先建立 → 切换 → 旧的再拆"顺序做完，
  失败保留旧的，同实例短路。比较接口值前先看动态类型可比较（`reflect.TypeOf(x).Comparable()`），func 类型的实现 `==` 会 panic。
- core `syncstream`：`History{epoch, streams, journal, sequenceFloor}`，Append / ACK / Delete 都"先 journal.Record 再改内存"；
  `FileHistoryJournal` 是 checkpoint+WAL 代数，`Load` → `replayWAL`；harness `NewFileHistoryJournal(t.TempDir(), epoch)` + `NewHistoryWithJournal`，
  重开用不同 initialEpoch 也能加载（以文件为准）。注入"真做完再报错"要给 journal 加 `syncFile` / `publish` 字段缝。
  识别"不确定结果"：从第一个字节写下去之后的任何错误；正解是 fail-stop 而不是回滚。
- `sync/syncbus/driver` JetStream（原 core `room`）：`fakeJetStream` 按 FilterSubject 存 handler，`deliver(subject, data)` 投递；
  `ISyncBus.Subscribe` 是广播语义，持久消费者是工作队列——适配层必须自己扇出。改 durable 名前先算清已部署游标的迁移代价，
  能做到"默认配置逐字不变"就别一刀切改名。
- **改了"编号 / 身份如何生成"就必须实跑全仓，别靠读代码断言下游不受影响**：U-0215 的序号地板我读完 `skillsync` 就在记录里写了"核对过，不受影响"，
  全仓 `-race` 一跑立刻红。而且红出来的是我自己实现的真错误（新建流的首包 base 指向了一条不存在的包——链起点的 base 必须是 0）。
  判据要选语义的那个："链起点"是 `BaseSequence == 0`，不是"绝对序号 == 1"。记录写错了就在原处更正，并把连带改动逐条列出。
- **契约被 RR 推翻时，编码旧契约的老测试要改成断言新不变量而不是删掉**（`TestObserverStreamsAreIsolated`：从"首序号都是 1"改成"每流连续、彼此独立推进"），
  并在记录 / CHANGELOG 标"行为变化"、列出核对过的消费方。
- `sync/lockstep`：单 goroutine 驱动、无锁；harness `newRecordingTransport()` / `newTestRoom(t, tr, nil)`（`room_test.go`），`Room` 内部字段（`catchups`、`history`）测试可直读。
  "去重"要问身份活多久：pending map 随切帧消失，跨帧幂等要单独的有界身份表（`accepted`，按 ReplayHorizon 修剪），而且输给同帧已有输入的也要记。
  配置校验要对齐协议常量（`MaxFrameInputs`）与速率关系（补帧 batch-1 > 0），不只字节预算。

## core manager / service

- **core `manager`**（M-09 自 kit 迁入）：`Engine{managers, started, starting, stopping}`；Start 取快照即置 `starting`（Register 关闭），
  每个 Start 成功后在 `e.mu` 下交接——stopping 已置则 Start 路径 `stopOne` 并报 aborted。测试用 `gatedManager{entered, release}` 钉住 Start，
  轮询 `stopRequested()` 同步"Stop 已生效"。"回收依赖下一轮循环"是这类引擎的典型缺口（RR-20260916-06）。
- **`service/match`**：`Queue.Key` 对 Mode / Partition 转义 `%`、`:`（纯键不变）；`scoreDistance` / `window` 用 uint64 饱和算术（`grouping.go`）；
  `Subject/Ticket/Match.clone` 在 Store 边界深复制。写 `Ticket()` 测试要传归属主体（否则 `ErrNotPermitted`）。
- **`service/session`**（核心实现；`kit/service/session` 是 Mod 装配层）：Enter = Requests.Get → Runs.Create → Claims.Create → Requests.Update（CAS 仲裁 RequestID）；
  撞键撤回顺序必须先 `releaseClaim`（带 run-id 校验）再 `discard` run。`versionstore` 的版本只在一次键生命周期内单调，删后重建回到 1——
  任何"按版本删"的清理都要有顺序或身份论证。harness：`newHarness(t, mutate…)`、`enterReq`、`barrierLedger`（让两次初始读都完成再写）。
- **`service/mail` 测试**（`kit/service/mail` 另有 Cluster 集成用例）：`Send` 按 `RequestID` 去重，churn 要给每次不同的 id；`Send` 会立刻 fanout，
  邮箱满时先 `Delete` 一条未读腾位；未读邮件不可淘汰，所以已领取的那条是唯一可淘汰项、会先走。新增错误码要同步 `errcode_test.go` 的 `segmentAllocated`。
- **Store.Update 回调的所有权契约**：回调拿到的是结构体浅拷贝，里面的 map 与存储共享；先改后拒会把存储改坏。
  凡是 `Mailbox` 这类含 map 的值，回调第一行 `current = current.clone()`（match 一直如此，mail 漏过一次）。加字段时同步检查 `clone` 是否深拷贝了新 map。
- `kit/service/global/activity`（包在 `kit/service/global/` 下，不是 `kit/service/activity`）：`Server` 是生成代码（`coordinator_rpc_gen.go`），不能加字段，
  跨 sweep 的状态放 `Service`；窗口记录 `Window{Keys, Opening, Delivering}` 一个 key 一个 CAS，跨 key 生命周期靠"条目分段 + 回收只删观察到的那一段"；
  harness `newActivityService(t)` 返回可推进的 `activityClock`，`Server{service: s}` 直接调 `sweepGroup`（`server_run.go`）。
- **service 的 Redis 变体挂在五个变量上**（RR-20261001-01）：本地跑要同时导出 `REDIS_ADDR`、`ROOST_REDIS_TEST_ADDR`、`ROOST_REVIEW_REDIS`（同一地址）、`ROOST_REVIEW3_BACKEND=redis`、`ROOST_REVIEW4_BACKEND=redis`，只设 `REDIS_ADDR` 时 account / mail / platform / chat / directory / rank / activity / session 的 Redis 变体静默 SKIP 或落回 Memory；根包 `TestCIRedisJobSetsEveryRedisGateVariable` 钉住 ci.yml。
- kit 的 Redis 用例需 `-tags integration`；新加 Redis 存储 / 索引要把键空间登记进 `kit/service/integration` 的 `everyNamespace`（U-0263，`redis_test.go`）。

## codegen

- `codegen/internal/entity`：每实体 `register<Name>Entity` + once；包级 `RegisterEntity`（带 `//roost:register phase=entity`）只在按名排序的第一个实体文件；
  `generate` 是单实体包装，多实体走 `generateInPackage`（`gen.go`）。改模板后
  `GOWORK=off go run ./codegen/cmd/entity -dir codegen/internal/entity/testdata -output codegen/internal/entity/testdata/player_gen_wire.go -force`。
- `codegen/internal/roost` consolidate：字节偏移拼 import，单行 import 要另起括号声明；包测试约 20–40 秒。
- **自己上一轮的修复最容易被下一轮打回**：U-0165 的墓碑连出两条 RR（计数上限保证不了时间期限、clone 漏拷新 map），
  M-05 连出两条（import 收集漏 category、别名保留集漏 entity）。加字段就检查 clone / 序列化 / import 收集这类"逐项列举"的地方；
  用条数给"必须活到某个时刻"的东西收界一定错。

## 方法手法（跨包）

- **卡在"造不出那个状态"时的两个手法**：**select 屏障**——select 阻塞前会求值所有 channel 操作数，所以传一个 `Done()` 会阻塞的 context
  就能把 goroutine 钉在 select 入口（`remoteentity` 的 `selectEntryBarrier`）；**降到内部入口**——端到端路径上的真实校验（如 skill 的宿主状态校验）
  会拒绝手搓状态，此时把逻辑抽成纯函数、对纯函数做表驱动，加上两端各自的断言，比硬造端到端状态更可靠，但要在记录的边界里写明没做端到端。
- **加"记住一批东西"的结构，上界要在写入口成立**：U-0193 加 `accepted` 只在 Advance 按期限清理，下一轮就被 RR-20260914-08 打回
  （两次 tick 之间灌旧 id 无界增长 + 清理遍历还是热路径开销）。定长环 + 取模定位是标准解：先证明"任一时刻的合法 key 区间长度 <= 环长"（于是不撞槽），
  再让区间外的 key **既不查也不记**（不记那一半同样重要——否则客户端提交区间外的 key 就能挤掉有效条目）。换成环之后清理遍历可以整段删掉，
  顺带是性能修复；同机 before/after 基准比跨机数字有意义。
- **"已收到"≠"已应用"**：消费者拿到一个批次就整体前移去重游标（nestwal 的 Sync、robot 的 Assembler），批次处理到一半失败就把尾部弄丢了，
  而且重传会被去重挡住。修法不是把去重游标退回去（会重放已成功的副作用），而是保留未完成的那部分 + 记住停在哪一步；**按失败点分恢复规则**：
  有副作用且不可判断进度的（Simulate）转 terminal 并在每个入口拒绝，纯出站的（send / report）保留重试并缓存已产出的输出，不重新调用生产者。
  保留队列同样要在写入口收界（见上条），跨调用保留调用方传进来的东西要先复制。
- **按客户端给的数寻址的结构，寻址必须排在校验之后**：U-0193 把"身份优先"写成了字面第一步，于是 `int(uint32)` 的环下标在 32 位平台上是负数（可 panic），
  垃圾帧号还会先分配环。挪动前先证明等价（被记住的 key 恒满足校验条件，且判据只会变宽）；取模留在原类型域里做，不要先转 `int`。
- **"需先定契约"不等于"不能做"**：RR-20260913-02/05/06 上一轮标为需定契约，这一轮契约定清就收敛了（只增代际 / 同版本同值是所有写入口共用的一条规则）。
  真正卡住的是那种要改状态机或加可取消等待的（所有权不确定态、nestwal 可取消锁等待、WAL Sync 屏障）——手法见 nestwal 一节。
- **红测试的"红"要落在承诺本身上**：U-0188 第一版红在返回值形状上，真正的缺陷是"旧 owner 被放行"，把准入断言挪到最前面再看一次红文本。
  给新 API 写的测试（如 `DeleteAtVersion`）修前只是编译不过，真正的红要用只走旧 API 的那条（`ApplyReplica`），用 `git stash push -- <impl files>` 验证。
- **新 API 的红**：把用到新 API 的用例暂时截掉（备份到 scratchpad，python 按注释锚点切），跑旧 API 用例拿红文本，再还原。
- **自己记录里的"未做"就是下一轮的 RR**：U-0188 写了"Enter/LeaveShared 同形状留后续"，审查第九轮就登记成 RR-20260913-12。
  同形状的入口能一起改就一起改，或者至少把骨架参数化好（applied 判据、目标状态、排除 SID），下一轮只填三行。
  状态表里"到冻结态"的路径每个起点不一样（Sharing 要经 Fenced），写 freeze 时先试直达再走绕行。

## 流程与记录

- **每轮先 `grep -n 'RR-' docs/bug/README.md | head` 看散文头，不只看表**：审查从 09-15 第七轮起只在索引头部散文里登记新 RR、不加表行，我连漏两轮五条。
  标已修复时把表行补上。
- **用户直接提出的缺陷没有 RR 编号**：不要自造 RR。记录文件名用 `U-xxxx-<topic>.md`（或 roost-coding 的 `<issue-id>`），`docs/bugfix/README.md` 索引照加，
  其余文档按 SKILL §4，来源写明"用户复审提出"。
- **一轮登记很多条时先分流**：能靠"补一处判断"收敛的先做完并提交；需要先定契约的（保留期限、订阅代际、不确定状态机、可取消的锁等待）不要赶工，
  在 `docs/bugfix/README.md` 末尾写清每条为什么没修、要先定什么。赶出来的修复下一轮会被打回。
- **复核残余的记法**：不新编号、不新 T；原 RR 的 bug / bugfix 记录末尾各加"## 复核后的补修"，相关 T 行处置列追加一句，
  两个 README 的那一行改成"已修复（含残余补修，未发版）"，CHANGELOG 单独一条，交接文档 §7 该行备注。旧 U 单元的残余仍按原 U 行追加（ledger 里已有的行）。

## 环境与工具

- codebase-memory 的 `index_repository` 报 "pre-coordination or unverified CBM generation is active"：是 `/private/tmp/cbm-daemon-502/` 的 flock 被一个停住 / 孤儿的
  codebase-memory 进程占着（09-16 根因：手动起的 `--ui=true` 实例处于 stopped 态八天），`lsof` 那个目录找持有者、`kill` + `kill -CONT` 让它退出、
  挪走无人持有的 `.sock/.anc`，再 `codebase-memory-mcp daemon start`。MCP 断连后用 `codebase-memory-mcp cli <tool> --flag value` 从 Bash 继续查图，
  不要退回纯读源码。记忆 `codebase-memory-daemon-lock`。
- 集成环境：`kit/scripts/integration/dataengine-env.sh up|test|status|heal|fault`，brew 二进制不是 docker。**在用的是新环境** `~/.roost-it/roost-dataengine-it`
  （`ROOST_IT_HOME=$HOME/.roost-it ROOST_IT_PORT_OFFSET=1000`，Mongo 28117～28119、NATS 15222～15224、Redis 17379、toxiproxy 19474）；
  旧 `/tmp/roost-dataengine-it`（Mongo 27117～、Redis 16379）mongo-3 已缺数据文件但仍被另一会话的 demo 使用，不要停、不要重置。
  env 文件含凭据，只 source、不输出、不提交；残留的 `ROOST_IT_HOME` 会让脚本报 "refuse unsafe root"。DataEngine 隔离依赖的准备另见 `docs/feature/DATAENGINE-RECOVERY-2026-09-24.md`。
- Remote 长跑 / 验收（C01、B30、`scripts/perf/remote.sh`）共用 `$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock`，只能串行；中止后按 C01-RUNBOOK §8 清理
  `roost_remote_generated_*` 库与同名前缀 Redis 键，其他库一律不动。

## 历史（包已删除，只留教训）

- **`statesync` 的 Replicator 一族已在 M-15 删除，帧编解码现为 `sync/frame`**：`Replicator` 持 `SnapshotRing` + 每会话 `SessionState{sent[tick], ackTick, forceFull, generation}`；
  `PrepareLatest` → `state.prepare` → 投影 → `BuildDelta(base)` → `PreparedFrame.Commit` 才写 `sent`；`SendLatest` 先发后 Commit（所以"只在 Commit 拒绝"来不及）。
  harness `mustSnapshot(t, tick, objects)`、`testLODRegistry`、`lodObject`、`ProjectorFunc` 直接当投影器。"释放某个意图"要先找它**已有**的释放点
  （forceFull 在全量帧提交处已经有），多出来的那个往往就是 bug。"按绝对 tick 取模"的周期判定换成"两次事件是否跨过区间边界"（`a/n != b/n`），
  无状态、无相位、逐点等价。
- **room 广播器在 M-13 退场，room/ 在 ARCH-10 改为 `sync/syncbus/driver`**：旧 harness `NewRoomTransportSink` + `NewRoomBroadcaster(id, sink)`、`testRoomState`、
  `RoomSessionResolverFunc`；末端 `AtomicBatchTransport` 用两阶段注入替身（第一次 backpressure、第二次任意错误）。
  教训见 sync 一节"不可撤销的副作用"与"Set 某个协作者"两条。
