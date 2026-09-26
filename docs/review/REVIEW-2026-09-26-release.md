# 2026-09-26 上线前复审运行记录（v1.16.1 → main `aaada47`）

**后续更新**：RR-10～24 已实施修复，验证与尚未覆盖的发布/环境边界见[修复记录](REVIEW-2026-09-26-release-fixes.md)。下文保留原始基线结论。

范围：上一轮（[核心优化复审](REVIEW-2026-09-26-core-optimization.md)）未覆盖的链路 + 发布门禁：Nest 事务收尾与 Sync 交付、DataEngine / Remote 持久化与生命周期、
Remote 持久权威、生成 demo 冒烟、CI、API 兼容与发版手续。全部在 `git archive aaada47` 导出副本上进行；仓库工作树中另一会话的未提交修复不在本轮基线内。
codebase-memory 图谱 generation 早于基线，结论来自源码、临时探针与实跑。

**结论：`aaada47` 不能直接发布。** 本地代码门禁全部通过；阻塞项是 3 个 P1 级产品缺陷、RR-20260926-03（相对 v1.16.1 的新回归）、发版手续与 CI。

## 登记的 RR（均未修复）

| 编号 | 等级 | 模块 | 结论 |
| --- | --- | --- | --- |
| [RR-20260926-10](../bug/RR-20260926-10.md) | **P1** | DataEngine | 卸载后在投影完成前重载读 Mongo 旧版本，后续事务投影冲突，进程 fence 且重启无法恢复（v1.16.1 已有） |
| [RR-20260926-11](../bug/RR-20260926-11.md) | **P1** | Remote | 已提交事务发布失败后重放被同实体新 fence 拒绝，WAL 投影永久卡住 |
| [RR-20260926-12](../bug/RR-20260926-12.md) | **P1（建议）** | kit / codegen | 生成的 `syncbus:` 配置段被整段忽略，JetStream 静默退回普通 NATS（0e1b469 引入） |
| [RR-20260926-13](../bug/RR-20260926-13.md) | P2 | Nest / Remote | 提交成功但释放失败时 postRemoteCommit 被跳过，Sync 永久冻结、AfterCommit 丢失 |
| [RR-20260926-14](../bug/RR-20260926-14.md) | P2 | Nest / Remote | 持久提交后 AfterCommit 失败仍 Abort，远端实体在锁外回滚，与持久层分叉 |
| [RR-20260926-15](../bug/RR-20260926-15.md) | P2 | Sync | 同 SessionID 关闭后立即重开，新会话被传输层静默丢弃（若重连复用 ID 建议上线前修） |
| [RR-20260926-16](../bug/RR-20260926-16.md) | P2 | nestwal | async 下 checkpoint 先于段 fsync，断电后 WAL 拒绝打开 |
| [RR-20260926-17](../bug/RR-20260926-17.md) | P2 | DataEngine / nestwal | 停机不等待外部 Flush / ReplayPass，旧回放在交出 WAL 后回退 checkpoint |
| [RR-20260926-18](../bug/RR-20260926-18.md) | P2 | nestwal | `OpenRuntime` 零值 `CloseWAL` 时 Shutdown 不释放自己打开的 WAL |
| [RR-20260926-19](../bug/RR-20260926-19.md) | P2 | Remote | 租约 fence 失效跳过记录后 Remote 事务悬挂，gate / 锁 / 额度永久占用 |
| [RR-20260926-20](../bug/RR-20260926-20.md) | P2 | Remote | Durability 0 提交结果未知时按失败回滚并立即释放权限 |
| [RR-20260926-21](../bug/RR-20260926-21.md) | P2 | Remote / kit | 正式装配下并行投影未开启；Remote 性能验收未在正式装配上测得 |
| [RR-20260926-22](../bug/RR-20260926-22.md) | P2 | demo 模板 | 公会持久发号 upsert 含范围谓词，首次并发 duplicate key（CI demo lane 间歇红） |
| [RR-20260926-23](../bug/RR-20260926-23.md) | P2 | 测试 | FatalSuffix 测试竞态（race 24%）+ Windows 5 个“经过时间 > 0”断言 |
| [RR-20260926-24](../bug/RR-20260926-24.md) | P2 | codegen 迁移 | 迁移表缺 `room` / `kit/room` 映射，旧工程升级后编译失败 |

