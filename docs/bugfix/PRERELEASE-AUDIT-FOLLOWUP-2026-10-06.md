# 发版前审查观察收尾（2026-10-06）

来源：发版前审查的两份报告（随提交 `207163f9` nest allow_stale、`42419890` saga 守卫提交，报告本身没有入库）里列为“观察”、不在那两笔里修的五条。
维护者授权：确认是缺陷的按先红后绿修；纯语义取舍的写文档。分支 `auditfu`，基线 `c99b59f6`，提交 `5a3c4a60`，未发版。

源码核对以当前源码为准：codebase-memory 共享 generation 停在 09-30，本轮用 `rg` 与直接阅读逐行核对（`kit/service/global/activity`、`saga`、`configdata/rules`、`service/mail`）。

| # | 观察 | 结论 | 处理 |
| --- | --- | --- | --- |
| 1 | activity 协调器的派发退避与进度凭证有效期走业务钟 | 缺陷（违反 D-L3“重试与退避属系统钟”） | 修：`Config.SystemNow` |
| 2 | mail 信封存储宽限只覆盖往回拨 ≤ 24h 的偏移 | 限制，语义取舍 | 文档：USER_GUIDE、T-270 |
| 3 | saga `ErrDefinitionMissing` 被两条结果流 Term | 缺陷（暂时状态被当成终态） | 修：移出终态，nak 退避 |
| 4 | saga `MongoCommandInbox.Handle` 撞 `ErrDuplicateKey` 回放时不交还自己的 claim | 无可观察后果 | 观察，代码注释写明理由 |
| 5 | `configdata/rules.Lookup` 大小写变体按 map 遍历顺序选 | 缺陷（规则查的值与类型化行不同、且不确定） | 修：`Rows` 按文档顺序定下来 |

## 1. activity 协调器：派发排期与凭证有效期改读系统钟

**触发**：测试环境两次运行之间把 `time.logic_offset` 往回拨 D（例如 +24h → 0）。**预期**：欠下的派发在退避到期后照常可取（重试排期是系统时间）。
**实际**：派发的 `NextAttemptAtUnix`（创建时“立即可取”与每次尝试后的退避，`kit/service/global/activity/service.go` 基线 :1460 / :1632）、到期比较
（`DueDispatches`、`AttemptDispatch`、`OwedDispatches` 传给 owed 索引的 `nowUnix`）都读 `Config.Now`（业务钟），上一轮打下的时间整体晚 D，派发多挂 D 才交得出去；
进度凭证 `ExpiresAtUnix`（:1253）同样按业务钟打戳，而它描述的是账本的相对 TTL（系统时间）。

**根因**：D-L3 实施时把整个协调器划成业务时间（方案 §3.1 activity 行“截止、宽限、退避都是同一个钟上的差值”），没有把重试排期单独拿出来。

**修法**（仿照 mail 的领取租约）：

- `Config.SystemNow`：nil 时沿用 `Now`（只注入一个钟的测试保持一个钟），两者都 nil 时 `time.Now`；Mod 注入 `time.Now`。
- 系统钟：`NextAttemptAtUnix` 的三个写入点（`ensureDispatches` 创建、`AttemptDispatch` 退避、`ReopenDispatch` 重开）、全部“是否到期”比较、owed 索引查询、
  `ProgressReservation.ExpiresAtUnix`。
- 业务钟不变：活动 / 窗口 / 宽限 / 开关窗、`OpeningGrace`，以及记录上的事件时间戳（派发的 `CreatedAtUnix` / `LastAttemptAtUnix` / `AckedAtUnix` / `ExhaustedAtUnix`，
  凭证的 `CreatedAtUnix` / `AppliedAtUnix`，审计与运维时间）。
- `ReopenDispatch` 之前把 `NextAttemptAtUnix` 写 0，Redis owed 索引对 0 用 `CreatedAtUnix`（业务时间）打分；查询改用系统钟后，偏移为正时这个分数落在系统钟的未来，
  重开的派发要等一个偏移才出现在 owed 清单。改为写系统钟的当前时间（仍是“立即可取”）。索引的 0 → `CreatedAtUnix` 兜底只剩改动之前写下的记录，那时偏移为 0（D-L3 未发版）。

