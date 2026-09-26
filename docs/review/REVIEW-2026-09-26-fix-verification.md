# 2026-09-26 修复复验与继续审查（基线 `985d5ba`，对照 `aaada47`）

范围：核验 `985d5ba`（“修复 RR-03～24”）的每条修复是否成立（修前红 / 修后绿 / 原探针 / 验收覆盖点），复跑发布门禁与故障矩阵，
并继续审查业务链路如何穿过修过的边界。全部在 `git archive` 导出副本中进行；codebase-memory 图谱不可用或过期，结论来自源码与实跑。

**结论：`985d5ba` 仍不能发布。** RR-03～24 的修复方向大多成立，但修复引入了回归（RR-25～29），另有既有缺陷（RR-30～32）与若干复核残留。

## 新登记（均未修复）

| 编号 | 等级 | 结论 |
| --- | --- | --- |
| [RR-20260926-25](../bug/RR-20260926-25.md) | **P1** | 生成 sender / demo 从不走 Slow，冷实体上的正式业务（saga 扣减/补偿、GM）固定失败（RR-02/06 回归） |
| [RR-20260926-26](../bug/RR-20260926-26.md) | P2 | 快阶段不阻塞的冷缺失被改为 panic，业务不能按 `ErrColdLoadInLogic` 降级（RR-06 回归） |
| [RR-20260926-27](../bug/RR-20260926-27.md) | P2 | 快池断言 panic 在删除准入被当作不确定，整个进程被 fence（RR-06 回归） |
| [RR-20260926-28](../bug/RR-20260926-28.md) | P2 | Durability 0 从未提交的不确定结果永无结论，gate / 额度永久占用（RR-20 回归） |
| [RR-20260926-29](../bug/RR-20260926-29.md) | P2 | RR-17 后 kit/dataengine 6 个真实 Mongo 集成测试夹具失效，覆盖丢失 |
| [RR-20260926-30](../bug/RR-20260926-30.md) | P2 | saga 原生步骤本地 lease fence 过期跳过后内存保留效果，后续投影 fatal、重启起不来（既有） |
| [RR-20260926-31](../bug/RR-20260926-31.md) | P2 | demo 闲置交还先释放租约未等投影，跨进程读旧版本（既有） |
| [RR-20260926-32](../bug/RR-20260926-32.md) | P2 | 提交后 release hook panic 仍 Abort，是否已提交靠错误类型推断（RR-14 根因未修完，既有） |

复核残留（追加在原记录“复核残留”一节，不新编号）：RR-04（跨窗口重复编码）、RR-05（游标轮转越过未尝试会话）、RR-07（nestwal Committer 同类漏计）、
RR-10（fatal 后等待方等到截止；回归弱；性能未对照）、RR-11（tracker 终态被覆盖）、RR-12（未读段无告警、生成工程未断言 transport）、
RR-15（调用方配套与并发 Open 残留竞态）、RR-17（OnFatal 同步 Close 自等；回归弱；CHANGELOG）、RR-19（回归用空 commit；SAGA.md 缺说明）、
RR-22（负对照弱、CI 跳过真实 Mongo）、RR-24（v1.16.1 game-demo 升级仍编译失败；无别名 room import 丢别名）。复现附录：[REPRO-2026-09-26-03](../bug/REPRO-2026-09-26-03.md)。

## 修复复验摘要（红绿对照）

| RR | 结论 | 说明 |
| --- | --- | --- |
| 03、06、08、13、14、15、23 | 成立 | 06 修复方测试修前编译失败，以只用旧 API 的等价探针取得真正的红；13/14 回归偏弱，探针补证 |
| 07、09、16、17、18、19、22 | 成立（回归部分偏弱） | 17 的 Committer 用例修前即绿、删修复仍绿；19 用空 commit；22 负对照 2/30 |
| 10 | 成立 | 真实 Mongo：修前重启恢复 fatal、删档复活；修后 10/10、终态正确；删除屏障的负对照 5/5 失败 |
| 04、05、11、12、21、24 | 部分成立 | 见各 RR 复核残留；21 能力转发成立，正式装配仅 60s/100TPS 短测 |
| 20 | 分类方向成立，引入 RR-28 | |

证据可复跑性：`985d5ba` 的 bugfix 记录中“修前失败”文本只存在于本机 `/tmp/roost-release-*.log` / `/tmp/roost-review-*.log`，
负对照基线为“381efc9 + 未提交的 RR-03～09”（不对应任何提交），违反 roost-coding “不依赖某台机器 /tmp”。