复现附录：[REPRO-2026-09-26-02](../bug/REPRO-2026-09-26-02.md)（仍存源码的四个探针）；其余探针输出摘录在各 RR 正文。

## 发布门禁（实跑，导出副本）

| 项目 | 结果 |
| --- | --- |
| `GOWORK=off go build ./...` / `go vet ./...` / `go vet -tags integration ./...` | 通过 |
| `go test -race -count=1 ./...` | 通过：119 包 ok，26 包无测试 |
| glsvet（四个核心包与 `./...`） | 通过 |
| `go mod tidy` 比对 / `go mod verify` | 通过；go.mod 与 v1.16.1 相同 |
| actionlint、govulncheck | 通过；govulncheck 无可达漏洞（klauspost/compress、x/crypto 有未调用到的公告） |
| `scripts/test-sync-modes-generated.sh` | 通过 |
| `scripts/test-dataengine-generated.sh`、`scripts/test-remote-generated.sh`（本地 roost-it） | 通过（三种策略） |
| `go test -tags integration -p 1 ./dataengine/... ./remoteentity/... ./nestwal/...` | 通过；Redis Cluster 两例需 `ROOST_REMOTE_CLUSTER_IT=1`，跳过 |
| `gofmt -l .` | 33 个文件未格式化（v1.16.1 为 53；新增 1 个）；CI 不检查 |
| `scripts/pretag.sh v1.17.0`（清单原样） | **失败**：`framework-release.yaml says release: v1.16.1` |
| `scripts/pretag.sh v1.17.0`（仅副本内把清单改为 v1.17.0） | 通过，`ready to tag` |
| CI `ci` | 646ce63 起每次 windows-compatibility 红；a22c5a5 另有 linux-test(1) 红（RR-23） |
| CI `framework-compat` | abfc421 起红：minimum / released lane 因生成物 import 未发布包（发版并提高下限后消除）；source-head demo 间歇红（RR-22） |
| nightly（fault-matrix 等） | 646ce63 之后未运行 |

## 发版手续（未完成，阻塞打 tag）

- `codegen/ci/framework-release.yaml:5` 仍为 `release: v1.16.1`。
- 生成器下限 `codegen/internal/roost/manifest.go:52`（`Core: v1.16.0`）与 `.github/workflows/framework-compat.yml:50`（`-roost-core-version v1.16.0`）须同改为 v1.17.0
  （`deploy_hygiene_test.go:109` 逐字比对二者）：当前生成物已 import v1.16.1 不存在的 `sync/*`、`kit/syncbus`。
- 版本号建议 **v1.17.0**：相对 v1.16.1 有大量破坏性变化（statesync / room / entitysync / nettransport / lockstep / syncbus / mirror / kit/room 包删除或搬迁，
  AsyncTransport datagram 删除，`SubjectSyncState.CaptureSnapshot` 删除，`EntitySyncBuilderParam/CreateParam.Topic` → `Namespace`，
  `remoteentity.NewVersionedLockFactory` 新增 `...WriteAuthority`，robot 等签名随类型迁移）；严格 SemVer 应为 v2，但 v2 需模块路径 `/v2` 并重写全部 import，
  仓库惯例（v1.16.0 合仓）与迁移表 `layout.boundary.core: v1.17.0` 均按小版本处理。apidiff 明细在审查副本中，未入库。
- CHANGELOG `[Unreleased]`：8ce21a5、a22c5a5 未写入；文件头两段 “2026-09-25 Nest 双池…” 位于 `[Unreleased]` 之外；缺上面列出的 4 条破坏性说明与“本版破坏性”总述、无版本标题。

## 疑点（未登记 RR）

