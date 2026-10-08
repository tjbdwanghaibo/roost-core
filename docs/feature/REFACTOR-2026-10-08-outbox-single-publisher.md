# Remote outbox 唯一发布入口与历史收口

维护者 2026-10-08 授权：先补 E14 精确崩溃测试，再收敛唯一发布者；集群故障仍在本机私有环境验证，负载长稳用 1 小时，暂不验证部署，历史项逐项收敛。基线 `88fc852c`，分支 `codex/outbox-closure`，不派子 agent。

## 当前问题与目标

`ApplyRemoteCommits`、finalizer 和 outbox 循环都能发布。同一持久提交有三个重试拥有者，近期修复链为 RR-20260926-11/37/38/61/63、RR-20261006-69。根因是责任分散；本轮减少入口，不再增加第四种补发策略。

目录保持原样：`remoteentity/transaction_manager.go` 负责持久提交、outbox 和延迟收尾，`transaction_tracking.go` 负责等待，`batch.go` 负责写批次；不新建包或通用队列框架。新增测试按职责放在同包，私有故障环境复用现有脚本。

## 发布与确认契约

1. `ApplyRemoteCommits` 校验并持久提交，取得持久状态后登记 Applied、唤醒 outbox；投影器不等待网络发布。已 Committed 的幂等重放只确认本地参与方，不重新发布。
2. `RecoverOutbox` 是唯一发布入口，启动恢复和后台循环共用，并发扫描串行且等待可取消。只有这里执行快照发布和 `MarkRemoteCommitPublished`。
3. 第一次唤醒立即扫描；失败按既有有界退避重试；循环中的新唤醒记 dirty，保证扫描结束边界不丢通知。持久 outbox 是恢复事实，内存通知不是唯一记录。
4. finalizer 看到 Applied 只唤醒 outbox 并重试等待；看到 Committed 只做本地确认、释放 gate/fence/写额度及提交后回调；不发快照。保留持久拒绝后的隔离、卸载与回调契约。
5. memory 模式仍等发布结论再成功回复；strict/pipelined 继续等 tracker。async 只承诺 WAL 准入，保持原有语义。等待超时是结果未知，不回滚可能已经持久的数据。
6. 发布失败与投影成功独立；停机取消并等待发布循环，不能让回调在释放 SnapshotClient 后继续运行。跨进程仍允许对同一 outbox 幂等补发；“唯一入口”不冒称分布式恰好一次。

公开方法 `ApplyRemoteCommits` 的 nil 只表示取得持久回执；需要发布完成的调用方使用 `FlushRemoteTransaction`。正式 batch/memory 接线同步修改。无 wire、WAL、Mongo 格式与配置变更；不移除 MaxDeliver，不加生产故障开关。旧公开错误哨兵保留兼容调用方。

## 分批与验收

先在旧实现上实跑 E14（真实 Mongo 提交后阻塞测试子进程、SIGKILL、同 sid Assembly.Start、真实 Redis/NATS 只读收敛），再实施唯一入口，复跑 E14 与发布失败不挡后续投影、重复/取消/停止/回执重放等回归。测试替身必须遵守 Applied→Committed outbox 契约，不能用“保存即 Committed、outbox 永远空”的旧替身证明新设计。

之后运行目标 race、全仓 build/vet/test、正式生成消费者与本机集群故障矩阵；串行运行 1 小时正式 Remote 负载，1000 会话、10000 实体、2 实体×2 DAO、80 TPS，并核对一致性、错误、p99/max、内存/goroutine/积压。Sync 按已有 1000/10000/约50 可见、1%/5% 变化、20Hz 两种模式补验证。性能实验不与编译、索引重建或其他压测并跑。

历史清单按 A1/A2/A6/A7/A8/A14、N03/N04 具名边界逐项补证或修复；原始失败不删除。部署/物理断电与真实跨机语义明确不由本机验证证明，按本轮维护者范围不作为完成门槛。

回退为回退本次代码和对应调用方，持久格式不变，Applied outbox 仍由原路径恢复；不得删除 pending 记录或强置 Committed。

## 实施与结果

第一批已实施：唯一 RecoverOutbox 发布入口、异步唤醒/退避/停止排空、Committed 重放只确认；memory/strict/pipelined 成功回复仍等发布。正式 Mongo/Backend 增加游标分页，失败页不阻塞后续页（RR-20261008-43）；旧自定义存储须实现 IRemoteCommitOutboxPager 才具备跨失败页保证。持久文档格式不变。

历史 Layered 容量候选已复现并修复（RR-20261008-42）。本地 Mirror S2 新发现兴趣广播耗尽读预算，改有界异步广播并保留显式续租错误（RR-20261008-44），没有扩大断线收敛阈值。

macOS 全仓 build/vet/test 通过；Remote/Entity/Kit race ×3 通过。21格私有集群矩阵全部通过（分页与兴趣改动前）；最新 Mirror 全8场景通过（104.164s），最新 E14 精确 SIGKILL/restart 连续3次通过（2.955s）。E14 读取在原 100ms+1.5s 预算内等待负缓存失效，不假定 Start 返回瞬间只读方必已更新。

原始证据根：artifacts/perf/outbox-closure-20261008；矩阵在 artifacts/perf/remote/outbox-closure-20261008-r3。旧失败均保留。正式1h负载和历史逐项收敛仍进行中，不据上述局部验证称整轮已完成。
