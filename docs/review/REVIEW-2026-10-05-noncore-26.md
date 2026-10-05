# 2026-10-05 N06 第二批：启动重投、完成路由与取消恢复

起点干净 main `47fca740`，fetch/pull --ff-only 到 `cb11be90`；新增另一 agent 的 codegen Ctrl-C 信号补修，Saga/Service 源码无本次拉取增量。没有新增 Wanted；三个 canonical skill 完整包与本机逐文件 LF 内容一致，无需同步。使用 roost-review/bugfix/coding，续接第25轮明确停点。

## 新问题与本轮增量

| 主链 | 证据与结论 |
| --- | --- |
| 启动→推进/全部完成→原 effect 重投 | 两后端四阶段8反例：同意图冲突、运行状态当新意图；P2 [NC-39](../bug/RR-20261005-NC-39.md)修复，原始摘要跨BSON/Replace保存 |
| Resume改/清deadline后的原启动 | 同一NC-39，不另编号；保留当前进度、原始identity与恢复代际 |
| 原生完成路由→正式Store | 3合法异键/缺目标反例发生状态和回执副作用；P2 [NC-40](../bug/RR-20261005-NC-40.md)修复，公开订阅回调拒绝后合法恢复/重复通过 |
| 兼容 | 8控制：规范化/自动ID、四种异意图、旧BSON初始/推进；旧已推进缺摘要明确冲突，不伪造迁移 |
| Complete事务取消与callback retry | 3写阶段取消→无半写→新ctx完成、outbox排空/摘要保留；1callback重跑控制 |
| 普通Mongo inbox | 业务已写后取消不发布事务，合法重试后重复不跑handler，1控制 |
| 原生消费者取消/屏障 | 普通取消保留lease、晚receipt恢复；明确WAL前fenced才交还，新token重试，2控制；receipt为手工注入，不冒认投影 |
| servicemetrics | core/kit seam与Recorder已读，复用既有计数/gauge/并发回归，补两处职责中文注释；不认为服务全部路径已上报 |

合计**26新正式叶子**：8启动+3路由+8兼容+5事务/普通inbox+2原生控制。原11反例全红；旧产品overlay复现相同11行为红；最终26绿。初次追加控制编译误用了不存在的测试替身方法名，已纠正为 `Attempts()` 并保留[原文](../bugfix/evidence/noncore-bugfix-20261005-16/fixture-build-failure.txt)，不计产品红。[红绿/矩阵/复跑](../bugfix/evidence/noncore-bugfix-20261005-16/README.md)。

## 设计、性能与使用边界

本机相关race586叶子、0fail/0skip；根包14；全仓build、相关vet、glsvet通过。正式DAO/Entity CLI生成periodic/on_change两消费叶子race通过；codegen/internal/roost普通包280叶子pass/10环境skip，vet通过。26新增、586矩阵与2生成消费不能累加成覆盖率，Unix tagged信号树用例未编入本机。

[实现学习与漏检复盘](IMPLEMENTATION-SAGA-START-IDENTITY-AND-CANCELLATION.md)。实际产品行为仅启动身份/持久映射、原生路由写前校验；Record添加optional字段，Store补保存契约。没有改核心Nest/DataEngine提交语义或增加抽象/无界缓存/重试。servicemetrics仅中文职责注释。无性能基准，不因有限新增hash成本宣称无吞吐影响。

NC-39旧已推进记录不能证明原始请求，重投明确 `ErrIdentityConflict`；运行仍沿原状态机，不自动回填/删记录/迁移。自定义Store和旧writer须完整保存新增字段，统一升级。NC-40不等于鉴权/签名，真实Subject与broker Term/重连另验。

## 上游整合与图谱证据

cb11be90的runCommandTree重发信号后等待默认动作，doctor分离期限接线；负对照与负载复现由作者在macOS运行。本机核对source/diff并跑普通codegen包；Unix tagged进程树用例在Windows不编入，因此未独立复做作者压力结论，不把合并等同完整验收。不等待或查询GitHub CI。

Tier2 Verify，roost-core ready32285 nodes/209828 edges，generation2026-09-30T11:58:14Z。有界Saga启动/完成/Reserve及servicemetrics查询相关分页均结束；初次file_pattern限制没有结果，不作不存在结论。双向trace/get_code_snippet与[33路径coverage](evidence/noncore-review-20261005-26/coverage.json)后读取当前源码补证metadata_changed/not_tracked；初始候选mongo_store_test.go不存在，改用实际正式Store回归文件，未列为已查路径。

旧图谱存在同名receiver/标准库误边；新增注释/方法导致get_code_snippet闭包旧行号读到邻近函数，`closedOperation`以当前源码精确范围校正，不能把该片段当函数证据。没有强制重建/删除锁/停止共享实例，无记录缺口不证明穷尽。[摘要/范围](evidence/noncore-review-20261005-26/README.md)。

## 进度与接力

### 最终整合与本地验证

交付时正常 rebase 到另一 agent 的 `47a9132c`（Nest U-0279），保留 CHANGELOG、排障索引和 bugfix 索引双方记录；T-220 留给上游，本轮改用 T-221/222。33 材料路径的 LF 摘要全部未变。追加三条 Nest 路径的[coverage](evidence/noncore-review-20261005-26/delivery-coverage.json)，旧图谱未包含新方法，由当前差异和源码补证。

合并后 Nest/Saga/Kit Saga 的 race 矩阵 **534 叶子、0 fail/0 skip**，根包14、全仓 build、相关 vet 和 glsvet 退出0，见[检查](../bugfix/evidence/noncore-bugfix-20261005-16/delivery-checks.json)和[逐叶清单](../bugfix/evidence/noncore-bugfix-20261005-16/delivery-results.json)。该矩阵与此前586使用不同包范围，不能相加或解释为覆盖率。上游生成工程交叉创建千轮/负载压力结论由作者记录，本轮没有独立重跑，不以正常整合替代完整验收。

N06仍场景部分完成，不计completed/15。Saga本轮指定启动/原生完成/本机取消组合已接续；Mongo未知提交、真实NATS ACK/Term/重连、跨协调器竞争、HA与长期容量未验收。旧37/38生产差异未变，相关回归在本轮包矩阵执行，不重复登记。

Service旧十域证据保留；相对两个10-01独立复审基线db4b7009，当前存在account建角恢复、chat分页、activity legacy opening与global静态绑定替代等增量，已经列出变更路径，但未本轮全审这些业务。下一先新增Wanted/差异，再审Service这些增量及指标实际落点、Saga跨协调器/receipt重投余项；具名缺口收口后转N07。Mirror DTO/外部N05留项不因Saga修复关闭；没有新的全仓完成日期承诺。
