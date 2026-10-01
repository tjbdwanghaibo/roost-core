# Service 第五批修复与收敛（2026-09-29）

基线 `cec1dd30c66f420d9e01f4e659251abe3dfaec09`，本轮 fetch/pull 无 main 新源码，复用隔离工作树。实施上轮[RR-20260929-25/26/27 与旧 RR-20260914-02 残余](../bug/REVIEW-2026-09-29-services-05.md)，**4/4 原触发已修复并有正式回归**。不发版、部署或迁移生产数据。

## 决策与实际行为

| 项 | 最终实现 | 正式行为回归 |
| --- | --- | --- |
| RR-25 P2 | FIFO/ScoreWindow 的公开 Group 先 Queue.Validate，非法模式/大小返回既有 ErrQueueInvalid；策略仍在 Store CAS 外运行 | service/match/bugfix_grouping_boundary_test.go：两策略 × 非法大小 × 空/非空候选；合法 2/64 和不足候选控制，无 panic |
| RR-26 P2 | nextScore 在正/负 int64 加法溢出时返回 ErrScoreInvalid，发生在 Lua CAS 和 request ring 写入前，未做饱和 | kit/service/rank/bugfix_add_overflow_test.go：拒绝后 score/Tie/Brief/rank 不变、同 RequestID 的合法重试成功；MaxInt64-1 的两个并发 +1 仅一个成功，另一个在 CAS 重读后拒绝 |
| RR-27 P3 | Prune 遍历有界 Ring，按每条 StoredAtUnix 清理、最多 limit；Seq 顺序和 Requests 保持。pageOf 同时报告页内/游标边界及清空尾部缺失，不改变 NextCursor/PrevCursor/HasMore 的保留消息含义 | kit/service/chat/bugfix_retention_gap_test.go：2 秒偏移形成中间/尾部洞，分批清理、正反分页、空页、只报告本页跨越的缺口、保留去重证明与计数；Memory/真实 Redis |
| RR-20260914-02 P2 残余 | 复用 Window.Opening，新增可选 Intent 保存完整初始 Activity 创建计划。Create 的错误/超时不释放已经承诺的名额；超过 OpeningGrace 的 sweep 帮助同计划创建与确认。confirmWindow 只消费已有名额，不能无条件补写。旧超容量窗口用可选持久 ScanAfter 轮转，每批至多 256 个不同活动 key | kit/service/global/activity/bugfix_opening_recovery_test.go：迟到原 Create、满 256 名额时额外 Open 被拒且原聚合可完成；admission/Create/confirm 写后丢回复与同 store 重建；旧 257 窗口跨重建扫描尾部、读取有界；legacy Opening 同 key 恢复；Memory/Redis |

没有新增包、存储系统或后台 worker。新增 Intent/ScanAfter 由 Window.clone 深复制，避免 Memory 路径共享指针/ExpectedGameSIDs。恢复计划只创建 Pending；不会替 game 发 Notify 或凭全局时钟完成未通知的活动。

### Activity 为什么没有继续“超时回收名额”

两个独立 versionstore CAS 无法证明一个已经开始的 Create 在 grace 后不会落库。先回收名额、迟到时再确认会超容量；只在迟到确认拒绝又会留下无扫描入口的已创建活动。本批选择保留持久恢复责任，由 sweep 帮助原计划完成。新的拒绝发生在**第 257 次准入**，不是默许第 257 个活动后再丢索引。

如果 sweep 先完成 Create，迟到原 Open 的 insert-only Create 返回已存在，原 Open 可返回 ErrExists；调用者用 LookupActivity 读同 key 的权威结果，不再得到一份不同预期集合的活动。普通重复 Open 的 ErrExists 契约保留。

原 TestSweepReclaimsAnAbandonedOpeningEntryAfterGrace 曾期待普通连接错误后自动遗忘，这与“错误可能已经提交”相冲突。本批改为错误持续时名额仍保留并上报，后端恢复后同计划完成。旧红测文档不改；正式测试更新真实安全契约，不 skip。

## 兼容、升级与旧数据

