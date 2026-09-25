# DataEngine 恢复与正式生成链路验收

后续真实业务压测及收尾判断见 [DataEngine 压力报告](DATAENGINE-PRESSURE-2026-09-24.md)；本文保留正确性与恢复场景证据。

日期：2026-09-24。基线：`967bc69` 加已实施的 DataEngine/Sync/Nest 工作树。Go 1.27.0，darwin/arm64，Apple M5。对应 [D2～D4 方案](REFACTOR-2026-09-24-dataengine.md)。本批保留原包结构，唯一运行代码修复是 [RR-13 慢发布退避](../bugfix/RR-20260924-13.md)，其余为有实际输入输出断言的恢复夹具与既有故障测试执行。

## 1. 100k WAL 积压恢复

入口：`kit/dataengine/backlog_integration_test.go` 的 `TestRealDataEngineLargeBacklogRecovery`，需显式启用 `ROOST_DATAENGINE_IT_BACKLOG=1`。

- 100,000 条记录，10,000 个实体，每实体 10 个连续版本；单 DAO、普通 Put/Patch，不包含 Remote、receipt 或 outbox。
- 文件 WAL v2，1 MiB segment / 64 KiB 单记录上限，实测 **20 个文件段**；按 WAL 顺序 enqueue，每窗口最多 256 张持久化票据，再等待全部 durable。预填阶段约 2.76 秒。
- 关闭 WAL 后重开，以通道让首条投影停在存储调用边界。确认调用已进入后，20ms Flush deadline 必须返回；关闭并再重开后，100,000 条仍全部可重放。这里是受控存储延迟，不是向 Mongo 服务器注入延迟。
- 用真实 MongoStore 批量恢复，断言 10,000 份 DAO 全部为版本 10，抽查首/中/末实体最终 payload；WAL pending=0，再关闭重开仍 pending=0。

最终结果（非 race，单次验收）：**11.283 秒恢复 100,000 条，约 8,863 条/秒，391 次 checkpoint ack**。首次通过为 11.152 秒 / 8,967 条/秒；两次未进行统计显著性分析，不宣称性能提升。第二次补充了跨段数量的显式断言。

该数据是单 DAO 普通记录的恢复速率，不是 Nest 在线吞吐、严格事务吞吐、MMO 容量或 50ms 延迟验收。带副作用事务的分段/ack 语义沿用已有基准，未修改合并策略。

## 2. 真实依赖故障矩阵

使用专用 `/tmp/roost-dataengine-it`（macOS 实路径 `/private/tmp/roost-dataengine-it`），Mongo 27117～27119、NATS 14222～14224、toxiproxy 18474 / NATS 24222～24224。故障脚本验证 PID 命令行属于这个固定目录，数据库均为测试专用名称。

| 用例 | 实际断言 | 结果 |
| --- | --- | --- |
| Mongo primary 停止 | 选主后继续投影，同实体版本从 1 到 2、payload 正确 | race 通过 |
| NATS 全部停止 | Mongo 投影继续，outbox 保留；恢复后 effect 交付并清空 | race 通过 |
| JetStream leader 停止 | 同 MsgID 返回原 sequence，下一条 sequence 增长，消费顺序正确 | race 通过 |
| NATS downstream 3 秒延迟 | WAL+Mongo 完成不被 Broker 延迟阻塞，恢复后交付 | race 通过 |
| NATS 连接 reset | 投影成功、outbox 保留，网络恢复后交付 | race 通过 |
| NATS 半开/ACK 丢失 | 发布有界返回失败；恢复后 MsgID 去重，观测窗口内没有重复交付 | race 通过 |

前三项合计 29.623s，后三项合计 16.044s。有限的去重窗口用例通过不等于永久 exactly-once 保证。测试后恢复集群健康并清除网络故障；测试中产生的旧 WAL 证据没有改写。

## 3. 正式生成 DAO → Nest → DataEngine → 三次独立进程

入口 `scripts/test-dataengine-generated.sh`；源码夹具在 `codegen/internal/entity/testdata/dataengine/`。运行正式 DAO 与 Entity 生成器，再编译一个带 race 的测试二进制，启动 **三个独立 OS 进程**，共同使用本次独立测试数据库和每种策略自己的 WAL 目录。这是正式 API 的最小持久化业务夹具，不是示例游戏，也不等同于完整游戏部署进程。

每种 async/strict/pipelined 策略分别有 100 个实体，8 个并发请求方：

1. 进程 1：创建实体，每个实体用生成 DAO setter 修改 10 次，合计 1,000 条业务消息；Nest 构建真实事务，Projector 写入文件 WAL 并投影 Mongo。
2. 进程 2：通过 Runtime.Repository 从 Mongo 加载，验证字段值和 Tracker.Version 都为 10；再执行 1,000 条消息，达到 20。
3. 进程 3：重新构建 Runtime/Nest，从 Mongo 加载并确认值/版本为 20。