**兼容**：持久格式不变；偏移为 0 时两个钟相同，行为不变。

**先红后绿**：

```text
$ GOWORK=off go test -count=1 -run 'TestDispatchBackoffAndProofExpiryAreSystemTime|TestAReopenedDispatchIsOwedNowOnTheSystemClock' ./kit/service/global/activity/
--- FAIL: TestDispatchBackoffAndProofExpiryAreSystemTime (0.00s)
    system_clock_promises_test.go:49: proof ExpiresAtUnix = 1700087000, 24h0m0s from system time + ReservationTTL; want the system clock (the ledger TTL is relative system time)
    system_clock_promises_test.go:63: a dispatch created due immediately in the previous run: activity: dispatch is not due for another attempt: activity group-a/race-untouched/close game 1000 is due at 1700086400, now 1700000000; want it due now, not an offset later
    system_clock_promises_test.go:76: after the backoff the game is owed []; want group-a/race-taken/close listed
    system_clock_promises_test.go:80: after the 5s backoff, moving the offset back by 24h0m0s still holds the owed dispatch: activity: dispatch is not due for another attempt: activity group-a/race-taken/close game 1000 is due at 1700086405, now 1700000005
--- FAIL: TestAReopenedDispatchIsOwedNowOnTheSystemClock (0.00s)
    system_clock_promises_test.go:117: reopened dispatch scores 1700086400 in the owed index (owed=true), system now 1700000060: the game would not find it until the score passes
```

红跑时 `Config` 只加了 `SystemNow` 字段声明（没有任何读取）以便编译。第二条在修前的代码上没有缺陷（修前查询也读业务钟），它守的是本次改动的组合：
查询换成系统钟之后，重开写 0 会变成上面的现象；只改前两处、不改重开时它同样红。修后两条通过；`TestTheModWiresTheCoordinatorToTheBusinessClock` 补断言 Mod 注入的
`SystemNow` 是真实时间。

## 2. mail：信封存储宽限的覆盖范围（只改文档）

信封键 TTL = 业务剩余时长 + `StorageGrace`（缺省 24h）。在 +D₁ 偏移下创建、业务剩余 L 的邮件，键在真实时间 L + G 后被 Redis 回收；下一次运行把偏移往回拨 Δ，
这封邮件按业务时钟要到真实时间 L + Δ 才过期。Δ > G 时存储先于业务过期回收：邮件从列表里提前消失（`service.dropped.total{service="mail",op="list.missing_envelope"}`
计数），领取返回 `ErrMailMissing`。这是“存储 TTL 只兜底且更长”在偏移往回拨时的边界，不是生产问题（生产偏移强制为 0）。

kit 的 mail Mod 没有 `StorageGrace` 配置键，固定用缺省 24h；要覆盖更大的回拨，自己装配 `service/mail.NewRedisStores(client, mail.RedisConfig{..., StorageGrace: ...})`
（代价是过期邮件在 Redis 里多留这么久），或者在回拨超过 24h 前清掉测试环境的邮件数据。写进 USER_GUIDE“业务时钟与系统时钟”与 T-270 处置列。

## 3. saga：结果先于定义到达时 nak 退避，不 Term

**触发**：滚动发布，saga 用新定义版本启动并由已升级的协调器派发；步骤结果被还没升级的协调器（共享 durable）消费。**预期**：定义随新进程上线，结果最终被接收。
**实际**：`Engine.Complete` 返回 `ErrDefinitionMissing`，`isTerminalCompletionError`（`saga/nest_completion_consumer.go` 基线 :149）把它归为终态，两条结果流都 Term 掉消息；
记录等到步骤超时，领到它的若也是旧进程就 fence 到 `ManualRequired`。

**判断**：`Complete` 只在记录正等着这个操作时才查定义（不等的走 `completeNotWaiting`，与定义无关），这时缺定义只可能是“派发它的进程有这个版本、本进程还没有”，
属可恢复的暂时状态。nak 退避之后有两种结局，都有界：定义上线 → 重投被接收；定义一直不来 → 步骤超时后没有定义的协调器按 NC-250 fence 到 `ManualRequired`
（放弃关闭这次操作），之后的重投走 `completeNotWaiting`，作为迟到成功 ack 并告警一次（`LateAfterAbandon`）。`MaxDeliver` 兜底。