- RPC 签名/错误码不变；Grouping 的非法输入现在被拒绝，Rank 溢出现在被拒绝。拒绝的 Rank 请求不消耗幂等键。过去已溢出的分数不能自动推导原值，需业务对账，本批不修历史数据。
- Chat wire 字段不变，但 Gap 现在也表示页内/尾部洞；年龄清理可产生非连续 Seq，客户端不能把 HasMore=false 等同“从未丢过消息”。空 forward 页仍保持输入 NextCursor，LatestSeq 提供最高已发序号。Requests 证明保留到原有限计数窗口。
- **Activity 全部 owner 升级并排空旧调用后再启用新流程；不支持混合旧 owner 写 Window。**旧代码会丢弃未知 Intent/ScanAfter 字段或继续回收名额，回退旧版会重新暴露原缺陷。JSON 可读不代表混合版本安全。
- OpeningEntry 新增字段；仓外使用位置复合字面量的代码需改为命名字段并重新编译。Core 内及正式生成消费者已编译通过，未证明所有仓外手写调用者兼容。
- Chat 所有历史读取/清理 owner 应一起升级，旧 pageOf 不理解新年龄清理产生的洞，混合读取不能兑现新 Gap 语义。
- 新 Activity 创建意图不是可随超时取消的任务。后端持续不可用时名额保留、错误上报，恢复后创建 Pending；未收到 Notify 的 Pending 本来就不按 global 时钟完成。需保留正确 sweep_groups 配置。
- 旧 Opening 若记录已存在，sweep 可以确认；若缺记录且没有 Intent，不能猜 expectedGameSIDs 或安全证明旧调用已取消，所以保留名额，按原业务 expected 集合同 key Open 恢复。**未实现自动猜测/自动取消 legacy 意图**；之前已经被遗忘的 orphan 仍需业务按已知 key 对账。
- 对旧超容量 Window 提供有界后端读取轮转和跨重建游标；原 Window JSON 本体的读取/复制成本仍随旧记录大小增长，未声称其内存成本已变成常量。没有自动删除历史活动。
- 无真实 allocator/资金/HA/多节点强杀验证，不把同 store 新建 Service 写成真实进程重启。

## 实际验证与复跑

Go 1.27.0 Windows amd64/race，独立无持久化 Redis loopback 16396。

修前原第五轮 Run-Repro redis 模式：9 leaf pass / 12 leaf fail / 0 skip，go_exit=1；当前基线与上轮生产源码相同，保留[原始失败材料](../review/evidence/service-review-20260929-05/README.md)。临时原始日志在本机 .tmp，小型摘要保存到[本批证据](evidence/service-bugfix-20260929-05/RESULTS.json)。

修后四个受影响包 race 全量 Memory 模式通过，Rank 连接真实 Redis。随后完整：
```powershell
$env:REDIS_ADDR='127.0.0.1:16396'
$env:ROOST_REDIS_TEST_ADDR='127.0.0.1:16396'
$env:ROOST_REVIEW_REDIS='127.0.0.1:16396'
$env:ROOST_REVIEW3_BACKEND='redis'
$env:ROOST_REVIEW4_BACKEND='redis'
$env:ROOST_BUGFIX5_BACKEND='redis'
go test -race -count=1 -p=1 -timeout=180s -tags integration -json ./versionstore ./service/... ./kit/service/...
```
**16 个测试包、830 个测试及子测试事件 pass，0 test fail/skip/build-fail**；servicemetrics 无测试包的事件不算行为跳过。完整套件后补了 Create/confirm 回包丢失两个邻接场景，无生产代码再改；最终全部 TestBugfix5 在两模式各 **27 叶子/32 测试事件全 pass**，无 skip/race 警报。两模式的 Rank 都是 Redis，Activity/Chat 随模式切换，分组算法与后端无关。

`go test -run '^$' ./...` 全仓编译、四包 vet、Chat/Activity 自身 RPC -check 全通过。既有正式 CLI 生成工程指向本工作树，Platform/purchase 编译与 handler/game 的实际测试通过；未重新把网络受限的依赖安装称为成功。

正式四份回归已进入包，复跑不依赖旧 overlay：
```powershell
$env:ROOST_REVIEW_REDIS='127.0.0.1:16396'
$env:ROOST_BUGFIX5_BACKEND='memory' # 随后改成 redis 再跑
go test -race -count=1 -run '^TestBugfix5' ./service/match ./kit/service/rank ./kit/service/chat ./kit/service/global/activity
```

图谱 Verify 使用 search/双向 trace/snippet/coverage，原 generation 05:34:58Z；已改路径 metadata_changed、新工作树测试 missing，全部按当前源码补证。主仓同步后再请求索引刷新，实际结果在后续 service review 记录，不声称尚未完成的刷新成功。

本批四项收敛不等于“所有 service 无 bug”。随后继续邻接 review 与完成矩阵更新，旧历史容量/真实业务效果/HA/强杀/性能专项仍有具名边界。

## 后续（2026-10-01）

RR-20260929-27 的 `Page.Gap` 语义收口（[RR-20261001-08](RR-20261001-08.md)，复审 [§4.2](../review/REVIEW-2026-10-01-bline-audit-service-2.md) 探针实证）：本批 `pageOf` 除了页内 / 尾部洞，还把"本页到达 ring 头部且头部序号 > 1"当作洞，而容量淘汰永远从头部丢，于是任何溢出过的频道无游标取最新页、或 `BeforeSeq` 翻到保留边缘都 `Gap=true`，`history.gap.<kind>` 每次 +1——上文与 CHANGELOG 只写了"页内 / 尾部洞"，没写这条。已删去该条件：`Gap` 只在页内序号不连续、游标点名的消息已不在、尾部缺失时为真，普通容量淘汰后的最新页 `Gap=false`、不计指标；本批两条 `TestBugfix5` 回归不改、仍绿。
