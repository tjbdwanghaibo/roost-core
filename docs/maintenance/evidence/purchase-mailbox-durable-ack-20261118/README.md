# purchase / mailbox：外部确认放到本地修改持久之后（2026-11-18）

> **同日改法已替换**：§1～§7 是第一版（`WaitEntityProjection` 屏障，提交 `cb0e5b14`）。维护者随后改为两个 handler 用 `durability=pipelined`，屏障代码已删除，现行做法与验证见 [§8](#8-改为-durabilitypipelined2026-11-18-第二版)。

基线 main `0a9976b8`。维护者 2026-11-18 定 **A**：handler 仍用缺省 async（不回退 strict），purchase 的 `Ack`、mailbox 的 `CommitClaim` 放到本地这笔修改已持久之后。问题来源见[模块改用 async 的核对 §4](../modules-async-20261118/README.md#4-资产类模块核对崩溃后重放--重试是否仍幂等)。本轮只改两个模块的模板、模块测试、清单、README，`codegen/internal/roost` 里这两个模块的场景数与变异锚点，`scripts/test-modules-generated.sh` 的场景数，以及 `demo/WALKTHROUGH.md` 描述这两条流程的句子；没有改框架。

## 1. 选的等持久手段

用 DataEngine Mod（`wiring/dataengine`，能力名 `mods.ModDataEngine`）已经导出的 `WaitEntityProjection(ctx, 完整 Entity ID)`：返回 nil 时，这名玩家在本进程此前已准入的事务都已投影进 Mongo（多数派写，`infra/storage/mongo/driver/client.go`）。

核对过的候选：

| 手段 | 结论 |
| --- | --- |
| `DurableLSN` / `NestMgr.DurableWatermark` | 只覆盖 pipelined 票据（`framework/nestwal/wal.go` 的 `Enqueue` 才分配 LSN）；async 记录走 `Append`，没有 LSN，水位不说明它。不可用。 |
| 提交回执 / `CommitFence` | 只在 committer 内部（`Projector.Commit` 丢弃 `Append` 返回的 fence），生成 Sender 拿不到。 |
| `completion=async` / pipelined | 要改 handler 的持久级别，违背“handler 仍用 async”。 |
| `WAL.Sync(ctx)`（经 `Mod.Runtime().WAL`） | 能做到（屏障 + fsync 覆盖已写入的 async 记录），但要穿过 `engine.Runtime` 的导出字段，`Sync` 的注释只承诺票据记录，不是给业务的接口；全局 fsync。 |
| `Mod.Flush` | WAL sync 加全量回放，全局、串行（`flushGate`），太重。 |
| **`Mod.WaitEntityProjection`（选用）** | 已导出、有中文契约（“跨进程交出实体所有权前的屏障”）、按实体、rehome 模块已经用同样方式从 Registry 取 DataEngine Mod；事务在 `Projector.reserve`（Nest 回复之前）登记，所以回复之后调用一定覆盖它；投影 fatal、WAL terminal、Runtime 停止都返回错误，不会把“不会再投影”当成已持久。 |
| 改成“下一次补领才 Ack 上一次的” | 下一次补领时内存里的游标仍不说明已持久，还要同一个屏障；只是把等待挪后，状态更多。不选。 |

代价：一次补领 / 领取多一次 Mongo 投影往返；Mongo 慢或不可用时，外部确认推迟（purchase 不 Ack、mailbox 不 CommitClaim），本地修改仍在内存里。投影落 Mongo 比 WAL fsync 晚，但比它更强：WAL 尾部掉电丢失时 Mongo 里仍有这笔修改。

取能力的时机：DataEngine Mod 在协议绑定之后才发布（rehome 模块的 `registryDocuments` 注释），所以两个模块都在用到时从 Registry 取，不放进启动检查。purchase 清单的 `needs.mods` 补上 `dataengine`（有 nest 就自动带上，`codegen/internal/roost/catalog.go` 的 `resolveMods`，对安装没有新要求）；mailbox 本来就声明了。

## 2. 修前修后的时序

purchase（`game/controllers/purchase.go` 的 `purchaseDeps.Collect`）：

```
修前：for 每笔 { platform.Order → Sync_PurchaseGrant(async，写进 WAL 页缓存即回复) → queue.Ack(删快照、acked+1) }
修后：for 每笔 { platform.Order → Sync_PurchaseGrant }      // grant()，遇到第一笔发不出去就停
      WaitEntityProjection(玩家)                           // 失败：一条都不 Ack，回执 + 错误
      for 已发的每笔 { queue.Ack }
```

中途某笔发不出去（在途投递、订单未终结、背包满……）时，它之前已发的照样等持久后 Ack，再返回那笔的错误，与修前“已确认的回执仍返回、失败那笔和之后的留在队列”一致。

mailbox（`game/controllers/mailbox.go` 的 `HandleMailboxClaim`）：

```
修前：ReserveClaim → Sync_MailboxClaimReward(async) → CommitClaim
修后：ReserveClaim → Sync_MailboxClaimReward(async) → WaitEntityProjection(玩家) → CommitClaim
      等持久失败：不 CommitClaim、不归还预留，答成功（附件在本进程里）并记 ERROR
```

账本已有这封的重试（`Claimed=false`）同样先等：第一次的发放可能还没持久。

## 3. 测试：先红后绿

全部 `GOWORK=off`。用本检出的 `roost` 生成临时工程（`project new pmdurable -template game --skip-deps`，`go mod edit -replace` 指向本 worktree，`module add purchase mailbox bag`）。

替身：purchase 的 `purchaseCommitter`、mailbox 的 `mailboxDurability` 兼作提交器与持久屏障——`Commit` 只算“已写进 WAL”（async 回复的时刻），`WaitEntityProjection` 之后才把此前的提交算作已持久。外部确认（purchase 队列替身的 `Ack`、mail 服务替身的 `CommitClaim`）到达时若还有没持久的提交，记一笔“过早确认”；每个端点 / 补领用例结束时断言没有过早确认（等于在那一刻整机掉电会出现“外部已确认、本地丢失”）。另加四条专门场景：

- `TestPurchaseCollectionAcksOnlyAfterTheGrantsArePersisted`：一批两笔，Ack 到达时本地已持久。
- `TestPurchaseCollectionUnpersistedGrantKeepsTheQueue`：等持久失败时回执 + 错误、队列一条不删；恢复后重试得到同一张回执、只提交一次、队列清空。
- `TestMailboxClaimEndpointCommitsTheMailOnlyAfterTheGrantIsPersisted`：首次发放与丢确认后的重试，`CommitClaim` 都在持久之后。
- `TestMailboxClaimEndpointUnpersistedGrantLeavesTheMailUnclaimed`：等持久失败时答成功、邮件不标已领；租约到期后再领只发一次并补上确认。

**修前（基线代码 + 新测试）**：`go test -mod=mod -count=1 -run '^TestPurchaseCollection|^TestMailboxClaimEndpoint' ./game/controllers`。基线的 `purchaseDeps` 没有持久屏障字段，红跑时把测试夹具里 `persist: f.committer` 这一项去掉（唯一一处编译不过的地方，断言不变）。原样输出（节选失败行）：

```
--- FAIL: TestMailboxClaimEndpointReservesGrantsAndCommits (0.00s)
    mailbox_test.go:263: mails committed before the grant was persisted locally: [57fc8d98c987469dcae61ba7ac7a9196]
--- FAIL: TestMailboxClaimEndpointRetryAfterLostCommitPaysOnce (0.00s)
    mailbox_test.go:263: mails committed before the grant was persisted locally: [addfcc6e43780b125c01c879e3c0128c a85feca4603319cca8dc7a64dad84c1e addfcc6e43780b125c01c879e3c0128c]
--- FAIL: TestMailboxClaimEndpointTimedOutGrantThatCommittedPaysOnce (0.00s)
    mailbox_test.go:263: mails committed before the grant was persisted locally: [bea4798a6f01905145110120fe30047e]
--- FAIL: TestMailboxClaimEndpointGrantsARegisteredType (0.00s)
    mailbox_test.go:263: mails committed before the grant was persisted locally: [9e6c533705a7d9f5d135551d9c96433a 732a9a42ee98e0b5cef7b7333570edb6 9e6c533705a7d9f5d135551d9c96433a]
--- FAIL: TestMailboxClaimEndpointCommitsTheMailOnlyAfterTheGrantIsPersisted (0.00s)
    mailbox_test.go:793: the mail was committed while the grant was only written, not persisted: [c03de4d36b4ce915ef7627a0c3270a22]
    mailbox_test.go:263: mails committed before the grant was persisted locally: [c03de4d36b4ce915ef7627a0c3270a22]
--- FAIL: TestMailboxClaimEndpointUnpersistedGrantLeavesTheMailUnclaimed (0.00s)
    mailbox_test.go:825: the mail service committed a mail whose grant was not persisted
    mailbox_test.go:263: mails committed before the grant was persisted locally: [af1f9107577470d1d1375fde23fcc01a]
--- FAIL: TestPurchaseCollectionAckFailureReturnsSameReceiptOnRetry (0.00s)
    purchase_test.go:183: orders acknowledged before the grant was persisted locally: [order-1]
--- FAIL: TestPurchaseCollectionSettledElsewhereDoesNotGrant (0.00s)
    purchase_test.go:183: orders acknowledged before the grant was persisted locally: [order-1]
--- FAIL: TestPurchaseCollectionAcksOnlyAfterTheGrantsArePersisted (0.00s)
    purchase_test.go:278: orders acknowledged while the grant was only written, not persisted: [order-1 order-2]
    purchase_test.go:183: orders acknowledged before the grant was persisted locally: [order-1 order-2]
--- FAIL: TestPurchaseCollectionUnpersistedGrantKeepsTheQueue (0.00s)
    purchase_test.go:290: receipts=[{Settled:false OrderID:order-1 Sequence:1 Reward:{Kind:coins ID:0 Count:100}}] err=<nil>, want the receipt and an error
    purchase_test.go:183: orders acknowledged before the grant was persisted locally: [order-1]
FAIL
FAIL	example.com/pmdurable/game/controllers	1.043s
```

结清订单（`Settled`）也红：它只推进游标，同样是一笔要持久的提交，先 Ack 再掉电同样卡住序号。

**修后**：同一命令 17 条全过（`ok example.com/pmdurable/game/controllers 0.334s`）；`go vet -tags integration ./game/controllers` 通过（真实依赖档的 `newMailboxTestController` 多了持久屏障参数）。

## 4. 变异（修后的生成工程上，改一处、跑同一组用例、还原）

| 变异 | 结果 |
| --- | --- |
| purchase：先 Ack 再等持久（恢复“回复后立即确认”） | 红：4 条（Acks… `orders acknowledged while the grant was only written`、Unpersisted… `acks=1 pending=0`、AckFailure / Settled 的结束核对） |
| purchase：去掉等持久 | 红：4 条（同上，Unpersisted 报 `err=<nil>, want the receipt and an error`） |
| purchase：等持久失败也照常 Ack | 红：`TestPurchaseCollectionUnpersistedGrantKeepsTheQueue` |
| mailbox：CommitClaim 先于等持久（恢复“回复后立即确认”） | 红：6 条（含两条新场景与四条已有端点用例的结束核对） |
| mailbox：等持久失败也照常 CommitClaim | 红：`TestMailboxClaimEndpointUnpersistedGrantLeavesTheMailUnclaimed`（`the mail service committed a mail whose grant was not persisted`） |

`codegen/internal/roost/module_purchase_test.go` 另加一条变异（去掉等持久，`TestPurchaseCollectionAcksOnlyAfterTheGrantsArePersisted` 必须红），并把“未终结订单”变异的锚点改成新的返回语句。

## 5. 场景数与工程检查

- 清单：purchase `verify.pass` 21 → 23（`game/controllers` 6 → 8），mailbox 33 → 35（端点 9 → 11）；同步 `scripts/test-modules-generated.sh`、`codegen/internal/roost` 的 `module_purchase_test.go`、`module_profile_mail_test.go`、`module_gm_test.go` 与 `modules/README.md` 的表。
- 临时工程 `roost module verify`：bag 20、mailbox 35、mailbox+bag 9、purchase 23、purchase+bag 4、wallet 17，零跳过；4 项工程检查（build、vet、`roost id check`、`roost generate --check`）通过。再 `module add enter` 后：另有 enter 29、mailbox+enter 5、purchase+enter 2，全部通过。
- `codegen/internal/roost`（只跑这两个模块相关的 3 个用例，`-run '^(TestModulePurchaseScenariosAndMutations|TestModuleProfileMailScenariosAndMutations|TestModuleGMScenariosAndMutations)$'`）：全过，`ok … 470.386s`；purchase 的新变异日志 `caught mutation in game/controllers/purchase.go: orders acknowledged while the grant was only written, not persisted`。
- 根包 `go test -count=1 .`（文档链接等仓库门禁）：通过。
- `scripts/testdata/operations-real/install.py` 的锚点 `\t\tif err := c.queue.Ack(ctx, grant); err != nil {` 在新代码里仍唯一存在（在等持久之后的 Ack 循环里），真实进程演练的暂停点因此落在“发货已持久、Ack 之前”。演练本身本轮没有重跑。

## 6. 剩余边界

- 持久的含义是“已投影进 Mongo（多数派写）”，依赖 Mongo 的部署持久性；与 Redis 一样由部署保证。
- 等持久失败或超时（投影 fatal、WAL 不健康、Runtime 停止、请求截止）：purchase 回执 + 错误、队列不删；mailbox 答成功、邮件不确认、预留留到租约到期。掉电恰好丢了这一笔时，客户端看到过一次回执 / 成功、重连后奖励没了：purchase 下次补领重发；mailbox 租约到期后再领重发，但若邮件在这之前过期，附件随邮件作废（与从没领过的过期邮件相同）。
- 一次补领 / 领取多一次投影等待；Mongo 积压时这两条请求变慢，不影响别的 handler。
- `WaitEntityProjection` 只看本进程：玩家在等待期间被换服、交出（rehome）不在本轮考虑；那条路径本来就先排空投影。
- naming、rehome、gacha 的类似依赖不在本轮范围（仍按[模块 async 核对](../modules-async-20261118/README.md)）。

## 7. 未验证

- 没有真实掉电 / 内核崩溃演练；结论来自替身上“外部确认时本地是否已持久”的断言与源码核对。
- 没有在真实 DataEngine（文件 WAL + Mongo）上跑这两个模块的端点；`scripts/test-operations-real.sh` 与 mailbox 的 `-tags integration` 档没有重跑（后者的 Nest 仍是替身提交器）。
- async 记录的投影不等 WAL fsync（`Projector.Commit` 写入即唤醒回放，`WAL.Replay` 读到已写入的末尾）：掉电后 WAL 尾部丢失而 Mongo 已有该修改时，启动排空的行为（是否会在 `classifyNoMatch` 判冲突）本轮没有核对，属于框架层面，不随本轮改动变化。
- 没跑全量 pretag 与全模块矩阵（按任务约定由协调者统一跑）。

## 8. 改为 durability=pipelined（2026-11-18 第二版）

基线 main `5845553a`（含第一版 `cb0e5b14`）。维护者原话：“很多都不需要strick的方式，pileline就已经很严格了”。规则：回复后马上对外部确认的写用 pipelined。

### 8.1 改法

- `PurchaseGrant`（`game/handler/purchase_grant.go`）、`handlerMailboxClaimReward`（`game/handler/mailbox_claim_reward.go`）的标记改为 `//roost:nest rollback=undo durability=pipelined`。
- 删掉第一版的屏障：两个端点不再从 Registry 取 DataEngine Mod、不再调 `WaitEntityProjection`；purchase 的 `Collect` 恢复成“逐笔：查 platform → Sender → Ack”（与 `cb0e5b14^` 的代码相同，只多一行注释），mailbox 恢复成 `ReserveClaim → Sync_MailboxClaimReward → CommitClaim`；purchase 清单 `needs.mods` 去掉补上的 `dataengine`（有 nest 自动带上）。
- 理由（按源码核对）：`framework/nest/execution.go` 的 `commitPipelined` 在锁内 `Enqueue`（唯一拒绝点），提前放锁，`<-ticket.Done()` 之后才完成事务并回复；票据由 WAL 组提交 fsync 后完成（`framework/nestwal/wal.go` 的 `resolveDurableLocked`）。所以 Sender 返回时这笔记录已 fsync，随后的 `Ack` / `CommitClaim` 不会比它“活得久”。比第一版少一次 Mongo 投影往返，也不依赖 Mongo 的可用性；代价是这条 handler 的回复多等一次组提交 fsync（与 strict 相同的等待，但实体锁在入队后就放了）。
- fsync 失败时票据带 `ErrCommitIndeterminate`，Nest fence 实例并把它交给调用方：purchase 不当回执、不 `Ack`；mailbox 按发放失败处理（归还预留、答错误码）。恢复后由 WAL 回放决定这笔在不在，账本 / 游标保证不重复。

### 8.2 运行时要求（逐项核对）

| 要求 | 结论 |
| --- | --- |
| `Rollback != none` | 两个都是 `rollback=undo`。 |
| 提交器实现 `Enqueue` / `DurableLSN`，否则 `ErrPipelinedCommitterRequired` | 正式进程的 committer 是 DataEngine Mod（`wiring/dataengine` 的 `Mod.Enqueue` / `DurableLSN`）。模块测试的替身提交器全部补上：purchase / mailbox 的 handler 与端点测试、mailbox 真实依赖档（`-tags integration`，Nest 替身改用 `mailboxDurability`）、`demo/internal/service/game/activity_test.go` 的 `settleCommitter`（demo 结算后逐封调领取 handler）。 |
| `nest.pipelined.allowlist` | 生成配置（`configs/service/config.game.yaml` 与 prod 示例）是 `allowlist: []`，空 = 全部允许，不用动。框架只自动补 saga 原生步骤；工程写了非空名单时必须把 `handlerPurchaseGrant`、`handlerMailboxClaimReward` 列进去，否则 `ErrPipelinedNotAllowed`。写进了两个模块 README 的“前提”与 `modules/README.md` 的“持久级别”。 |
| 异步完成 | 两个都不是 saga 原生步骤、没声明 `completion=async`：第一阶段，在派发 worker 上等票据。 |
| Remote 批次 | 只写 Player（本地实体），不走 strict 回退路径。 |

### 8.3 测试与变异

替身：端点 / 补领测试的提交器按持久级别区分——`Commit`（async）只算“已写进 WAL”，strict 算已持久；`Enqueue`（pipelined）入队即写入，票据在另一个 goroutine 里睡 1ms（组提交间隔）后“fsync”、才算已持久并完成。外部确认（队列替身的 `Ack`、mail 替身的 `CommitClaim`）到达时若还有没持久的提交，记一笔“过早确认”；保留第一版的“每个用例结束时没有过早确认”核对与 `…AcksOnlyAfterTheGrantsArePersisted`、`…CommitsTheMailOnlyAfterTheGrantIsPersisted` 两条场景。第一版的两条“等持久失败”场景换成 fsync 结果未知的场景：`TestPurchaseCollectionIndeterminateGrantKeepsTheQueue`（`ErrCommitIndeterminate`、无回执、不 Ack）、`TestMailboxClaimEndpointIndeterminateGrantLeavesTheMailUnclaimed`（答错误码、邮件未领、预留已归还）。handler 层 `TestPurchaseGrantCommitsRewardAndCursor`、`TestMailboxFirstClaimPaysTheCoins` 断言提交记录声明的是 pipelined。场景数不变（purchase 23、mailbox 35）。

全部 `GOWORK=off`，临时工程 `module add purchase mailbox bag` 后再 `module add enter`：

- 定向 19 条（端点 / 补领 17 + handler 2）全过；`go vet -tags integration ./game/controllers` 通过。
- 变异（生成工程上改、跑同一组、还原后复跑全绿）：

| 变异 | 结果 |
| --- | --- |
| purchase 生成注册 `DurabilityPipelined` → `DurabilityAsync` | 红：controllers 4 条（`orders acknowledged while the grant was only written, not persisted: [order-1 order-2]` 等）+ handler `the grant committed with durability async, want pipelined` |
| purchase 标记去掉 `durability=pipelined` 后 `roost generate` | 红：同上 5 条 |
| mailbox 生成注册改回 async | 红：端点 6 条（`mails committed before the grant was persisted locally`、`claim whose grant is indeterminate = &{Code:0 …}`）+ handler 1 条 |
| mailbox 标记去掉 durability 后重新生成 | 红：同上 7 条 |

- `roost module verify`：bag 20、enter 29、mailbox 35、mailbox+bag 9、mailbox+enter 5、purchase 23、purchase+bag 4、purchase+enter 2、wallet 17，零跳过，4 项工程检查通过。
- `codegen/internal/roost` 相关 3 个用例：全过（`ok … 249.865s`）；purchase 的变异换成“生成注册改回 async”，日志 `caught mutation in game/handler/purchase_grant_nest_gen.go: orders acknowledged while the grant was only written, not persisted`；“未终结订单”变异的锚点恢复原文。
- game-demo：生成后 `go vet ./internal/service/game` 通过，`TestActivitySettlementPaysCoinsIntoTheWallet` 通过（领取 handler 走 pipelined 替身）。
- 真实进程：`scripts/test-operations-real.sh`，私有 `mirror-local.sh` 环境（偏移 26000，etcd 58979，端口基 46500，跑完 `clean`），工作树 = `5845553a` + 本轮未提交改动：`TestOperationsGMHTTPMailAndRehome`、`TestOperationsPurchaseCrashAcrossSIDAndRedisRestart` 两条 PASS（正式 DataEngine 提交器、`allowlist: []` 下 pipelined 发货与领取正常；SIGKILL 停在 pipelined Sender 返回之后、Ack 之前，冷启后 `next=acked=Applied=2`、钱包 200）。`install.py` 的 Ack 锚点在模板里仍唯一。
- 根包 `go test -count=1 .`：通过。

### 8.4 剩余边界与未验证

- 两条 handler 的回复多等一次组提交 fsync；同一玩家连续补领 / 领取时，下一笔要等上一笔回复（第一阶段 pipelined），不影响别的玩家。
- fsync 失败是实例级故障（fence），这两条请求的结果未知，恢复后按账本 / 游标收敛。
- 没有真实掉电演练；没有跑 mailbox 的 `-tags integration` 档（只做了编译检查）。§7 关于“投影先于 fsync 时掉电后的启动排空”仍是框架层面的尚未确认项，pipelined 记录要 fsync 后才投影（`Projector.Enqueue` 的 `signalWhenDurable`），这两条写不再落在那个窗口里。
