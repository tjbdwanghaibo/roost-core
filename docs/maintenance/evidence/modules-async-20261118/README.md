# 业务模块改用缺省 async（2026-11-18）

维护者 2026-11-18 决定：模块里哪里都不用 `durability=strict`。saga 相关的写用 `durability=pipelined`，只读的保留 `durability=memory`，其余一律缺省 async（`//roost:nest` 不写 durability）。`strict` 仍是框架能力，运行时照常支持，工程自己的 handler 可以显式选择。基线 main `f229585e`（v1.56.0）。本轮只改 `modules/` 的模板、模块测试与 README，以及 `docs/framework` 里描述模块持久级别的句子；没有改 `codegen/`、`demo/`、`scripts/testdata/`，没有改 Nest / DataEngine 的行为。

## 1. async 的实际含义（按源码核对）

- `framework/nestwal/wal.go`：`Append` 对 async 记录“只等写入”——批写用 `writeFull(w.active, …)`，`w.active` 是 `*os.File`，写进操作系统页缓存后就回复；fsync 由刷盘 goroutine 按组提交 ticker（`GroupCommitInterval`，缺省 10ms，配置键 `dataengine.wal.group_commit_interval`）或 strict / pipelined 记录的敲门触发。
- 因此**进程崩溃**（kill -9、panic、OOM）不丢已回复的 async 修改：数据已在内核里，重启后 WAL 回放照常读到。**整机掉电、内核崩溃、虚机被强停**时，最后一个组提交间隔里已回复的修改可能丢失。WAL 是单写者单日志，丢的只是尾部；一笔事务是一条记录，多实体一起丢或一起在。（任务简报里“进程崩溃时可能丢最后一小段”的说法与源码不符，模块 README 按源码写。）
- Remote 托管实体上的 async 写不等 Remote 投影结论就回复（[Nest §6 第 4 行](../../../framework/guide/02-nest-entity.md#6-回复错误判别是否可能已提交能否重试)）；pipelined 写在带 Remote 批次时走 strict 路径（`framework/nest/execution.go` 的 `takesPipelinedCompletion`）。
- `nest.Emit` 的 best-effort effect 仍会把 async 升到 strict（`framework/nest/rollback.go`），这是框架规则，模块里不受影响。

## 2. 改了什么

| 项 | 数量 | 说明 |
| --- | --- | --- |
| `//roost:nest … durability=strict` 改为缺省 async | 93 处 | 去掉 `durability=strict`，保留 `rollback=undo`（含 3 处 `target=Player` 变体）；连同下一行的 6 处共涉及 31 个模块的 81 个模板文件 |
| `durability=strict` 改为 `durability=pipelined` | 6 处 | 都在 alliance，见 §3 |
| 原有 `durability=pipelined` | 2 处，未动 | alliance 原生步骤 `AllianceDebit` / `AllianceCredit` |
| 原有 `durability=memory` | 未动 | 只读 handler |
| 模块测试里手写注册的 `HandlerMeta{…Durability: nest.DurabilityStrict}` | 28 处改为 `DurabilityAsync` | 与生成的注册一致 |
| 断言提交记录是 strict 的测试 | 4 处改为断言 async | mailbox（发放）、dungeon（开局）、terrain（聚合提交）、cdkey（意图）；mailbox 的注释改写了代价 |
| combat 心跳 | 1 处 | 去掉 `heartbeat.WithDurability(nest.DurabilityStrict)`，取心跳缺省 async |
| alliance 测试提交器 `effectLog` | 加 `Enqueue` / `DurableLSN` | 联盟服务的 6 个 pipelined 写需要 `PipelinedTransactionCommitter`，否则派发报 `ErrPipelinedCommitterRequired` |
| README 与代码注释 | 29 个模块 README、`modules/README.md` 约定、facebook / naming / cellworld 注释 | 写 async 的事实与代价；`grep -rn "durability=strict" modules/` 为 0 |
| `docs/framework` | 3 篇 | `PLAYER-ACCESS.md`（引用的装备 handler 标记与持久说明）、`SAGA.md` 与 `guide/06-saga.md`（alliance 的非原生步骤 pipelined 写、allowlist 要自己列） |

## 3. alliance 的逐个归类

判定规则：作为 saga 步骤或补偿被 saga 调用、回复会被拿去推进协调器的，改 pipelined；其余缺省 async。联盟实体是 Remote 托管，6 个 pipelined 写实际走 strict 路径，运行时行为与改前的 strict 相同。

| handler | 归类 | 理由 |
| --- | --- | --- |
| `GuardReserve`（简报列出） | pipelined | alliance_found 的两步建盟（Saga 建盟步骤）与跨实例入盟拿这次预约去建盟 / 入席。预约在回复后丢失：守卫“少占”（玩家在联盟、守卫为空），一人一盟失效；守卫上的回执是建盟重投的去重锚，丢了重投会再建一个联盟。 |
| `GuardClaimReward`（简报列出） | pipelined | alliance_reward 的 claim 步骤。标记丢失而协调器已推进到发奖，同一周期可再领一次。 |
| `GuardReleaseReward`（简报列出） | pipelined | claim 步骤的补偿。撤销丢失而协调器判补偿完成，标记留着、道具没发，这一周期奖励丢失。 |
| `GuardResolve`（简报列出） | async | 建盟后 `finishGuard` 的尽力确认与入盟确认；丢失只让守卫停在预约（多占），由预约期限与惰性校正修复，不影响 Saga 结论。 |
| `GuardClear`（简报列出） | async | 校正守卫（清掉已不成立的成员关系）；丢失只让守卫多占，下次请求再校正。 |
| `StartAllianceOp`（简报列出） | async | 不被 Saga 调用，是启动方：不改 Player，启动意图随这条记录经 outbox 送出。记录丢失时协调器从没收到启动，客户端的 `SagaKey` 查不到进展，重发同一帧（业务键相同）即可；没有资产变化。 |
| `AllianceFound`、`AllianceEstablish`（简报未列，补判） | pipelined | alliance_found 的建盟步骤经 RPC `Found` 调到（同实例一步、跨实例两步）。建盟丢失而协调器已结束 Saga：建盟费已扣、联盟没了、不会补偿。 |
| `AllianceDonate`（简报未列，补判） | pipelined | alliance_donate 的捐赠步骤。记账丢失而 Saga 已结束：道具扣了、资金与贡献没记上。 |
| 其余 14 个联盟写（Admit、AdmitReserved、Apply、Reject、Remove、RemoveDetached、Transfer、Settle、Home、DeclareWar、DeclareWarDetached、AcknowledgeWar、WarAcknowledged、WarAcks）与 game 侧 `ShowAlliance` | async | 不是 Saga 步骤；靠请求号回执与守卫校正收敛。跨实例写丢失按代码推断只让守卫多占，没有掉电场景覆盖，**尚未确认**。 |

pipelined 的运行要求逐项核对：`rollback=undo`（满足 `Rollback != none`）；正式进程的 committer 是 DataEngine Projector，实现了 `Enqueue` / `DurableLSN`；生成配置 `nest.pipelined.allowlist: []`（空 = 全部允许），工程自己写了名单时要把这 6 个列进去（框架只自动补原生步骤，`docs/framework/SAGA.md` 已写明）；它们不是原生步骤，不进 Nest 异步完成，是第一阶段 pipelined；带 Remote 批次走 strict 路径。

## 4. 资产类模块核对（崩溃后重放 / 重试是否仍幂等）

“掉电”指整机掉电、内核崩溃；进程崩溃不丢已回复的 async 修改，以下各项在进程崩溃下与改前相同。

| 模块 | 幂等锚 | 掉电丢掉已回复的一笔之后 | 结论 |
| --- | --- | --- | --- |
| wallet | 请求键回执与余额同一笔 | 同键重发执行一次 | 幂等，无外部依赖 |
| shop | 请求键回执，扣钱 / 发货 / 限购同一笔 | 同键重发按新购买执行一次 | 幂等 |
| bag | 丢弃协议无外部确认 | 道具回到背包，可再丢 | 无资产复制 |
| gacha | 请求键回执固定成功结果 | 同键重发重新抽样：只扣一次发一次，但**结果可能与客户端已展示的不同** | 资产不重复；“回执固定结果”在掉电下不成立，写进 README |
| cdkey | 玩家账本意图 + directory 属主（玩家号）+ 发件请求键 | 再兑：directory 认出同一属主的占用（`service/directory/store.go` 的 `Reserve`），发件按同一键返回原信封 | 幂等 |
| mailbox | 账本按邮件号去重 | **发放回复后端点让 mail 服务 `CommitClaim`；掉电丢发放而 `CommitClaim` 已到达时，邮件已领、附件没到账**，模块不会发现 | **依赖“回复前已落盘”**，风险写进 README，交维护者决定 |
| purchase | Player 的 `Applied` 游标 + Redis 序号 | **`PurchaseGrant` 回复后 `Ack` 删掉 Redis 快照、推进 `acked`；掉电丢发货而 `Ack` 已执行时，奖励丢失，且下一笔因“序号必须恰好是游标加一”答 `purchase_sequence`，该玩家补领永久卡住**（`game/modules/purchase/queue.go` 的 `pendingGrantsScript` 从 `acked+1` 取，组件 `Grant` 拒绝越号） | **依赖“回复前已落盘”**，风险最大，写进 README，交维护者决定 |
| alliance | 见 §3 | Saga 相关写已是 pipelined | 资产路径不变 |

其他有外部后续动作的模块：

- naming：目录属主是 `naming:<玩家>:<Seq>`，`Seq` 与阶段一起丢失、重发时不变，同一请求键能接着做；客户端换请求键、而丢掉的那一笔已在目录提交了新名字时，名字留在旧属主名下——没有场景覆盖，**尚未确认**。
- rehome：意图丢失而 account 已登记换服时，换服仍由在途索引做完，目标服到达时没有意图：不起冷却、不跑到达钩子（与运维直接 `accountctl` 换服相同）。
- matchmaking：阶段推进丢失而 match 服务已收到排队时，恢复是否仍只开一间房没有场景覆盖，**尚未确认**。
- facebook：收件箱记录回复时未投影进 Mongo，别的 game 立刻查可能看不到；收件箱本来就只是提示。

## 5. 测试结果

全部 `GOWORK=off`，本检出（基线 `f229585e` 加本轮改动）。

- `scripts/test-modules-generated.sh`（`ROOST_MODULES_OUTPUT` 指向会话 scratch，全模块矩阵一次）：`modules: ok`，退出 0。末尾两个全模块工程（正序 / 反序安装）`roost module verify` 每个模块与联动的场景数都等于清单 `verify.pass`、零跳过，4 项工程检查（build、vet、`roost id check`、`roost generate --check`）通过，随后生成工程 `go test -mod=mod ./...` 通过。全模块一行摘要（节选）：`alliance=55 alliance+enter=2 alliance+rehome=2 … facebook=9 … gacha=29 … mailbox=33 … purchase=21 … rehome=62 … room=34 … team=32 … wallet=17 + 4 project checks`。
- 定向复核（模板注释最后改动之后）：新工程装 alliance、facebook、mailbox、purchase：alliance 55、bag 20、facebook 9、mailbox 33、mailbox+bag 9、purchase 21、purchase+bag 4、wallet 17，零跳过，4 项工程检查通过。
- 没有用例因“假设 strict”而失败：改动前就断言 strict 的 4 处用例（§2）同步改成断言 async，原因是这些断言钉的是“这笔提交声明的持久级别”，声明变了断言随之变；mailbox 用例的注释写明了 async 下的掉电代价。alliance 联盟服务测试提交器补 pipelined 能力后 55 条全过。
- 根包 `go test -count=1 .`（文档链接等仓库门禁）：通过。
- `codegen/internal/roost` 的 `-run Module`：见报告（这里只记入已跑完的结果）。

## 6. 未验证

- 没有任何掉电 / 内核崩溃演练（本机做不到），§4 的掉电结论是按源码推断。
- 没有跑 `scripts/testdata` 的 REAL 档与 game-demo。genre 文档（AUTO-CHESS / SURVIVAL / TOWER-DEFENSE / PARTY-ROOM / CARD-RPG / MERGE / IDLE / SESSION-GENRES）记录的“strict 已确认的提交 SIGKILL 后可恢复”是在模块还是 strict 时取得的；改后 enter / room / gacha 等模块写入是 async，按 §1 SIGKILL 下仍应恢复，但未重跑，文档未改。
- demo/ 与 scripts/testdata/ 未改：demo 剩 23 个 `//roost:nest … durability=strict` 标记（15 个文件、共 28 行含 `durability=strict`）、7 处 `DurabilityStrict`；scripts/testdata 剩 80 个标记（19 个文件）、34 处 `DurabilityStrict`。