每个进程、每种策略均追加一个先修改字段再拒绝业务的 handler，验证返回原错误、内存值/版本恢复、Mongo 未改变、WAL 无残留。合计 **6,000 条成功业务消息 + 9 次回滚**。pipelined 使用正式 allowlist、异步完成与 durable ticket；其失败 handler 也在 allowlist，确保实际进入事务回滚。

最终三个进程均通过（6.68s / 9.70s / 3.19s），测试库清理也通过。带 race 的 Request 完成延迟如下；它包含磁盘和运行时诊断开销，是正确性夹具的观测值，不是吞吐基准：

| 策略 | 进程 1 完成消息/s | p95 / p99 | 进程 2 完成消息/s | p95 / p99 |
| --- | ---: | --- | ---: | --- |
| async | 571 | 30.40 / 40.83 ms | 568 | 30.05 / 35.66 ms |
| strict | 409 | 40.26 / 57.52 ms | 400 | 41.00 / 56.14 ms |
| pipelined | 515 | 23.02 / 26.07 ms | 496 | 23.88 / 27.08 ms |

这些采样止于业务 Request 完成，最终 Mongo 可见性另由 Flush 和逐文档检查确认；不能将 async 的返回延迟视为 Mongo 已落库的延迟。

没有 effect 的工作负载使用拒绝任何意外 effect 的 publisher；Broker 真实故障由上一节独立覆盖。没有把 Sync 位置变化等同于持久化写入，也未给实际 MMO 强加落库比例。

## 4. 复跑

先确认隔离环境可用，所有故障测试串行执行，不与其他使用同一测试集群的工作并发。

```sh
bash kit/scripts/integration/dataengine-env.sh up
source /tmp/roost-dataengine-it/env.sh
export GOCACHE=/tmp/roost-nest-go-cache GOWORK=off

ROOST_DATAENGINE_IT_BACKLOG=1 go test -tags=integration ./kit/dataengine \
  -run '^TestRealDataEngineLargeBacklogRecovery$' -count=1 -timeout=240s -v

go test -race -tags=integration ./kit/dataengine \
  -run '^TestReal(MongoPrimaryFailover|NATSOutage|JetStreamLeaderFailover)' \
  -count=1 -timeout=240s -v

ROOST_IT_TOXIPROXY=1 go test -race -tags=integration ./kit/dataengine \
  -run '^TestToxicNATS' -count=1 -timeout=180s -v

bash scripts/test-dataengine-generated.sh
```

原始证据位于 `/tmp/roost-dataengine-recovery/`：`backlog-final.log`、`failover.log`、`toxic.log`、`generated-final.log`、`regression.log`、`retry-red.log`、`retry-green.log`、`heal-restored.log`。临时日志不是仓库的长期附件，本文件保存复跑入口、关键结果和测量口径。

## 5. 验收边界

8 包 race 回归通过：dataengine、engine、nestwal、remoteentity、entity、nest、kit/dataengine、kit/nest。vet、core build、脚本语法检查通过；build 有沙箱不允许写模块版本 stat cache 的提示，但退出码为 0。最终夹具的生成编译与三进程检查单独验收。

本轮补齐了上轮尚未执行的 100k 积压、存储调用阻塞时取消、Broker 故障恢复，以及正式生成 DAO/Nest 的进程重建。没有重现磁盘断电/fsync 错误、任意时刻 SIGKILL、长时间生产负载，也没有对全仓作无遗漏审计。上述属于剩余部署/故障扩展范围，不标作已验证；历史冲突 WAL 的业务数据修复也不在这里自动执行。


图谱采用 Verify：起始代际 `2026-09-24T09:09:45Z`，结束刷新至 `2026-09-24T10:14:59Z`（18,016 节点 / 142,123 边）。相关搜索均已处理所用范围分页；宽泛的 Entity 查询改为精确 manager_access 文件查询，不作全仓穷尽结论。retryDelay 双向一跳确认 RunOnce 调用；同名方法和 heuristic 边以当前源文件校正。修改运行代码和新增重放/重试测试已是 metadata_match/no_recorded_issue，engine scope 无记录缺口及剩余分页。testdata/scripts/docs 被规则排除，已直接读取并实际生成、编译、运行补证；索引干净不是完整性证明。755 个本地文档链接目标和 diff 空白检查通过。

## 6. 多实体、多 DAO 的正式业务验收

