# Remote 故障矩阵与生成工程验收：v1.31.0 发布后补跑（2026-10-29）

v1.31.0 发布验收 V10（故障矩阵）与生成工程 Remote 验收没有执行（见 [v1.31.0-VALIDATION](../../../release/v1.31.0-VALIDATION.md#v10-addendum)）。本目录是发布后补跑的结果摘要。
这是本机单机证据，不扩大为跨机或生产保证。

- 被测代码：main `5c85223c`（`env-head.txt`）。与 tag `v1.31.0` 相比，`framework/remoteentity`、`framework/nest`、`framework/dataengine`、`wiring/dataengine`、`infra/storage`、`codegen/internal/entity` 与矩阵脚本逐字相同；之后的代码改动（saga JetStream 日志、chunksync / physicsroom）不在 Remote 链路上。
  运行时工作树另有 `scripts/perf/remote.sh` 缺省时长与 `scripts/README.md` 的未提交改动，矩阵不调用它们。
- 环境：Apple M5、macOS 26.5 / arm64，`go1.27.3`；共享隔离环境 `~/.roost-it/roost-dataengine-it`（Mongo 三节点副本集、NATS JetStream 三节点、Redis、toxiproxy；只 source env，不打印）；
  Redis Cluster 两格由用例从 PATH 自起 3 主 3 从。`remote-acceptance.lock` 开始前空闲，由脚本取得与释放。
- 文件：`results-r1.tsv` / `results-r2.tsv` 是两次完整运行的逐格结果（脚本原样输出）；`env-head.txt` 是 `env.txt` 的前两行（Go 版本与提交），两次运行的源码 sha256 清单逐行相同。
  完整日志在忽略目录 `artifacts/perf/remote/matrix-post-v1310-20261029{,-r2}/`（运行所在的 worktree `roost-core-wt-remote`，删除 worktree 前转存；生成工程验收日志为 `generated-functional.log`）。

## 结果

| 运行 | 命令 | 结果 | 墙钟 |
| --- | --- | --- | --- |
| 生成工程功能验收 | 持锁后 `bash scripts/test-remote-generated.sh`（功能模式，`^TestGeneratedRemote`） | 退出 0：15 个顶层用例 PASS，3 个按设计 SKIP（`TestGeneratedRemoteMirrorLocal` 要 `mirror-local.sh` 私有环境；`TestGeneratedRemoteMirrorLocalProcess`、`TestGeneratedRemoteMirrorReaderProcess` 是子进程入口，父用例 PASS） | 36s |
| 矩阵第 1 次 | `ROOST_REMOTE_MATRIX_LABEL=matrix-post-v1310-20261029 bash scripts/test-remote-matrix.sh` | **21/21 PASS**，退出 0 | 265s |
| 矩阵第 2 次 | 同上，标签 `…-r2` | **21/21 PASS**，退出 0 | 250s |

矩阵没有时长旋钮，也不需要：业务格每格 6～14s（`ROOST_REMOTE_TIMEOUT=240s` 只是上限），整轮约 4.5 分钟。

逐格核对（第 1 次；第 2 次结果相同）：

- 12 个业务格（`business-{mongo-primary,mongo-majority,nats-node,nats-all}-{async,strict,pipelined}`）：每格 `TestGeneratedRemoteNestFlow/<策略>` PASS，日志里确有注入与恢复
  （mongo-primary 停当前主；mongo-majority 停两个节点；nats-node 停 nats-1；nats-all 停全部三个），`flow_test.go:448` 报“2 entities x 2 DAOs, rejection/panic rollback,
  all Mongo versions/values and 4 snapshots verified”；NATS 两种故障下 `authoritative_refill=true`。
- `lease-process`：`TestRealRemote{ProcessKillRecoversLease,ProcessPartitionFencesOldOwner,ColdAdmissionDeadlineUnderLatency}`。
- `redis-cluster` / `redis-unreplicated-fence`：`TestRealRemoteRedisClusterFailover`、`TestRealRemoteRedisClusterUnreplicatedFence`。
- `durable-process`：`TestRealRemoteDurableAuthorityProcessPartition`。
- `ownership-counters`：`TestRealRemoteOwnershipClaimAndTransfer` 与 `TestRealVersionedLock*` 10 个（13 条 PASS 含 2 个子用例）。
- `mongo-wal-recovery`：`TestRealDataEngineRemotePublicationAndWALRecovery` 及子用例 `published`、`stop-interrupts-publication`。
- `broker-failover`：`TestRealMongoPrimaryFailoverContinuesProjection`、`TestRealNATSOutageDoesNotBlockProjectionAndRecoversOutbox`、`TestRealJetStreamLeaderFailoverPreservesDedupAndOrder`。
- `broker-network`：`TestToxicNATS{LatencyKeepsTheCommitOnTheDurablePath,ConnectionResetDeliversTheEffectExactlyOnce,HalfOpenAckLossIsBoundedAndDeliversExactlyOnce}`。
- `final-health`：Mongo 一主两从、NATS 三节点 JetStream、Redis 与 toxiproxy 运行。

所有格都是 `-race`；日志里没有 `DATA RACE`、`panic:`、`--- SKIP`、`no tests to run`。

## 覆盖边界

- 这些格覆盖 v1.31.0 之后 Remote 链路改动（投影器按实体并行、删除 lease fence 的 Remote 分支、DataEngine 启动排空有界、Redis `EvalDurable` / DistLock / `IScript` 删除、
  Nest 按 handler 异步完成与同实体按提交序、remote-entity 停止用例合并）经过的正式路径，但业务格只有 2 个实体，投影器按实体并行只是被走到，不是被压到；
  启动排空的 60s 上限没有被这些故障触发（故障都在几秒内恢复）。负载下的 Remote 回归仍要靠 `scripts/perf/remote.sh`，本次没有跑。
- 跨机、Linux、掉电与磁盘故障不由这些用例证明（[EXTERNAL-VERIFICATION](../../../maintenance/EXTERNAL-VERIFICATION.md) E01～E28）。

## 收尾核对

运行前后只读快照一致：共享 Mongo 只有 `admin` / `config` / `game` / `local`；Redis 17379 的键数 11、键名清单不变；JetStream 流 0 个；toxiproxy 只有常驻 `nats-1..3`、`redis` 代理；
锁目录不存在；没有残留的测试进程。
