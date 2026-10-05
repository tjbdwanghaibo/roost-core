# 2026-10-05 接续 review：N05 停机契约与 N06 Saga

从干净 main `7949da08` fetch / pull --ff-only 到 `af2f67fbb75e34110f59e4603e0046eedf7e358d`；上游只更新 App 演练残留清理文档，无新实现/Wanted/bugfix。三个 canonical skill 完整包与本机镜像一致，无需同步。不等待或查询 GitHub CI，不发版。

## 新问题与修复

| 入口 | 原反例 | 本批结论 |
| --- | --- | --- |
| Saga 三消费者 health | 原生 Nest 完成消费者缺失/关闭，正式 Kit 仍 ok；4红/6控制 | P2 [NC-37](../bugfix/RR-20261005-NC-37.md)已修；三消费者和重新启动恢复验证 |
| Resume→持久重读→派发/回执 | BSON 丢 incarnation，正向/补偿恢复复用 ID，再次 waiting；2红 | P2 [NC-38](../bugfix/RR-20261005-NC-38.md)已修；两次恢复、四种代际兼容通过 |
| N05 Mirror 在途停机 | 正式 JetStreamSyncBus/Replicator，旧 Store 已进入时退订、重新 Start、新旧回调交错 | 新控制通过；Stop不是handler drain；版本保护由可控 Store 提供，未冒认 Remote/真实broker证明 |
| N06 生命周期 | 第二/第三订阅失败清理后重试；第三消费者 drain 阻塞→取消→强停→再次Stop | 三新控制通过；不改现有启动/关闭策略 |

合计20新正式叶子：10健康、6持久代际（含4兼容）、4生命周期。修改前12叶子6fail/6控制，旧产品 overlay 再现同口径；最终20通过。不同运行数量不累加成覆盖率。[矩阵/原始红/复跑](../bugfix/evidence/noncore-bugfix-20261005-15/README.md)。

## 范围、设计与性能

生产只改 Saga Assembly 的健康检查、MongoStore 记录转换；必要中文注释同步维护。[机制与为什么漏检](IMPLEMENTATION-SAGA-CONSUMER-HEALTH-AND-DURABLE-RESUME.md)。既有 Resume/命令代际机制合理，关键问题在消费者新增后的故障观测链与持久表示不完整，不需要额外协议或抽象。

接续读取了 Engine 的 Resume/Complete/Compensate/processClaimed、Mongo Create/Get/Apply/回执与outbox、原生与普通完成消费者、正式 Kit 配置/装配/health；没有重新全审 Nest/Sync/DataEngine 或 Service 十域。健康检查增加一次非阻塞channel读取，代际只增加 BSON 字段，无新 RPC/锁/goroutine；没有性能基准或容量承诺。

SyncBus.Handler 明确错误只告警、不重试；JetStreamSyncBus fanout返回nil，和原生 Saga消费者错误进入NATS settle的策略不同。因此旧 repair miss/loadErr=nil 仍是观察，不假设它代表可靠业务 ACK。Replicator取消订阅无 drain承诺，在途清理需要依赖transport与Store准入，真实断线/ACK/重连仍未验。

## 本地验证与图谱

相关race498叶子/546通过事件，1 skip（RemoteAsyncFinalizerProjectionLoad）；根包14；全仓build、相关vet、glsvet通过。DAO/Entity正式CLI生成的periodic/on_change两消费叶子race通过。普通包矩阵不替代真实Mongo/NATS/HA或生成game-demo全链启动。[source/coverage](evidence/noncore-review-20261005-25/README.md)。

Tier2 Verify，项目roost-core，ready32285 nodes/209828 edges，generation仍 `2026-09-30T11:58:14Z`。先图谱定位、双向trace与精确片段，33材料路径coverage后用当前源码补证 metadata_changed/not_tracked；图谱同名receiver/跨包误边不用于因果判断。没有刷新/停止共享服务，无记录缺口不证明穷尽。33是证据路径，不是33产品文件已全面审完。

## 进度与下一入口

N05本机在途/退订契约补证后转入N06；N05仍场景部分完成，真实broker、跨节点L2删除水位、Mirror DTO、HA和长容量另留。N06第一批关闭两个P2并补生命周期控制，整个Saga/Service域仍部分完成，不计completed/15，不承诺全仓review日期。

下一优先新增Wanted/变更；再查 Saga 启动意图重投在Data/DeadlineAt变化后的身份判断、原生完成信封/收件箱与事务取消的组合，随后补Service增量/servicemetrics，按计划继续N07。该下一清单不是本轮确认缺陷；不以当前包绿继承未审业务承诺。另一线App/静态玩家绑定仅接手最新文档，没有将其外部演练或整合写成本轮独立验收。

## 交付同步：v1.20.0发布记录

提交前fetch到 `3f29a921749e23f4d8958863288eec155e9c86f0`，正常rebase整合4231e414/999dc672/3f29a921。发布记录、生成器Core下限v1.20.0、兼容lane/release manifest与pretag失败输出已包含；没有执行pretag、创建或推送tag。NC-37/38保留Unreleased，旧NC-31～36按上游记录已随v1.20.0发布，不能把本轮两项混入该tag。

bug/bugfix索引头部冲突手工保留双方内容；33材料路径LF哈希逐一相同，原20正式/498相关race场景证据未变化。补充两个发布相关材料路径的[coverage](evidence/noncore-review-20261005-25/delivery-coverage.json)及当前diff/源码；最新本地检查另列[退出码](../bugfix/evidence/noncore-bugfix-20261005-15/delivery-checks.json)，不把此前矩阵冒认成生成器全域重审。

canonical roost-bugfix只更新已发布版本标注，完整本机包已备份到本轮独占scratch后同步/逐文件核对一致；未写入macOS/Claude目录。该skill版本同步表仍有旧v1.18.0示例，实际manifest/compat已为v1.20.0，以当前源码为准。本轮不扩写发布流程或修改另一线规则源，不查询/等待GitHub CI。

追加生成器普通包回归首跑278叶子pass/2环境fail/10skip：[记录](../bugfix/evidence/noncore-bugfix-20261005-15/delivery-codegen-initial-summary.json)。两失败都是`exec: "sh": executable file not found in %PATH%`，不是本轮Saga或版本下限的行为红。[环境失败原文](../bugfix/evidence/noncore-bugfix-20261005-15/delivery-codegen-environment-failure.txt)保留；后续使用已有Git Bash/Go子进程PATH与独占TEMP重新运行，没有修改源码、测试门禁或安装依赖。

补齐环境后同一完整生成器包普通测试exit0，280叶子pass/0fail/10skip；codegen vet exit0。[最终清单](../bugfix/evidence/noncore-bugfix-20261005-15/delivery-codegen-final-summary.json) · [复跑环境/退出码](../bugfix/evidence/noncore-bugfix-20261005-15/delivery-codegen-retry.json)。十个具名skip仍受Windows/POSIX或外部环境限制，不计通过；这不是全codegen树620叶子的重跑。合并后最新build/根包14均exit0，源码与原Saga正式/race矩阵保持一致；不等待GitHub CI，本轮修复未发布。