按用户要求，本批扩展本地 Entity 事务，Remote 后续另行验证。沿用第 3 节脚本、真实生成器、文件 WAL 和隔离 Mongo 副本集；未修改运行逻辑、包结构或提交协议，也没有新增已确认 bug。

夹具新增 `Trader`，每个实体挂载生成的 `WalletDao` 和 `InventoryDao`，分别落到 `trade_wallets`、`trade_inventories` 两个集合。交易通过正式 `Nest.RequestMulti` 修改双方的钱包与背包：付款方减 3 金币、加 1 物品，收款方反向变化，同时四份 DAO 各增加交易计数。setter 负责标脏，Nest 在业务完成后收集为一条包含四个 mutation 的提交记录，再由 DataEngine 投影为 Mongo 原子事务。

| 场景 | 输入与断言 | 结果 |
| --- | --- | --- |
| 共享实体并发交易 | 8 个实体、每实体 2 个 DAO，8 个请求方沿环交易，相邻交易共享实体，末尾请求 ID 顺序与锁排序相反；每个写入进程每种策略执行 96 笔交易，逐 DAO 检查值、交易计数与版本 | 通过 |
| 单实体多 DAO | 每个写入进程每种策略追加 1 次购买，同时扣钱包、加背包；恢复后两份文档与内存一致 | 通过 |
| 业务拒绝与 panic | 四份 DAO 全部修改后返回错误或 panic；检查内存字段/版本回滚、Mongo 不变、Committed 不增加、WAL 无残留 | 通过 |
| 跨进程加载与续写 | 进程 1 写入，进程 2 从 Mongo 加载并继续交易，进程 3 再加载核对；每一步检查全部实体的两份 DAO | 通过 |
| Mongo 已提交、WAL ack 丢失 | 在真实 Mongo 成功后让 ack 返回错误，关闭并重开确认 WAL 保留原事务；下一 OS 进程启动重放恰好 1 条，四份 DAO 无重复扣款/增量，继续交易成功，第三进程再检查 | 通过 |
| 最后一份文档冲突 | 投影进入真实 Mongo 前把排序最后一份钱包文档版本从 1 改到 999；投影失败后四份完整 BSON 文档与注入后的快照一致、事务 marker 不存在、后续提交被 fatal 拒绝、原 WAL 保留 | 通过 |

以上逐项覆盖 async、strict、pipelined。常规交易合计 **576 笔四文档交易 + 6 笔单实体双 DAO 购买 + 18 次错误/panic 回滚检查**，不含初始化和故障场景各自的交易。ack 丢失与末文档冲突各覆盖 3 种策略。

故障夹具只用通道控制下一次进入 MongoStore 的时刻，用既有 `OverrideAck` 注入 ack 错误；事务记录来自正式 Nest，实际写入与原子回滚由 Mongo 完成。ack 注入模拟已提交未确认窗口，**不等同于任意时刻强杀或断电**。末文档冲突场景只检查 Mongo 原子性和 fatal/WAL 保留；已接受的内存事务不会因后台投影冲突自动撤回，故障处理仍需以保留的 WAL 和数据库状态为依据。

新增多实体测试三个进程分别耗时 **8.57s / 7.43s / 0.41s**，全部带 race 通过；旧单 DAO 6,000 条业务消息场景也在同次脚本中通过，最后测试库清理成功。这是事务与恢复正确性验收，包含共享实体锁竞争和故障控制，不作为吞吐基准或 MMO 容量数据。

实现入口：

- [DAO 定义](../../codegen/internal/entity/testdata/dataengine/def/trade.go)、[实体定义](../../codegen/internal/entity/testdata/dataengine/trader.go)。
- [正式运行时与业务处理器](../../codegen/internal/entity/testdata/dataengine/trade_fixture_test.go)。
- [多实体验收用例](../../codegen/internal/entity/testdata/dataengine/multi_entity_test.go)。
- [三进程执行脚本](../../scripts/test-dataengine-generated.sh)：同时执行旧单 DAO 与新增多 DAO 用例。

仅复跑本轮业务验收，无需运行 Remote 或 Broker 故障矩阵：

```sh
source /tmp/roost-dataengine-it/env.sh
GOCACHE=/tmp/roost-nest-go-cache bash scripts/test-dataengine-generated.sh
```

相关 7 包 `go test -race ./nest ./dataengine/... ./entity ./kit/dataengine ./codegen/internal/dao ./codegen/internal/entity -count=1 -timeout=120s` 通过。本轮原始日志为 `/tmp/roost-dataengine-multi/generated.log`、`regression.log`。图谱沿用 `2026-09-24T10:14:59Z`；调用链以图谱定位、源码复核，新增 testdata 和脚本被索引规则排除，已直接审阅并通过真实生成、编译与执行补证。