**修法**：从 `isTerminalCompletionError` 删掉 `ErrDefinitionMissing`。两条流仍共用这一个函数（O-S5-1 不拆）。同步 SAGA.md 的终态分类段与 saga 方向方案的 O-S5-1 段。

**先红后绿**：

```text
$ GOWORK=off go test -count=1 -run 'TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated|TestADefinitionThatNeverArrivesEndsInTheCoordinatorFence' ./saga/
--- FAIL: TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated (0.00s)
    --- FAIL: TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated/plain_result_stream (0.00s)
        completion_definition_rollout_promises_test.go:61: completion before the definition is registered = saga: definition not registered: rally (permanent=true); want ErrDefinitionMissing nak'd with backoff — the definition arrives with the new process, a terminated result is gone
    --- FAIL: TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated/native_result_stream (0.00s)
        completion_definition_rollout_promises_test.go:61: completion before the definition is registered = saga: definition not registered: rally (permanent=true); want ErrDefinitionMissing nak'd with backoff — the definition arrives with the new process, a terminated result is gone
--- FAIL: TestADefinitionThatNeverArrivesEndsInTheCoordinatorFence (0.00s)
    completion_definition_rollout_promises_test.go:97: first delivery = saga: definition not registered: rally (permanent=true), want a retryable ErrDefinitionMissing
```

修后：两条流第一次投递返回可重试的 `ErrDefinitionMissing`、记录不动；注册定义后同一条消息重投被接收，记录推进到第 1 步。定义不来时 fence 之后的重投被 ack，
`LateAfterAbandon` = 1。O-S5-1 的 `TestCompletionConsumersTermTheSameTerminalErrors` 不改断言通过。

**兼容 / 代价**：定义确实永远不会注册（配置错误）时，结果消息在步骤超时之前按 nak 退避重投（缺省 250ms～30s 退避），占一个 `MaxAckPending` 位；步骤超时由定义的
`Timeout` 决定，与之前“Term 后等超时”的时长相同。

## 4. saga：`ErrDuplicateKey` 回放不交还 claim（观察，不改）

`MongoCommandInbox.Handle` 的执行事务撞回执唯一键（只在混跑时：升级前的步骤进程不写 claim，在本进程 Reserve 与提交之间先提交了同一 CommandID 的回执）时，
回放那份回执，不交还本次拿到的 claim：claim 留在 pending、租约有效。

逐条核对读 claim 的路径（`saga/step_operation_inbox.go`）：同一命令重投时 `reserveInTransaction` 第 1 步先读回执，回放并 `markCompleted`；同一操作实例的其他尝试经
`resolveOtherAttempts` → `attemptResult` 读到这份回执，标记 completed 后回放，只有“没有结论”的尝试才看租约；`operationSuccess` 同样经 `attemptResult`。
所以这份未交还的租约不会让任何投递等待，交还与否没有可观察的差别，写不出在承诺上变红的用例。列为观察，在分支处加注释写明理由（`saga/command_consumer.go`）。
回执过期（缺省 30 天）之后 claim 仍 pending 时会被接替，那已在任何重试窗口之外。

## 5. configdata/rules：大小写变体按文档顺序取最后一个

**事实核对**（Go 1.27.0，`encoding/json` 解进结构体）：

```text
{"level":1,"Level":2} → 2     {"Level":2,"level":1} → 1
{"LEVEL":1,"Level":2} → 2     {"Level":2,"LEVEL":1} → 1
{"level":1,"level":3} → 3
```

几个键落到同一个字段时按文档顺序最后一个生效，**精确拼写并不优先**。任务里说的“先精确”只对“键分派到哪个字段”成立（`fieldForJSONKey`），对“字段最后取哪个值”不成立。
规则若按“先精确”选，`{"level":5,"Level":0}` 查到 5、类型化行是 0，min=1 会放过 0。所以按 encoding/json 的实际行为实现，不按字面的“先精确”。