## 门禁与故障矩阵（985d5ba 导出副本）

- 通过：`go build` / `go vet` / `go vet -tags integration` / `go test -race ./...`（120 包）/ glsvet / gofmt（新增修改的 59 个文件）/ tidy；
  FatalSuffix `-race -count=500` 0 失败；Windows 交叉编译通过（未实机）；sync-modes / dataengine / remote 生成脚本；非故障集成用例；
  生成工程 demo 与 full lane；demo 机器人 3 轮 6/6；syncbus 实际 `transport=jetstream`。
- 失败：kit/dataengine 6 个集成测试（RR-29）。
- 故障矩阵 `scripts/test-remote-matrix.sh`（985d5ba 临时 worktree，本地 roost-it）：**20/21 PASS**——12 格 Remote 业务故障（Mongo 主/多数派、NATS 单节点/全停 × 三策略）、
  租约进程、Redis Cluster 故障切换与未复制 fence、持久权威分区、所有权计数、broker 故障与网络、最终健康；唯一 FAIL `mongo-wal-recovery` 即 RR-29 夹具问题。
- 发版手续未做：`framework-release.yaml` 仍 v1.16.1、生成器下限 `manifest.go:52` / `framework-compat.yml:50` 仍 v1.16.0；CHANGELOG 缺 8ce21a5、a22c5a5、RR-03～09 条目、
  985d5ba 的 3 处兼容变化与 v1.17.0 标题。

## 疑点（未登记）

- 冷登录等待投影无上限：`EnterGame` 在 TCP goroutine 上以无截止的 `connectionCtx` 调 `GetOrCreate`，投影卡住时登录挂起并占连接槽。
- RR-08：已入队未运行的续行不计入准入。RR-13：回复中无“已提交”哨兵，调用方无法区分“已提交但释放失败”与“未提交”。
- RR-16：每次 Ack 可能多一次 fsync（持 stateMu），async 吞吐未做同口径对照。
- Remote Abort 总走 RunLocal，Prepare 阶段失败也占一次快池续行；无 Guard scope 时 `GetEntityGuard` 取出不归还。
- RR-19：Admitted 回源后 finalizer 与投影器可能并发发布同一事务；自定义 remoteStore 回放历史混合记录的硬性要求偏严。
- 生成工程 DAO 库名写死 `"game"`；demo scene `sessionLost` / Leave 不检查 ActiveSessions，重连期间可能把新连接的玩家移出场景。

## 环境副作用

本轮全部使用隔离库 / 前缀并清理；未写共享 `game` 库。故障矩阵按脚本对本地 roost-it 注入故障并 heal，结束时 `final-health` PASS。

## 修复后门禁（2026-09-26，`b8d985d` 临时 worktree，本地 roost-it）

全部 PASS：`go build ./...`、`go vet ./...`、`go vet -tags integration ./...`、`go test -race -count=1 ./...`、`glsvet ./...`、gofmt（aaada47 以来改动的 .go 文件）、
`scripts/test-sync-modes-generated.sh`、`scripts/test-dataengine-generated.sh`、`scripts/test-remote-generated.sh`、
`go test -race -tags integration -p 1 ./dataengine/... ./nestwal/... ./kit/dataengine/... ./kit/syncbus/...`（跳过 Failover/Toxic/Outage，由矩阵覆盖）。

故障矩阵 `scripts/test-remote-matrix.sh`：**21/21 PASS**（985d5ba 时唯一失败的 `mongo-wal-recovery` 已由 RR-29 修复）：

```text
business-mongo-primary-async	PASS
business-mongo-primary-strict	PASS
business-mongo-primary-pipelined	PASS
business-mongo-majority-async	PASS
business-mongo-majority-strict	PASS
business-mongo-majority-pipelined	PASS
business-nats-node-async	PASS
business-nats-node-strict	PASS
business-nats-node-pipelined	PASS
business-nats-all-async	PASS
business-nats-all-strict	PASS
business-nats-all-pipelined	PASS
lease-process	PASS
redis-cluster	PASS
redis-unreplicated-fence	PASS
durable-process	PASS
ownership-counters	PASS
mongo-wal-recovery	PASS
broker-failover	PASS
broker-network	PASS
final-health	PASS
```

未做：Windows 实机（windows-compatibility 待 CI）、24 小时长稳、正式 kit 装配下 Remote 容量阶梯复测（RR-21）、RR-16 / RR-10 性能对照、1000 玩家负载下 RR-25 尾延迟复测。
RR-20260926-30 未修复（需维护者拍板）。
