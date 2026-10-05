# 2026-10-05 Roost 接续 review：N04 嵌套迁移与持续竞争

基线 main `c3aa0eddf67be234ba742188c53d18bbc1be9604`；开始 fetch / ff-only 已是最新，无新增代码修复待验收。共同 coding/bugfix/optimize skill 与仓库一致。本轮按既定停点补嵌套类型迁移和持续 CAS 消费，发现并修复一个 P2 [NC-32](../bug/RR-20261005-NC-32.md)，未发版。最后同步上游情况以交付备注为准。

## 缺陷与新增场景

NC-32：wire 转换在临时父对象上接好子通知，再复制父对象；普通装载值正确但下一次业务事务没有提交。正式九项及生成六项行为反例确认；修复递归绑定时机，兼容与重新生成要求见[修复文档](../bugfix/RR-20261005-NC-32.md)。本轮没有重开其他 agent 的 Nest/Sync/DataEngine 全专项，只读取此生成消费必需的核心链。

| 场景 | 本轮增量及结论 |
| --- | --- |
| DAO map/slice/pointer 中的第二层子对象 | 9 正式提交承诺红→绿；既有 44 正式运行叶子仍绿 |
| 指针/值父对象的 leaf/map/slice 子对象 | 6 正式 CLI 消费红→绿，真实 Nest 提交记录可达 |
| 字段类型随 schema 变化 | 两步迁移将深层 string score 转为 int64，再增量变更；指针、值、map、slice 的值与标签保留 |
| 坏迁移目标类型 / 不可转换源数值 | 两项提交前返回错误，Manager 不发布，schema1/version7 不变，系统提交0 |
| null 子项 | nil 指针与 map/slice 中 null 保留，迁移后可由新 Manager 无迁移器加载 |
| 正常迁移后的下一次业务事务 | 深层值字段中的 leaf 改99，经实际文件 WAL/Projector/MongoStore 落库；fresh Manager 重载99/version9，另一个父对象仍42 |
| 连续 CAS 淘汰 / 预算 | 两次迁移投影前用正式 MongoStore CAS 写竞争者；第三次视图仍旧 schema，返回 ErrMigrationConflict、未发布，竞争者102/version9保留 |
| 竞争结束后的恢复 | 原 Repository 第二次 Load 清掉失败 flight，从最新版本迁移到3/version10；历史被淘汰 WAL 经 Flush barrier 确认清零 |

消费者合计 **28 叶子通过**，其中17沿用上轮，**11本轮增量**，不能重复计入新覆盖。金样运行53 = 44原有 + 9新正式。原始命令/日志见[复跑证据](evidence/noncore-review-20261005-22/README.md)。生成消费者用实际文件 WAL 与正式 Projector/MongoStore，后端为框架 mongotest，未假称真实 Mongo。

## 图谱与证据边界

Tier 2 Verify，项目 roost-core，root D:/whb_s/cube-core，ready 32285 nodes / 209828 edges，generation `2026-09-30T11:58:14Z`。结构定位先 search_graph、trace_path、get_code_snippet，候选路径统一 check_index_coverage；相关路径无记录缺口但 metadata_changed，新回归 not_tracked，故以精确当前源码/生成物/执行为准。早期 broad testdata 搜索45/51只作候选，随后 exact hero 搜索完整分页；不作全仓穷尽声明。模板字符串和 receiver 链有索引盲区，不能把 trace 未列 Migrate 当成无调用。[coverage](evidence/noncore-review-20261005-22/coverage.json)、[摘要](evidence/noncore-review-20261005-22/source-hashes.csv)。

## 设计、性能与进度

生成恢复仍把数据与运行期接线分开，只在最终对象上建立父归属。这比放宽唯一父约束可解释，也避免把额外保存入口推给业务。迁移依旧逐 DAO 持久化而非全聚合原子事务；持续竞争使用现有预算与显式错误，没有无界自动重放。

新绑定是本地递归遍历，无新存储往返；深树/大容器的恢复 CPU 与分配需单独 benchmark，本轮 race 验证不能证明延迟指标。业务持久化链保留真实 WAL 消费测试，但 Mongo/HA/未知网络结果/长期容量未在本机验收。

N04 历史41/41候选源文累计已读，生成迁移的上述具名本机场景现已补齐，**N04整个功能域仍部分完成，不计 completed/15**。具名留项：真实 Mongo replica set 的迁移并发/未知提交、真实 Redis Cluster/弱网、部署故障矩阵与长期容量，以及大嵌套树绑定性能。既有外部17 skip 不关闭。下一转 N05 路由/mirror 的当前实现与既有方案增量，不继续重复复验已闭合的 N04 本机用例；若维护者提供 N04 修复/新 Wanted 再按新证据回查。静态 PlayerOwner 方案仍是另线提案，不误计实现完成。

本轮不新增自动任务、不发版，按本地编译与适用验证收尾后提交推送，不等待 GitHub CI。

## 交付前上游增量

再次fetch发现 `ed127736` / `0f554aed`，只新增/修订[App单实例锁方案](../feature/APP-SINGLETON-LOCK-2026-10-05.md)与[静态玩家绑定方案](../feature/PLAYEROWNER-STATIC-BINDING-2026-10-05.md)，没有Go实现、测试或Wanted更改。本轮整合这两个文档提交；N04已执行消费的Go源码身份不变，不重复跑已通过检查。

新方案已明确App集中持锁、RuntimeFailure触发Nest围栏，activity改用只读Live，且明示未实施/未演练。本轮只核对文档性质与本次修复范围，没有将其计成实现/修复/独立架构验收，也不把方案的4～5 agent日当作全仓review完成承诺。后续另线实施后需要按具名启动/失锁/停机/进程接手场景验收。

推送期间再次整合 `84512f8f`，仍仅修订App锁方案，加入全仓接管清单（global租约API删除、election/lock能力停发、etcd注册仅做发现）。这些是方案范围调整，未有Go/Wanted增量；本轮未实施或独立验收该接管方案，不能据此关闭现有实现的缺陷或扩大全仓已完成范围。