**触发**：数据文件的一行里同一列有几种大小写拼写。**实际**：`Lookup`（`configdata/rules/rules.go` 基线 :219）先取精确键、否则遍历 map 取第一个变体：精确键在前、变体在后时
查错了值（确定性地错）；没有精确键时取哪个随 map 遍历顺序变。required / unique / min / enum 都可能对着与加载层不同的值判断。

**修法**：`Rows` 解析后检查每行有没有只差大小写的键（全是不含大写的 ASCII 键时直接跳过，不分配）；有的行按原文的键顺序，每组变体只保留最后一个（折叠用每个字符所在
大小写等价类的最小字符，等价于 `strings.EqualFold`，与 encoding/json 的折叠相同）。只有出现这种行时才多解析一次载荷。`Lookup` 仍是“精确键，否则大小写不敏感”，
对 `Rows` 的输出最多一个候选；手拼的 map 仍有几个变体时取字节序最小的键，结果确定。

**前提**：行结构体里没有两个只差大小写的 JSON 名（那样 encoding/json 先按精确名分派，规则层不知道结构体的其他字段）。生成器产出的表不会这样；写在 `Rows` 注释里。

**兼容**：只影响一行里同一列有几种拼写的数据——之前的检查结果不确定或与加载值不一致，现在与加载值一致；没有这种行的数据完全不变。tablegen 的 `-check` 与 CSV 转换共用 `Rows`，同样生效。

**先红后绿**：

```text
$ GOWORK=off go test -count=1 -run 'TestRulesCheckTheValueEncodingJSONDecodes|TestObjectRulesCheckTheValueEncodingJSONDecodes|TestRowsWithoutCaseVariantsAreUnchanged' ./configdata/rules/
--- FAIL: TestRulesCheckTheValueEncodingJSONDecodes (0.00s)
    lookup_order_promises_test.go:44: [{"id":1,"level":5,"Level":0}]: encoding/json decodes level=0, but the rule check refused=false (<nil>) on run 0
--- FAIL: TestObjectRulesCheckTheValueEncodingJSONDecodes (0.00s)
    lookup_order_promises_test.go:64: {"width":3,"Width":0} decodes width=0, but the min=1 rule accepted it
# 只留没有精确键的几种变体（临时副本，跑完删除）：
--- FAIL: TestTmpVariantsOnly (0.00s)
    tmp_variants_test.go:42: [{"id":1,"Level":5,"LEVEL":0}]: encoding/json decodes level=0, but the rule check refused=false (<nil>) on run 0
```

前两条是确定性的红（精确键在前）。没有精确键的那条在修前取决于 map 遍历顺序（每次约一半概率），用例对同一份载荷重复 64 次，修前几乎必红、修后必绿；
它的确定性体现在修后，修前的红不是确定性证据。修后全部通过；`TestLookupIsDeterministicOnAHandBuiltRow` 守手拼 map 的确定选择。用例以 `encoding/json` 解出的结构体为准绳，
不手写期望值。

## 验证（全部 `GOWORK=off`）

- 改动的 `.go` 文件 `gofmt -l` 为空；`go build ./... && go vet ./...`；`go vet -tags integration ./saga/ ./kit/service/global/activity/ ./kit/service/integration/ ./service/mail/`。
- `go test -race -count=3 ./kit/service/global/activity/ ./saga/... ./kit/saga/... ./configdata/... ./codegen/internal/tablegen/`：全部通过。
- `go test -count=1 ./kit/... ./service/... ./codegen/...`：全部通过；根包 `go test -count=1 .` 通过。
- 没有改 nest / entity / dataengine / sync，没有跑 glsvet；没有改生成形状（模板、生成器输出不变），没有跑 `go generate` 与生成工程。没有跑真实依赖（存储格式未变，见下）。

## 未验证 / 风险

- 第 1 条只在内存存储上验证；Redis owed 索引的打分函数 `owedDispatchEntry` 有直接断言，没有跑真实 Redis 的 integration（存储格式未变）。
- 第 3 条的“滚动发布”用两个 Engine 共用一个 mongotest 存储模拟，没有在真实 NATS 上跑 nak 退避与 `MaxDeliver`。
- 第 5 条的前提（结构体里没有只差大小写的 JSON 名）没有在注册时强制。