- Remote strict 确认超时后 Sync 屏障永不解除：finalizer 事后判定已提交也不回来 Confirm，Nest 在该路径不 Fence（`remoteentity/batch.go:457-460`）。需裁定 Fence 还是由 finalizer 放行。
- pipelined 阶段一 / 完成队列满降级时，快 worker 在锁外 `<-ticket.Done()` 等 fsync（`nest/execution.go:256,266`）：与“快池内不得阻塞等待”冲突，RR-20260926-06 豁免清单需明确。
- Broadcast 每实体的 Release / Confirm 在整个循环结束后才执行（`nest/execution.go:103` 等），首个实体同步被推迟（延迟，不影响正确性）。
- handler 内 `CreateInScope` 新建实体不在 SyncMutation 提交边界内；kit 未接 `DurableWatermark`（唯一入口 `nestwal/runtime.go:40`）。
- `kit/nest` 停机超时后 entitySync 不 Stop / Drain / Close（`kit/nest/nest_mod.go:156-158`）；`kit/syncbus` Provide 后 Register 失败 JetStream 不清理（`kit/syncbus/mod.go:98`）。
- DataEngine：断电后零填充尾部不被容忍（`nestwal/wal.go:1218-1226`）；重启后积压不计入 `WALUnacked`（生产链路 Start 先排空）；段切换失败后的 Replay 错误信息；Stats 回放与 prune 并发。
- 生命周期：关闭窗口内 `CommitSystem` 孤儿票据；fatal 后已登记票据等到 Close；`dataengine.shutdown_timeout` 在 app `StopWithContext` 路径不生效（用 app 均分截止时间）；
  WAL terminal 后 Shutdown 永远报错致 Assembly 无法进程内重启；Remote 停机按 NextVersion 解锁（durable 路径不受影响）。
- Remote：sid 作用域 DAO 用于 Remote 实体时提交与加载选库不一致（fail-closed）；事务内 DuplicateKey 后继续的分支仅伪 Mongo 覆盖；versionedLock 续期重启竞态；tracker 终态被 Indeterminate 覆盖。
- 生成工程写死库名 `"game"`（DAO 与 demo 发号器集合）。

## 已核对通过（摘要）

- Nest：on_change 冻结在 Admit / 持锁 commit 内、网络发送在锁外、Release+Confirm 齐后才唤醒；回滚丢弃屏障与掩码；动态 Cast 先 Include 再 Capture；完成泵顺序与 Shutdown 排空。
- Sync：AsyncTransport 字节额度各路径只归还一次、无 latest-only、会话失败终态；Flush 全部帧准入才结算、RetryLater 强制全量；kit 配置负值校验与启动失败回滚。
- DataEngine：WAL LSN / 物理顺序、容量预留、切段前 fsync、checkpoint 双槽代际、prune 保留、撕裂尾截断；Projector held 即停、只 ack 连续前缀、冲突 fatal 不跳过、
  多 DAO marker 幂等；完成语义三种级别正确；`MaxUnackedRecords` 原子预留；启动屏障先 Flush 再 Ready；19 段跨段探针与 100k 积压跨 20 段恢复通过；RR-09/10/11/19 回归保持。
- Remote：提交 CAS 条件完整（`_ver` 精确相等、epoch、route、fence、token）；grant `$inc` 单调；GrantWrite 未知结果只按唯一 token 读回；多实体多 DAO 同事务；digest 完整；
  回执快读 majority；锁 token 代际隔离；outbox lease CAS、至少一次；超时不当作未提交。

## 没有做的

- `scripts/test-remote-matrix.sh`、kit/dataengine 故障注入用例、Redis Cluster 集成、24 小时长稳、任何压测；RR-21 要求的正式装配 Remote 性能复测。
- 审查中对共享本地环境的副作用：一轮 demo 复现未隔离库名，推进了 itest 环境共享 `game` 库中 6 个 player 与 1 个 world 文档的版本（属另一会话的测试数据，无法回滚），
  并可能向共享 `ROOST_SYNC` 流写入少量消息；其余自建库 / 键 / 流已清理。
